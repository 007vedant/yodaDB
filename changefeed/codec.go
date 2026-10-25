package changefeed

import (
	"encoding/binary"
	"fmt"
	"math"

	"yodadb/dberrors"
	"yodadb/types"
)

const (
	recordFormatVersion = uint16(1)
	recordHeaderSize    = 32
	maxRecordBytes      = 16 << 20
	flagBefore          = byte(1 << 0)
	flagAfter           = byte(1 << 1)
)

var recordMagic = [4]byte{'C', 'H', 'N', 'G'}

// CorruptionError describes a malformed persisted logical change.
type CorruptionError struct {
	Offset    int
	Invariant string
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("change corruption at byte %d: %s", e.Offset, e.Invariant)
}

func (e *CorruptionError) Unwrap() error { return dberrors.ErrCorruption }

// EncodeRecord encodes one version-1 logical change value.
func EncodeRecord(change Change) ([]byte, error) {
	if change.TxnID == 0 || change.TableID == 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "changefeed.encode", "transaction and table IDs must be nonzero")
	}
	if err := validateShape(change, false); err != nil {
		return nil, err
	}
	before, err := encodeImage(change.Before)
	if err != nil {
		return nil, err
	}
	after, err := encodeImage(change.After)
	if err != nil {
		return nil, err
	}
	lengths := []int{len(change.BeforeKey), len(change.AfterKey), len(before), len(after)}
	total := recordHeaderSize
	for _, length := range lengths {
		if uint64(length) > math.MaxUint32 || total > maxRecordBytes-length {
			return nil, dberrors.New(dberrors.CodeResourceLimit, "changefeed.encode", "change record exceeds size limit")
		}
		total += length
	}
	encoded := make([]byte, total)
	copy(encoded[:4], recordMagic[:])
	binary.LittleEndian.PutUint16(encoded[4:6], recordFormatVersion)
	encoded[6] = byte(change.Kind)
	if change.Before != nil {
		encoded[7] |= flagBefore
	}
	if change.After != nil {
		encoded[7] |= flagAfter
	}
	binary.LittleEndian.PutUint64(encoded[8:16], change.TableID)
	binary.LittleEndian.PutUint32(encoded[16:20], uint32(lengths[0]))
	binary.LittleEndian.PutUint32(encoded[20:24], uint32(lengths[1]))
	binary.LittleEndian.PutUint32(encoded[24:28], uint32(lengths[2]))
	binary.LittleEndian.PutUint32(encoded[28:32], uint32(lengths[3]))
	offset := recordHeaderSize
	for _, part := range [][]byte{change.BeforeKey, change.AfterKey, before, after} {
		copy(encoded[offset:], part)
		offset += len(part)
	}
	return encoded, nil
}

// DecodeRecord decodes one reassembled version-1 logical change value.
func DecodeRecord(txnID uint64, sequence uint32, encoded []byte) (Change, error) {
	corrupt := func(offset int, invariant string) error {
		return &CorruptionError{Offset: offset, Invariant: invariant}
	}
	if txnID == 0 {
		return Change{}, corrupt(0, "transaction ID must be nonzero")
	}
	if len(encoded) < recordHeaderSize {
		return Change{}, corrupt(0, "record is shorter than its header")
	}
	if len(encoded) > maxRecordBytes {
		return Change{}, corrupt(0, "record exceeds size limit")
	}
	if string(encoded[:4]) != string(recordMagic[:]) {
		return Change{}, corrupt(0, "invalid record magic")
	}
	if version := binary.LittleEndian.Uint16(encoded[4:6]); version != recordFormatVersion {
		return Change{}, corrupt(4, fmt.Sprintf("unsupported record version %d", version))
	}
	change := Change{
		TxnID:    txnID,
		Sequence: sequence,
		Kind:     Kind(encoded[6]),
		TableID:  binary.LittleEndian.Uint64(encoded[8:16]),
	}
	if change.TableID == 0 {
		return Change{}, corrupt(8, "table ID must be nonzero")
	}
	lengths := [4]uint32{
		binary.LittleEndian.Uint32(encoded[16:20]), binary.LittleEndian.Uint32(encoded[20:24]),
		binary.LittleEndian.Uint32(encoded[24:28]), binary.LittleEndian.Uint32(encoded[28:32]),
	}
	offset := recordHeaderSize
	parts := make([][]byte, len(lengths))
	for index, length := range lengths {
		if uint64(length) > uint64(len(encoded)-offset) {
			return Change{}, corrupt(16+index*4, "field lengths exceed record")
		}
		parts[index] = encoded[offset : offset+int(length)]
		offset += int(length)
	}
	if offset != len(encoded) {
		return Change{}, corrupt(offset, "record contains trailing bytes")
	}
	change.BeforeKey = append([]byte(nil), parts[0]...)
	change.AfterKey = append([]byte(nil), parts[1]...)
	flags := encoded[7]
	if flags & ^byte(flagBefore|flagAfter) != 0 {
		return Change{}, corrupt(7, "record contains unknown image flags")
	}
	if flags&flagBefore != 0 {
		image, err := decodeImage(parts[2], 24)
		if err != nil {
			return Change{}, err
		}
		change.Before = image
	} else if len(parts[2]) != 0 {
		return Change{}, corrupt(24, "before bytes exist without flag")
	}
	if flags&flagAfter != 0 {
		image, err := decodeImage(parts[3], 28)
		if err != nil {
			return Change{}, err
		}
		change.After = image
	} else if len(parts[3]) != 0 {
		return Change{}, corrupt(28, "after bytes exist without flag")
	}
	if err := validateShape(change, true); err != nil {
		return Change{}, corrupt(6, err.Error())
	}
	return cloneChange(change), nil
}

func encodeImage(image *Image) ([]byte, error) {
	if image == nil {
		return nil, nil
	}
	if image.SchemaVersion == 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "changefeed.encode", "image schema version must be nonzero")
	}
	return types.EncodeTuple(image.SchemaVersion, image.Row, types.TupleLimits{})
}

func decodeImage(encoded []byte, offset int) (*Image, error) {
	if len(encoded) == 0 {
		return nil, &CorruptionError{Offset: offset, Invariant: "flagged image is empty"}
	}
	version, row, err := types.DecodeTuple(encoded, types.TupleLimits{})
	if err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeCorruption, "changefeed.decode", fmt.Sprintf("invalid image at record byte %d", offset))
	}
	if version == 0 {
		return nil, &CorruptionError{Offset: offset, Invariant: "image schema version must be nonzero"}
	}
	return &Image{SchemaVersion: version, Row: row}, nil
}

func validateShape(change Change, decoded bool) error {
	invalid := func(message string) error {
		code := dberrors.CodeConstraint
		operation := "changefeed.encode"
		if decoded {
			code = dberrors.CodeCorruption
			operation = "changefeed.decode"
		}
		return dberrors.New(code, operation, message)
	}
	switch change.Kind {
	case Insert:
		if len(change.BeforeKey) != 0 || change.Before != nil || len(change.AfterKey) == 0 || change.After == nil {
			return invalid("insert requires only an after key and image")
		}
	case Update:
		if len(change.BeforeKey) == 0 || change.Before == nil || len(change.AfterKey) == 0 || change.After == nil {
			return invalid("update requires before and after keys and images")
		}
	case Delete:
		if len(change.BeforeKey) == 0 || change.Before == nil || len(change.AfterKey) != 0 || change.After != nil {
			return invalid("delete requires only a before key and image")
		}
	default:
		return invalid(fmt.Sprintf("unknown change kind %d", change.Kind))
	}
	return nil
}
