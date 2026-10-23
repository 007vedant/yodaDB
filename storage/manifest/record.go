package manifest

import (
	"encoding/binary"
	"fmt"
	"sort"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

const (
	manifestFormatVersion = uint16(1)
	manifestHeaderSize    = 64
	manifestRootEntrySize = 16
)

var manifestMagic = [4]byte{'M', 'N', 'F', 'T'}

// Manifest is one immutable committed root set. Map keys are stable catalog
// object IDs and map values are B+ tree root page IDs; zero denotes an empty
// tree.
type Manifest struct {
	TxnID               uint64
	ParentTxnID         uint64
	CommitUnixNano      int64
	CatalogRoot         page.ID
	OldestRetainedTxnID uint64
	ChangeRoot          page.ID
	TableRoots          map[uint64]page.ID
	IndexRoots          map[uint64]page.ID
}

// ManifestCorruptionError describes malformed persisted manifest bytes.
type ManifestCorruptionError struct {
	Offset    int
	Invariant string
}

// Error implements error.
func (e *ManifestCorruptionError) Error() string {
	return fmt.Sprintf("manifest corruption at payload byte %d: %s", e.Offset, e.Invariant)
}

// Unwrap categorizes malformed manifests as corruption.
func (e *ManifestCorruptionError) Unwrap() error { return dberrors.ErrCorruption }

// EncodeManifest encodes a canonical version-1 manifest payload.
func EncodeManifest(record Manifest) ([]byte, error) {
	if record.TxnID == 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "manifest.encode", "transaction ID must be nonzero")
	}
	if record.ParentTxnID >= record.TxnID && record.ParentTxnID != 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "manifest.encode", "parent transaction must precede transaction")
	}
	if record.OldestRetainedTxnID > record.TxnID {
		return nil, dberrors.New(dberrors.CodeConstraint, "manifest.encode", "oldest retained transaction exceeds transaction")
	}
	if err := validateOptionalRoot("catalog", record.CatalogRoot); err != nil {
		return nil, err
	}
	if err := validateOptionalRoot("change", record.ChangeRoot); err != nil {
		return nil, err
	}
	if len(record.TableRoots) > int(^uint16(0)) || len(record.IndexRoots) > int(^uint16(0)) {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "manifest.encode", "root map contains too many entries")
	}
	size := manifestHeaderSize + (len(record.TableRoots)+len(record.IndexRoots))*manifestRootEntrySize
	if size > page.MaxPayloadSize {
		return nil, dberrors.New(dberrors.CodeResourceLimit, "manifest.encode", fmt.Sprintf("manifest requires %d-byte payload, maximum is %d", size, page.MaxPayloadSize))
	}

	tableIDs, err := sortedRootIDs("table", record.TableRoots)
	if err != nil {
		return nil, err
	}
	indexIDs, err := sortedRootIDs("index", record.IndexRoots)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, size)
	copy(payload[0:4], manifestMagic[:])
	binary.LittleEndian.PutUint16(payload[4:6], manifestFormatVersion)
	binary.LittleEndian.PutUint64(payload[8:16], record.TxnID)
	binary.LittleEndian.PutUint64(payload[16:24], record.ParentTxnID)
	binary.LittleEndian.PutUint64(payload[24:32], uint64(record.CommitUnixNano))
	binary.LittleEndian.PutUint64(payload[32:40], uint64(record.CatalogRoot))
	binary.LittleEndian.PutUint64(payload[40:48], record.OldestRetainedTxnID)
	binary.LittleEndian.PutUint16(payload[48:50], uint16(len(tableIDs)))
	binary.LittleEndian.PutUint16(payload[50:52], uint16(len(indexIDs)))
	binary.LittleEndian.PutUint64(payload[56:64], uint64(record.ChangeRoot))
	offset := manifestHeaderSize
	for _, id := range tableIDs {
		binary.LittleEndian.PutUint64(payload[offset:offset+8], id)
		binary.LittleEndian.PutUint64(payload[offset+8:offset+16], uint64(record.TableRoots[id]))
		offset += manifestRootEntrySize
	}
	for _, id := range indexIDs {
		binary.LittleEndian.PutUint64(payload[offset:offset+8], id)
		binary.LittleEndian.PutUint64(payload[offset+8:offset+16], uint64(record.IndexRoots[id]))
		offset += manifestRootEntrySize
	}
	return payload, nil
}

// DecodeManifest validates and decodes a canonical version-1 manifest payload.
func DecodeManifest(payload []byte) (Manifest, error) {
	corrupt := func(offset int, invariant string) error {
		return &ManifestCorruptionError{Offset: offset, Invariant: invariant}
	}
	if len(payload) < manifestHeaderSize {
		return Manifest{}, corrupt(0, "payload is shorter than manifest header")
	}
	if string(payload[0:4]) != string(manifestMagic[:]) {
		return Manifest{}, corrupt(0, "invalid manifest magic")
	}
	if version := binary.LittleEndian.Uint16(payload[4:6]); version != manifestFormatVersion {
		return Manifest{}, corrupt(4, fmt.Sprintf("unsupported manifest version %d", version))
	}
	if binary.LittleEndian.Uint16(payload[6:8]) != 0 || binary.LittleEndian.Uint32(payload[52:56]) != 0 {
		return Manifest{}, corrupt(6, "reserved manifest bytes must be zero")
	}
	tableCount := int(binary.LittleEndian.Uint16(payload[48:50]))
	indexCount := int(binary.LittleEndian.Uint16(payload[50:52]))
	expected := manifestHeaderSize + (tableCount+indexCount)*manifestRootEntrySize
	if expected != len(payload) {
		return Manifest{}, corrupt(48, fmt.Sprintf("root counts require %d-byte payload, got %d", expected, len(payload)))
	}
	record := Manifest{
		TxnID:               binary.LittleEndian.Uint64(payload[8:16]),
		ParentTxnID:         binary.LittleEndian.Uint64(payload[16:24]),
		CommitUnixNano:      int64(binary.LittleEndian.Uint64(payload[24:32])),
		CatalogRoot:         page.ID(binary.LittleEndian.Uint64(payload[32:40])),
		OldestRetainedTxnID: binary.LittleEndian.Uint64(payload[40:48]),
		ChangeRoot:          page.ID(binary.LittleEndian.Uint64(payload[56:64])),
		TableRoots:          make(map[uint64]page.ID, tableCount),
		IndexRoots:          make(map[uint64]page.ID, indexCount),
	}
	if record.TxnID == 0 {
		return Manifest{}, corrupt(8, "transaction ID must be nonzero")
	}
	if record.ParentTxnID >= record.TxnID && record.ParentTxnID != 0 {
		return Manifest{}, corrupt(16, "parent transaction does not precede transaction")
	}
	if record.OldestRetainedTxnID > record.TxnID {
		return Manifest{}, corrupt(40, "oldest retained transaction exceeds transaction")
	}
	if invalidOptionalRoot(record.CatalogRoot) {
		return Manifest{}, corrupt(32, "catalog root uses a reserved page")
	}
	if invalidOptionalRoot(record.ChangeRoot) {
		return Manifest{}, corrupt(56, "change root uses a reserved page")
	}
	offset := manifestHeaderSize
	if err := decodeRootEntries(payload, &offset, tableCount, record.TableRoots, "table", corrupt); err != nil {
		return Manifest{}, err
	}
	if err := decodeRootEntries(payload, &offset, indexCount, record.IndexRoots, "index", corrupt); err != nil {
		return Manifest{}, err
	}
	return record, nil
}

func sortedRootIDs(kind string, roots map[uint64]page.ID) ([]uint64, error) {
	ids := make([]uint64, 0, len(roots))
	for id, root := range roots {
		if id == 0 {
			return nil, dberrors.New(dberrors.CodeConstraint, "manifest.encode", kind+" ID must be nonzero")
		}
		if invalidOptionalRoot(root) {
			return nil, dberrors.New(dberrors.CodeConstraint, "manifest.encode", fmt.Sprintf("%s %d root %d is reserved", kind, id, root))
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids, nil
}

func decodeRootEntries(payload []byte, offset *int, count int, roots map[uint64]page.ID, kind string, corrupt func(int, string) error) error {
	var previous uint64
	for index := range count {
		entryOffset := *offset
		id := binary.LittleEndian.Uint64(payload[entryOffset : entryOffset+8])
		root := page.ID(binary.LittleEndian.Uint64(payload[entryOffset+8 : entryOffset+16]))
		if id == 0 {
			return corrupt(entryOffset, kind+" ID must be nonzero")
		}
		if index > 0 && id <= previous {
			return corrupt(entryOffset, kind+" IDs are not strictly increasing")
		}
		if invalidOptionalRoot(root) {
			return corrupt(entryOffset+8, fmt.Sprintf("%s %d root %d is reserved", kind, id, root))
		}
		roots[id] = root
		previous = id
		*offset += manifestRootEntrySize
	}
	return nil
}

func validateOptionalRoot(kind string, root page.ID) error {
	if invalidOptionalRoot(root) {
		return dberrors.New(dberrors.CodeConstraint, "manifest.encode", fmt.Sprintf("%s root %d is reserved", kind, root))
	}
	return nil
}

func invalidOptionalRoot(root page.ID) bool { return root != 0 && root < 2 }
