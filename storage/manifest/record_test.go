package manifest_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestManifestRoundTripIsCanonical(t *testing.T) {
	record := manifest.Manifest{
		TxnID:               7,
		ParentTxnID:         6,
		CommitUnixNano:      -123,
		CatalogRoot:         9,
		OldestRetainedTxnID: 2,
		ChangeRoot:          10,
		TableRoots:          map[uint64]page.ID{8: 18, 2: 12},
		IndexRoots:          map[uint64]page.ID{5: 15, 1: 0},
	}
	first, err := manifest.EncodeManifest(record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manifest.EncodeManifest(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("manifest encoding is not deterministic")
	}
	decoded, err := manifest.DecodeManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.TxnID != record.TxnID || decoded.ParentTxnID != record.ParentTxnID || decoded.CommitUnixNano != record.CommitUnixNano ||
		decoded.CatalogRoot != record.CatalogRoot || decoded.OldestRetainedTxnID != record.OldestRetainedTxnID || decoded.ChangeRoot != record.ChangeRoot ||
		decoded.TableRoots[2] != 12 || decoded.TableRoots[8] != 18 || decoded.IndexRoots[1] != 0 || decoded.IndexRoots[5] != 15 {
		t.Fatalf("decoded manifest = %#v", decoded)
	}
	if firstTableID := binary.LittleEndian.Uint64(first[64:72]); firstTableID != 2 {
		t.Fatalf("first table ID = %d, want sorted ID 2", firstTableID)
	}
}

func TestManifestRejectsConstraintsAndResourceLimit(t *testing.T) {
	for name, record := range map[string]manifest.Manifest{
		"zero txn":       {},
		"future parent":  {TxnID: 2, ParentTxnID: 2},
		"retention":      {TxnID: 2, OldestRetainedTxnID: 3},
		"reserved root":  {TxnID: 1, CatalogRoot: 1},
		"zero object ID": {TxnID: 1, TableRoots: map[uint64]page.ID{0: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := manifest.EncodeManifest(record); !errors.Is(err, dberrors.ErrConstraint) {
				t.Fatalf("EncodeManifest error = %v, want constraint", err)
			}
		})
	}
	roots := make(map[uint64]page.ID)
	for id := uint64(1); id <= 250; id++ {
		roots[id] = 0
	}
	if _, err := manifest.EncodeManifest(manifest.Manifest{TxnID: 1, TableRoots: roots}); !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("oversized manifest error = %v, want resource limit", err)
	}
}

func TestDecodeManifestRejectsMalformedPayloads(t *testing.T) {
	valid, err := manifest.EncodeManifest(manifest.Manifest{TxnID: 2, ParentTxnID: 1, TableRoots: map[uint64]page.ID{1: 2, 2: 3}})
	if err != nil {
		t.Fatal(err)
	}
	tests := [][]byte{
		valid[:len(valid)-1],
		mutateManifest(valid, 0, 1),
		mutateManifest(valid, 4, 1),
		mutateManifest(valid, 6, 1),
		mutateManifest(valid, 8, 2),
		mutateManifest(valid, 48, 1),
	}
	unordered := bytes.Clone(valid)
	copy(unordered[80:96], unordered[64:80])
	tests = append(tests, unordered)
	reserved := bytes.Clone(valid)
	binary.LittleEndian.PutUint64(reserved[72:80], 1)
	tests = append(tests, reserved)
	for index, payload := range tests {
		if _, err := manifest.DecodeManifest(payload); !errors.Is(err, dberrors.ErrCorruption) {
			t.Errorf("case %d error = %v, want corruption", index, err)
		}
	}
}

func TestDecodeManifestRejectsEveryTruncatedPrefix(t *testing.T) {
	valid, _ := manifest.EncodeManifest(manifest.Manifest{TxnID: 1, IndexRoots: map[uint64]page.ID{1: 0}})
	for length := range len(valid) {
		if _, err := manifest.DecodeManifest(valid[:length]); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("prefix %d error = %v, want corruption", length, err)
		}
	}
}

func FuzzDecodeManifest(f *testing.F) {
	valid, _ := manifest.EncodeManifest(manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{1: 2}})
	f.Add(valid)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = manifest.DecodeManifest(payload)
	})
}

func mutateManifest(payload []byte, offset int, xor byte) []byte {
	mutated := bytes.Clone(payload)
	mutated[offset] ^= xor
	return mutated
}
