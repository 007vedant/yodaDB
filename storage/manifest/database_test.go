package manifest_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
	"yodadb/storage/vfs"
)

func TestCreateAppendCloseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{CachePages: 2})
	if err != nil {
		t.Fatal(err)
	}
	metadata := database.Metadata()
	if metadata.PageSize != page.Size || metadata.ActiveSlot != 0 || metadata.Generation != 1 || metadata.FileID == (manifest.FileID{}) {
		t.Fatalf("created metadata = %#v", metadata)
	}
	if database.PageCount() != 2 {
		t.Fatalf("created page count = %d, want 2", database.PageCount())
	}
	leafBuilder := page.NewLeafBuilder(0)
	if err := leafBuilder.Insert(0, []byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	payload, _ := leafBuilder.Encode()
	id, err := database.Append(page.TypeBTreeLeaf, 1, payload)
	if err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("appended page ID = %d, want 2", id)
	}
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{CachePages: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Metadata().FileID != metadata.FileID || reopened.PageCount() != 3 {
		t.Fatalf("reopened metadata/page count = %#v/%d", reopened.Metadata(), reopened.PageCount())
	}
	decoded, err := reopened.ReadPage(2)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := page.DecodeLeaf(decoded.Payload)
	if err != nil || string(leaf.Entries[0].Value) != "value" {
		t.Fatalf("reopened leaf = %#v, error %v", leaf, err)
	}
}

func TestPublishAndReopenLatestManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "published.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	record := manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{1: 0}, IndexRoots: map[uint64]page.ID{1: 0}}
	manifestID, err := database.Publish(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if manifestID != 2 {
		t.Fatalf("manifest page = %d, want 2", manifestID)
	}
	metadata := database.Metadata()
	if metadata.ActiveSlot != 1 || metadata.Generation != 2 || metadata.LatestTxnID != 1 || metadata.LatestManifestPageID != manifestID {
		t.Fatalf("metadata = %#v", metadata)
	}
	latest, found, err := database.LatestManifest()
	if err != nil || !found || latest.TxnID != 1 || latest.TableRoots[1] != 0 {
		t.Fatalf("LatestManifest = %#v, %t, %v", latest, found, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := manifest.Open(context.Background(), path, manifest.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, found, err = reopened.LatestManifest()
	if err != nil || !found || latest.TxnID != 1 || reopened.Metadata().ActiveSlot != 1 {
		t.Fatalf("reopened manifest = %#v, %t, %v", latest, found, err)
	}
}

func TestOpenFallsBackToOlderValidMetapage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fallback.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{'X'}, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if metadata := reopened.Metadata(); metadata.ActiveSlot != 1 || metadata.Generation != 0 {
		t.Fatalf("fallback selected %#v, want slot 1 generation 0", metadata)
	}
}

func TestOpenRejectsWhenBothMetapagesAreInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{'X'}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{'X'}, page.Size); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := manifest.Open(context.Background(), path, manifest.Options{}); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("Open error = %v, want corruption", err)
	}
}

func TestOpenHonorsAdvisoryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.yoda")
	first, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := manifest.Open(context.Background(), path, manifest.Options{}); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("second open error = %v, want lock conflict", err)
	}
}

func TestPublishAlternatesMetapagesByGeneration(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "generations.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for txnID := uint64(1); txnID <= 3; txnID++ {
		record := manifest.Manifest{TxnID: txnID, ParentTxnID: txnID - 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}}
		if _, err := database.Publish(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		metadata := database.Metadata()
		wantSlot := page.ID(txnID % 2)
		if metadata.ActiveSlot != wantSlot || metadata.Generation != txnID+1 || metadata.LatestTxnID != txnID {
			t.Fatalf("after txn %d metadata=%+v, want slot=%d generation=%d", txnID, metadata, wantSlot, txnID+1)
		}
	}
}

func TestOpenFallsBackWhenNewerManifestIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest-fallback.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 2, ParentTxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	corruptPageByte(t, path, secondID)

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, found, err := reopened.LatestManifest()
	if err != nil || !found || latest.TxnID != 1 {
		t.Fatalf("fallback manifest=%+v found=%t error=%v", latest, found, err)
	}
	if metadata := reopened.Metadata(); metadata.ActiveSlot != 1 || metadata.LatestManifestPageID != firstID {
		t.Fatalf("selected metadata=%+v", metadata)
	}
	thirdID, err := reopened.Publish(context.Background(), manifest.Manifest{TxnID: 2, ParentTxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}})
	if err != nil {
		t.Fatalf("publish after fallback: %v", err)
	}
	if metadata := reopened.Metadata(); metadata.ActiveSlot != 0 || metadata.Generation != 3 || metadata.LatestManifestPageID != thirdID {
		t.Fatalf("metadata after fallback publish=%+v", metadata)
	}
}

func TestOpenFallsBackWhenNewerRootIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root-fallback.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	root1 := appendEmptyLeaf(t, database, 1)
	if _, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{1: root1}, IndexRoots: map[uint64]page.ID{}}); err != nil {
		t.Fatal(err)
	}
	root2 := appendEmptyLeaf(t, database, 2)
	if _, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 2, ParentTxnID: 1, TableRoots: map[uint64]page.ID{1: root2}, IndexRoots: map[uint64]page.ID{}}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	corruptPageByte(t, path, root2)

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, found, err := reopened.LatestManifest()
	if err != nil || !found || latest.TxnID != 1 || latest.TableRoots[1] != root1 {
		t.Fatalf("fallback manifest=%+v found=%t error=%v", latest, found, err)
	}
}

func TestOpenRejectsWhenEveryReferencedCommitIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-valid-commit.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 2, ParentTxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	corruptPageByte(t, path, firstID)
	corruptPageByte(t, path, secondID)
	if _, err := manifest.Open(context.Background(), path, manifest.Options{}); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("Open error=%v, want corruption", err)
	}
}

func TestFailedMetapageSyncMakesHandleWriteUnusable(t *testing.T) {
	filesystem := &failSyncVFS{failAt: 3}
	database, err := manifest.CreateWithVFS(context.Background(), filesystem, filepath.Join(t.TempDir(), "failed-sync.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	record := manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}}
	if _, err := database.Publish(context.Background(), record); !errors.Is(err, dberrors.ErrIO) {
		t.Fatalf("Publish error=%v, want I/O", err)
	}
	if _, err := database.Publish(context.Background(), record); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("second Publish error=%v, want unusable handle", err)
	}
}

func TestOpenRejectsNewerRootWithOutOfFileChildren(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shallow-root.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	builder := page.NewInternalBuilder(999)
	if err := builder.Insert(0, []byte("separator"), 1000); err != nil {
		t.Fatal(err)
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	root, err := database.Append(page.TypeBTreeInternal, 1, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{1: root}, IndexRoots: map[uint64]page.ID{}}); err != nil {
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
	if _, found, err := reopened.LatestManifest(); err != nil || found {
		t.Fatalf("fallback found=%t error=%v, want empty older generation", found, err)
	}
}

func TestPublishRejectsExhaustedMetapageGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "generation-exhausted.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	meta := database.Metadata().Meta
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := manifest.EncodeMetapage(0, ^uint64(0), meta)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(encoded, 0); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = manifest.Open(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.Publish(context.Background(), manifest.Manifest{TxnID: 1, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{}})
	if !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("Publish error=%v, want resource limit", err)
	}
	if database.PageCount() != 2 {
		t.Fatalf("PageCount=%d, exhausted generation should append nothing", database.PageCount())
	}
}

func appendEmptyLeaf(t *testing.T, database *manifest.Database, generation uint64) page.ID {
	t.Helper()
	payload, err := page.NewLeafBuilder(0).Encode()
	if err != nil {
		t.Fatal(err)
	}
	id, err := database.Append(page.TypeBTreeLeaf, generation, payload)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func corruptPageByte(t *testing.T, path string, id page.ID) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{'X'}, int64(id)*page.Size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

type failSyncVFS struct{ failAt int }

func (f *failSyncVFS) Open(path string, options vfs.OpenOptions) (vfs.File, error) {
	file, err := (vfs.OS{}).Open(path, options)
	if err != nil {
		return nil, err
	}
	return &failSyncFile{File: file, failAt: f.failAt}, nil
}

type failSyncFile struct {
	vfs.File
	failAt int
	count  int
}

func (f *failSyncFile) Sync() error {
	f.count++
	if f.count == f.failAt {
		return dberrors.New(dberrors.CodeIO, "test.sync", "injected sync failure")
	}
	return f.File.Sync()
}
