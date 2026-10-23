package manifest

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

const (
	metaPayloadSize   = 56
	metaFormatVersion = uint16(1)
)

var metaMagic = [4]byte{'M', 'E', 'T', 'A'}

// FileID is the stable random identity of one database file.
type FileID [16]byte

// String returns the lowercase hexadecimal file identity.
func (id FileID) String() string { return hex.EncodeToString(id[:]) }

// Meta is the logical content shared by the two metapage candidates.
type Meta struct {
	FileID               FileID
	PageSize             uint32
	LatestTxnID          uint64
	LatestManifestPageID page.ID
	OldestRetainedTxnID  uint64
}

// Candidate is one validated metapage and its common-header generation.
type Candidate struct {
	Slot       page.ID
	Generation uint64
	Meta       Meta
}

// MetapageCorruptionError describes an invalid metapage payload.
type MetapageCorruptionError struct {
	Slot      page.ID
	Offset    int
	Invariant string
}

// Error implements error.
func (e *MetapageCorruptionError) Error() string {
	return fmt.Sprintf("metapage %d corruption at payload byte %d: %s", e.Slot, e.Offset, e.Invariant)
}

// Unwrap categorizes invalid persisted metapage bytes as corruption.
func (e *MetapageCorruptionError) Unwrap() error { return dberrors.ErrCorruption }

// EncodeMetapage encodes a complete page 0 or page 1 candidate.
func EncodeMetapage(slot page.ID, generation uint64, meta Meta) ([]byte, error) {
	if slot > 1 {
		return nil, dberrors.New(dberrors.CodeConstraint, "metapage.encode", fmt.Sprintf("slot %d is not page 0 or 1", slot))
	}
	if meta.PageSize != page.Size {
		return nil, dberrors.New(dberrors.CodeConstraint, "metapage.encode", fmt.Sprintf("page size %d is unsupported", meta.PageSize))
	}
	if zeroFileID(meta.FileID) {
		return nil, dberrors.New(dberrors.CodeConstraint, "metapage.encode", "file ID cannot be zero")
	}
	payload := make([]byte, metaPayloadSize)
	copy(payload[0:4], metaMagic[:])
	binary.LittleEndian.PutUint16(payload[4:6], metaFormatVersion)
	copy(payload[8:24], meta.FileID[:])
	binary.LittleEndian.PutUint32(payload[24:28], meta.PageSize)
	binary.LittleEndian.PutUint64(payload[32:40], meta.LatestTxnID)
	binary.LittleEndian.PutUint64(payload[40:48], uint64(meta.LatestManifestPageID))
	binary.LittleEndian.PutUint64(payload[48:56], meta.OldestRetainedTxnID)
	return page.Encode(page.Header{Type: page.TypeMeta, PageID: slot, Generation: generation}, payload)
}

// DecodeMetapage validates one complete metapage candidate at expectedSlot.
func DecodeMetapage(encoded []byte, expectedSlot page.ID) (Candidate, error) {
	decoded, err := page.Decode(encoded)
	if err != nil {
		return Candidate{}, err
	}
	corrupt := func(offset int, invariant string) error {
		return &MetapageCorruptionError{Slot: expectedSlot, Offset: offset, Invariant: invariant}
	}
	if expectedSlot > 1 {
		return Candidate{}, corrupt(0, "expected slot is not page 0 or 1")
	}
	if decoded.Header.PageID != expectedSlot {
		return Candidate{}, corrupt(0, fmt.Sprintf("page declares ID %d", decoded.Header.PageID))
	}
	if decoded.Header.Type != page.TypeMeta {
		return Candidate{}, corrupt(0, fmt.Sprintf("page type is %s, expected meta", decoded.Header.Type))
	}
	if len(decoded.Payload) != metaPayloadSize {
		return Candidate{}, corrupt(0, fmt.Sprintf("payload size is %d, expected %d", len(decoded.Payload), metaPayloadSize))
	}
	payload := decoded.Payload
	if string(payload[0:4]) != string(metaMagic[:]) {
		return Candidate{}, corrupt(0, "invalid metapage magic")
	}
	if version := binary.LittleEndian.Uint16(payload[4:6]); version != metaFormatVersion {
		return Candidate{}, corrupt(4, fmt.Sprintf("unsupported metapage version %d", version))
	}
	if binary.LittleEndian.Uint16(payload[6:8]) != 0 || binary.LittleEndian.Uint32(payload[28:32]) != 0 {
		return Candidate{}, corrupt(6, "reserved metapage bytes must be zero")
	}
	var fileID FileID
	copy(fileID[:], payload[8:24])
	if zeroFileID(fileID) {
		return Candidate{}, corrupt(8, "file ID cannot be zero")
	}
	pageSize := binary.LittleEndian.Uint32(payload[24:28])
	if pageSize != page.Size {
		return Candidate{}, corrupt(24, fmt.Sprintf("unsupported page size %d", pageSize))
	}
	meta := Meta{
		FileID:               fileID,
		PageSize:             pageSize,
		LatestTxnID:          binary.LittleEndian.Uint64(payload[32:40]),
		LatestManifestPageID: page.ID(binary.LittleEndian.Uint64(payload[40:48])),
		OldestRetainedTxnID:  binary.LittleEndian.Uint64(payload[48:56]),
	}
	if meta.LatestTxnID == 0 && meta.LatestManifestPageID != 0 {
		return Candidate{}, corrupt(40, "empty transaction state references a manifest")
	}
	if meta.OldestRetainedTxnID > meta.LatestTxnID {
		return Candidate{}, corrupt(48, "oldest retained transaction exceeds latest transaction")
	}
	return Candidate{Slot: expectedSlot, Generation: decoded.Header.Generation, Meta: meta}, nil
}

func zeroFileID(id FileID) bool {
	return id == FileID{}
}
