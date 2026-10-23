package page

import (
	"encoding/binary"
	"fmt"
	"hash/crc64"

	"yodadb/dberrors"
)

const (
	// Size is the only page size supported by format version 1.
	Size = 4096
	// HeaderSize is the encoded common page-header size.
	HeaderSize = 36
	// MaxPayloadSize is the maximum version-1 common page payload.
	MaxPayloadSize = Size - HeaderSize
	// FormatVersion is the current common page format version.
	FormatVersion uint16 = 1
)

var (
	pageMagic     = [4]byte{'Y', 'O', 'D', 'A'}
	checksumTable = crc64.MakeTable(crc64.ECMA)
	zeroChecksum  [8]byte
)

// ID identifies a fixed-size page by its zero-based position in the file.
type ID uint64

// Type identifies the logical contents of a page.
type Type uint8

const (
	// TypeMeta identifies one of the two mutable publication metapages.
	TypeMeta Type = iota + 1
	// TypeManifest identifies a commit manifest.
	TypeManifest
	// TypeBTreeInternal identifies an internal B+ tree page.
	TypeBTreeInternal
	// TypeBTreeLeaf identifies a leaf B+ tree page.
	TypeBTreeLeaf
	// TypeCatalog identifies a catalog data page.
	TypeCatalog
)

// String returns a diagnostic page-type name.
func (t Type) String() string {
	switch t {
	case TypeMeta:
		return "meta"
	case TypeManifest:
		return "manifest"
	case TypeBTreeInternal:
		return "btree-internal"
	case TypeBTreeLeaf:
		return "btree-leaf"
	case TypeCatalog:
		return "catalog"
	default:
		return fmt.Sprintf("page-type(%d)", t)
	}
}

// Header is the decoded common page header.
type Header struct {
	Type       Type
	Flags      uint8
	PageID     ID
	Generation uint64
	Checksum   uint64
}

// Page is a validated page with a defensively copied payload.
type Page struct {
	Header  Header
	Payload []byte
}

// CorruptionError describes a failed page invariant.
type CorruptionError struct {
	PageID    ID
	PageType  Type
	Offset    int
	Invariant string
}

// Error implements error.
func (e *CorruptionError) Error() string {
	return fmt.Sprintf("page %d (%s) corruption at byte %d: %s", e.PageID, e.PageType, e.Offset, e.Invariant)
}

// Unwrap categorizes invalid persisted page bytes as corruption.
func (e *CorruptionError) Unwrap() error { return dberrors.ErrCorruption }

// Encode returns one canonical 4096-byte page.
func Encode(header Header, payload []byte) ([]byte, error) {
	if !validType(header.Type) {
		return nil, dberrors.New(dberrors.CodeConstraint, "page.encode", fmt.Sprintf("invalid page type %d", header.Type))
	}
	if header.Flags != 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "page.encode", "format version 1 page flags must be zero")
	}
	if len(payload) > MaxPayloadSize {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "page.encode", fmt.Sprintf("payload is %d bytes, maximum is %d", len(payload), MaxPayloadSize))
	}

	encoded := make([]byte, Size)
	copy(encoded[0:4], pageMagic[:])
	binary.LittleEndian.PutUint16(encoded[4:6], FormatVersion)
	encoded[6] = byte(header.Type)
	encoded[7] = header.Flags
	binary.LittleEndian.PutUint64(encoded[8:16], uint64(header.PageID))
	binary.LittleEndian.PutUint64(encoded[16:24], header.Generation)
	binary.LittleEndian.PutUint16(encoded[24:26], uint16(len(payload)))
	copy(encoded[HeaderSize:HeaderSize+len(payload)], payload)
	checksum := checksumBytes(encoded, len(payload))
	binary.LittleEndian.PutUint64(encoded[28:36], checksum)
	return encoded, nil
}

// Decode validates and decodes exactly one canonical version-1 page.
func Decode(encoded []byte) (Page, error) {
	if len(encoded) != Size {
		return Page{}, &CorruptionError{Offset: 0, Invariant: fmt.Sprintf("page size is %d, expected %d", len(encoded), Size)}
	}
	pageID := ID(binary.LittleEndian.Uint64(encoded[8:16]))
	pageType := Type(encoded[6])
	corrupt := func(offset int, invariant string) error {
		return &CorruptionError{PageID: pageID, PageType: pageType, Offset: offset, Invariant: invariant}
	}
	if string(encoded[0:4]) != string(pageMagic[:]) {
		return Page{}, corrupt(0, "invalid page magic")
	}
	if version := binary.LittleEndian.Uint16(encoded[4:6]); version != FormatVersion {
		return Page{}, corrupt(4, fmt.Sprintf("unsupported format version %d", version))
	}
	if !validType(pageType) {
		return Page{}, corrupt(6, fmt.Sprintf("unknown page type %d", pageType))
	}
	if encoded[7] != 0 {
		return Page{}, corrupt(7, "format version 1 page flags must be zero")
	}
	payloadLength := int(binary.LittleEndian.Uint16(encoded[24:26]))
	if payloadLength > MaxPayloadSize {
		return Page{}, corrupt(24, "payload length exceeds page capacity")
	}
	if binary.LittleEndian.Uint16(encoded[26:28]) != 0 {
		return Page{}, corrupt(26, "reserved header bytes must be zero")
	}
	storedChecksum := binary.LittleEndian.Uint64(encoded[28:36])
	calculatedChecksum := checksumBytes(encoded, payloadLength)
	if storedChecksum != calculatedChecksum {
		return Page{}, corrupt(28, fmt.Sprintf("checksum mismatch: stored %016x, calculated %016x", storedChecksum, calculatedChecksum))
	}
	for offset, value := range encoded[HeaderSize+payloadLength:] {
		if value != 0 {
			return Page{}, corrupt(HeaderSize+payloadLength+offset, "unused page bytes must be zero")
		}
	}
	payload := append([]byte(nil), encoded[HeaderSize:HeaderSize+payloadLength]...)
	return Page{
		Header: Header{
			Type:       pageType,
			Flags:      encoded[7],
			PageID:     pageID,
			Generation: binary.LittleEndian.Uint64(encoded[16:24]),
			Checksum:   storedChecksum,
		},
		Payload: payload,
	}, nil
}

func checksumBytes(encoded []byte, payloadLength int) uint64 {
	checksum := crc64.Update(0, checksumTable, encoded[:28])
	checksum = crc64.Update(checksum, checksumTable, zeroChecksum[:])
	return crc64.Update(checksum, checksumTable, encoded[HeaderSize:HeaderSize+payloadLength])
}

func validType(pageType Type) bool {
	return pageType >= TypeMeta && pageType <= TypeCatalog
}
