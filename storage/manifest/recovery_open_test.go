package manifest_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/internal/testutil/faultvfs"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestOpenIgnoresIncompleteGarbageTailWithoutRepairingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-tail.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	tail := make([]byte, 137)
	for index := range tail {
		tail[index] = byte(index + 1)
	}
	writeAt(t, path, tail, before.Size())

	readOnly, err := manifest.Open(context.Background(), path, manifest.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open with partial tail: %v", err)
	}
	assertLatestTxn(t, readOnly, 1)
	completePages := uint64(before.Size() / page.Size)
	if got := readOnly.PageCount(); got != completePages {
		t.Fatalf("PageCount = %d, want complete-page count %d", got, completePages)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	afterReadOnly, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := afterReadOnly.Size(), before.Size()+int64(len(tail)); got != want {
		t.Fatalf("read-only recovery changed file size to %d, want %d", got, want)
	}

	writable, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatalf("writable Open with partial tail: %v", err)
	}
	root := appendEmptyLeaf(t, writable, 2)
	if root != page.ID(completePages) {
		t.Fatalf("appended page ID = %d, want first incomplete page %d", root, completePages)
	}
	if err := writable.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	afterAppend, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := afterAppend.Size(), before.Size()+page.Size; got != want {
		t.Fatalf("file size after replacing partial tail = %d, want %d", got, want)
	}
}

func TestOpenIgnoresCompleteUnreachableGarbagePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "complete-tail.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}
	pagesBefore := database.PageCount()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	writeAt(t, path, make([]byte, page.Size), int64(pagesBefore)*page.Size)

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatalf("Open with complete garbage page: %v", err)
	}
	defer reopened.Close()
	assertLatestTxn(t, reopened, 1)
	if got := reopened.PageCount(); got != pagesBefore+1 {
		t.Fatalf("PageCount = %d, want physical count %d", got, pagesBefore+1)
	}
}

func TestOpenFallsBackWhenNewerManifestReferencesGarbageTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root-after-manifest.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}
	futureRoot := page.ID(database.PageCount() + 1)
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 2, ParentTxnID: 1,
		TableRoots: map[uint64]page.ID{1: futureRoot}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}
	if root := appendEmptyLeaf(t, database, 2); root != futureRoot {
		t.Fatalf("tail root = %d, want %d", root, futureRoot)
	}
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLatestTxn(t, reopened, 1)
}

func TestOpenFallsBackWhenRootReferencesChildAfterManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child-after-manifest.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}
	futureChild := page.ID(database.PageCount() + 2)
	builder := page.NewInternalBuilder(futureChild)
	if err := builder.Insert(0, []byte("separator"), futureChild); err != nil {
		t.Fatal(err)
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	root, err := database.Append(page.TypeBTreeInternal, 2, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 2, ParentTxnID: 1,
		TableRoots: map[uint64]page.ID{1: root}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}
	if child := appendEmptyLeaf(t, database, 2); child != futureChild {
		t.Fatalf("tail child = %d, want %d", child, futureChild)
	}
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertLatestTxn(t, reopened, 1)
}

func TestOpenDoesNotTreatReadFailureAsCandidateCorruption(t *testing.T) {
	tests := []struct {
		name   string
		readAt int
		root   bool
	}{
		{name: "metapage", readAt: 1},
		{name: "manifest", readAt: 3},
		{name: "root", readAt: 4, root: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filesystem, err := faultvfs.New(faultvfs.Config{})
			if err != nil {
				t.Fatal(err)
			}
			database, err := manifest.CreateWithVFS(context.Background(), filesystem, "recovery.yoda", manifest.Options{})
			if err != nil {
				t.Fatal(err)
			}
			record := manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}}
			if test.root {
				record.TableRoots[1] = appendEmptyLeaf(t, database, 1)
			}
			if _, err := database.Publish(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			if err := filesystem.Arm(faultvfs.Config{Faults: []faultvfs.Fault{{
				Kind: faultvfs.Read, At: test.readAt, Action: faultvfs.Fail,
			}}}); err != nil {
				t.Fatal(err)
			}

			if _, err := manifest.OpenWithVFS(context.Background(), filesystem, "recovery.yoda", manifest.Options{}); !errors.Is(err, dberrors.ErrIO) {
				t.Fatalf("Open error = %v, want I/O error", err)
			}
		})
	}
}

func TestOpenRejectsAmbiguousMetapageOrderingAndIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(manifest.Meta) manifest.Meta
	}{
		{name: "equal_generation", mutate: func(meta manifest.Meta) manifest.Meta { return meta }},
		{name: "different_file_identity", mutate: func(meta manifest.Meta) manifest.Meta {
			meta.FileID[0] ^= 0xff
			return meta
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ambiguous.yoda")
			database, err := manifest.Create(context.Background(), path, manifest.Options{})
			if err != nil {
				t.Fatal(err)
			}
			meta := test.mutate(database.Metadata().Meta)
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			generation := uint64(1)
			if test.name == "different_file_identity" {
				generation = 0
			}
			encoded, err := manifest.EncodeMetapage(1, generation, meta)
			if err != nil {
				t.Fatal(err)
			}
			writeAt(t, path, encoded, page.Size)
			if _, err := manifest.Open(context.Background(), path, manifest.Options{}); !errors.Is(err, dberrors.ErrCorruption) {
				t.Fatalf("Open error = %v, want corruption", err)
			}
		})
	}
}

func assertLatestTxn(t *testing.T, database *manifest.Database, want uint64) {
	t.Helper()
	record, found, err := database.LatestManifest()
	if err != nil || !found || record.TxnID != want {
		t.Fatalf("LatestManifest = (%+v, %t, %v), want txn %d", record, found, err, want)
	}
}

func writeAt(t *testing.T, path string, data []byte, offset int64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := file.WriteAt(data, offset); err != nil || count != len(data) {
		_ = file.Close()
		t.Fatalf("WriteAt = (%d, %v), want (%d, nil)", count, err, len(data))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
