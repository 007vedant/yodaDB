package types

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"yodadb/dberrors"
)

const tupleFormatVersion byte = 1

// ErrValueTooLarge identifies a value or tuple rejected by configured limits.
var ErrValueTooLarge = errors.New("value too large")

// TupleLimits bounds tuple decoder allocations and encoded values.
type TupleLimits struct {
	MaxColumns    int
	MaxValueBytes int
	MaxTupleBytes int
}

// DefaultTupleLimits returns the version-1 default resource limits.
func DefaultTupleLimits() TupleLimits {
	return TupleLimits{MaxColumns: 1024, MaxValueBytes: 1 << 20, MaxTupleBytes: 4 << 20}
}

// TupleCorruptionError describes a malformed persisted tuple. Column is -1
// when the failure is in the tuple-level header.
type TupleCorruptionError struct {
	Offset    int
	Column    int
	Invariant string
}

// Error implements error.
func (e *TupleCorruptionError) Error() string {
	if e.Column >= 0 {
		return fmt.Sprintf("tuple corruption at byte %d, column %d: %s", e.Offset, e.Column, e.Invariant)
	}
	return fmt.Sprintf("tuple corruption at byte %d: %s", e.Offset, e.Invariant)
}

// Unwrap categorizes malformed tuple bytes as corruption.
func (e *TupleCorruptionError) Unwrap() error { return dberrors.ErrCorruption }

// EncodeTuple encodes a self-describing version-1 row.
func EncodeTuple(schemaVersion uint64, row Row, limits TupleLimits) ([]byte, error) {
	limits = normalizeTupleLimits(limits)
	if len(row) > limits.MaxColumns {
		return nil, tupleLimitError("tuple has too many columns")
	}

	nullBitmap := make([]byte, (len(row)+7)/8)
	tags := make([]byte, len(row))
	ends := make([]uint64, len(row))
	payload := make([]byte, 0)
	for column, value := range row {
		tags[column] = byte(value.Kind())
		if value.IsNull() {
			nullBitmap[column/8] |= 1 << uint(column%8)
			ends[column] = uint64(len(payload))
			continue
		}
		switch value.Kind() {
		case BoolKind:
			payloadValue, _ := value.BoolValue()
			if payloadValue {
				payload = append(payload, 1)
			} else {
				payload = append(payload, 0)
			}
		case Int64Kind:
			payloadValue, _ := value.Int64Value()
			payload = binary.LittleEndian.AppendUint64(payload, uint64(payloadValue))
		case Float64Kind:
			payloadValue, _ := value.Float64Value()
			payload = binary.LittleEndian.AppendUint64(payload, math.Float64bits(payloadValue))
		case TextKind:
			payloadValue, _ := value.TextValue()
			if !utf8.ValidString(payloadValue) {
				return nil, dberrors.New(dberrors.CodeType, "tuple.encode", fmt.Sprintf("column %d TEXT is not valid UTF-8", column))
			}
			if len(payloadValue) > limits.MaxValueBytes {
				return nil, tupleLimitError(fmt.Sprintf("column %d TEXT exceeds value limit", column))
			}
			payload = append(payload, payloadValue...)
		case BytesKind:
			payloadValue, _ := value.BytesValue()
			if len(payloadValue) > limits.MaxValueBytes {
				return nil, tupleLimitError(fmt.Sprintf("column %d BYTES exceeds value limit", column))
			}
			payload = append(payload, payloadValue...)
		default:
			return nil, dberrors.New(dberrors.CodeType, "tuple.encode", fmt.Sprintf("column %d has invalid kind %s", column, value.Kind()))
		}
		ends[column] = uint64(len(payload))
		if len(payload) > limits.MaxTupleBytes {
			return nil, tupleLimitError("tuple payload exceeds tuple limit")
		}
	}

	encoded := make([]byte, 0, 1+binary.MaxVarintLen64*2+len(nullBitmap)+len(tags)+len(ends)*binary.MaxVarintLen64+len(payload))
	encoded = append(encoded, tupleFormatVersion)
	encoded = binary.AppendUvarint(encoded, schemaVersion)
	encoded = binary.AppendUvarint(encoded, uint64(len(row)))
	encoded = append(encoded, nullBitmap...)
	encoded = append(encoded, tags...)
	for _, end := range ends {
		encoded = binary.AppendUvarint(encoded, end)
	}
	encoded = append(encoded, payload...)
	if len(encoded) > limits.MaxTupleBytes {
		return nil, tupleLimitError("encoded tuple exceeds tuple limit")
	}
	return encoded, nil
}

// DecodeTuple decodes a self-describing version-1 row. It never retains data.
func DecodeTuple(data []byte, limits TupleLimits) (uint64, Row, error) {
	limits = normalizeTupleLimits(limits)
	if len(data) > limits.MaxTupleBytes {
		return 0, nil, tupleLimitError("encoded tuple exceeds tuple limit")
	}
	cursor := tupleCursor{data: data}
	version, err := cursor.byte(-1, "missing tuple format version")
	if err != nil {
		return 0, nil, err
	}
	if version != tupleFormatVersion {
		return 0, nil, cursor.corrupt(-1, fmt.Sprintf("unsupported tuple format version %d", version))
	}
	schemaVersion, err := cursor.uvarint(-1, "invalid schema version")
	if err != nil {
		return 0, nil, err
	}
	columnCount64, err := cursor.uvarint(-1, "invalid column count")
	if err != nil {
		return 0, nil, err
	}
	if columnCount64 > uint64(limits.MaxColumns) {
		return 0, nil, tupleLimitError("tuple has too many columns")
	}
	columnCount := int(columnCount64)
	bitmapLength := (columnCount + 7) / 8
	nullBitmap, err := cursor.bytes(bitmapLength, -1, "truncated null bitmap")
	if err != nil {
		return 0, nil, err
	}
	if columnCount%8 != 0 && bitmapLength > 0 {
		usedBits := uint(columnCount % 8)
		if nullBitmap[bitmapLength-1]&(^byte((1<<usedBits)-1)) != 0 {
			return 0, nil, cursor.corrupt(-1, "unused null-bitmap bits must be zero")
		}
	}
	tags, err := cursor.bytes(columnCount, -1, "truncated type tags")
	if err != nil {
		return 0, nil, err
	}
	ends := make([]uint64, columnCount)
	var previous uint64
	for column := range columnCount {
		end, err := cursor.uvarint(column, "invalid payload end offset")
		if err != nil {
			return 0, nil, err
		}
		if end < previous {
			return 0, nil, cursor.corrupt(column, "payload end offsets are not monotonic")
		}
		ends[column] = end
		previous = end
	}
	payload := data[cursor.offset:]
	if previous != uint64(len(payload)) {
		return 0, nil, cursor.corrupt(-1, "final payload end offset does not match payload length")
	}

	row := make(Row, columnCount)
	previous = 0
	for column := range columnCount {
		end := ends[column]
		valueBytes := payload[previous:end]
		isNull := nullBitmap[column/8]&(1<<uint(column%8)) != 0
		kind := Kind(tags[column])
		if isNull {
			if kind != NullKind || len(valueBytes) != 0 {
				return 0, nil, cursor.corrupt(column, "NULL bitmap, type tag, and payload disagree")
			}
			row[column] = Null()
			previous = end
			continue
		}
		if kind == NullKind {
			return 0, nil, cursor.corrupt(column, "non-NULL column uses NULL type tag")
		}
		value, err := decodeTupleValue(kind, valueBytes, limits, cursor.offset+int(previous), column)
		if err != nil {
			return 0, nil, err
		}
		row[column] = value
		previous = end
	}
	return schemaVersion, row, nil
}

func decodeTupleValue(kind Kind, data []byte, limits TupleLimits, offset, column int) (Value, error) {
	corrupt := func(invariant string) error {
		return &TupleCorruptionError{Offset: offset, Column: column, Invariant: invariant}
	}
	switch kind {
	case BoolKind:
		if len(data) != 1 || data[0] > 1 {
			return Value{}, corrupt("BOOL payload must be one byte containing 0 or 1")
		}
		return Bool(data[0] == 1), nil
	case Int64Kind:
		if len(data) != 8 {
			return Value{}, corrupt("INT64 payload must be eight bytes")
		}
		return Int64(int64(binary.LittleEndian.Uint64(data))), nil
	case Float64Kind:
		if len(data) != 8 {
			return Value{}, corrupt("FLOAT64 payload must be eight bytes")
		}
		return Float64(math.Float64frombits(binary.LittleEndian.Uint64(data))), nil
	case TextKind:
		if len(data) > limits.MaxValueBytes {
			return Value{}, tupleLimitError(fmt.Sprintf("column %d TEXT exceeds value limit", column))
		}
		if !utf8.Valid(data) {
			return Value{}, corrupt("TEXT payload is not valid UTF-8")
		}
		return Text(string(data)), nil
	case BytesKind:
		if len(data) > limits.MaxValueBytes {
			return Value{}, tupleLimitError(fmt.Sprintf("column %d BYTES exceeds value limit", column))
		}
		return Bytes(data), nil
	default:
		return Value{}, corrupt(fmt.Sprintf("unknown type tag %d", kind))
	}
}

type tupleCursor struct {
	data   []byte
	offset int
}

func (c *tupleCursor) byte(column int, invariant string) (byte, error) {
	if c.offset >= len(c.data) {
		return 0, c.corrupt(column, invariant)
	}
	value := c.data[c.offset]
	c.offset++
	return value, nil
}

func (c *tupleCursor) bytes(length, column int, invariant string) ([]byte, error) {
	if length < 0 || c.offset > len(c.data)-length {
		return nil, c.corrupt(column, invariant)
	}
	value := c.data[c.offset : c.offset+length]
	c.offset += length
	return value, nil
}

func (c *tupleCursor) uvarint(column int, invariant string) (uint64, error) {
	start := c.offset
	value, count := binary.Uvarint(c.data[c.offset:])
	if count == 0 {
		return 0, c.corruptAt(start, column, invariant+": truncated varint")
	}
	if count < 0 {
		return 0, c.corruptAt(start, column, invariant+": overflowing varint")
	}
	var canonical [binary.MaxVarintLen64]byte
	canonicalLength := binary.PutUvarint(canonical[:], value)
	if canonicalLength != count {
		return 0, c.corruptAt(start, column, invariant+": non-canonical varint")
	}
	for index := range count {
		if c.data[start+index] != canonical[index] {
			return 0, c.corruptAt(start, column, invariant+": non-canonical varint")
		}
	}
	c.offset += count
	return value, nil
}

func (c *tupleCursor) corrupt(column int, invariant string) error {
	return c.corruptAt(c.offset, column, invariant)
}

func (c *tupleCursor) corruptAt(offset, column int, invariant string) error {
	return &TupleCorruptionError{Offset: offset, Column: column, Invariant: invariant}
}

func normalizeTupleLimits(limits TupleLimits) TupleLimits {
	defaults := DefaultTupleLimits()
	if limits.MaxColumns <= 0 {
		limits.MaxColumns = defaults.MaxColumns
	}
	if limits.MaxValueBytes <= 0 {
		limits.MaxValueBytes = defaults.MaxValueBytes
	}
	if limits.MaxTupleBytes <= 0 {
		limits.MaxTupleBytes = defaults.MaxTupleBytes
	}
	return limits
}

func tupleLimitError(message string) error {
	return dberrors.Wrap(ErrValueTooLarge, dberrors.CodeResourceLimit, "tuple.limit", message)
}
