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
