package manifest_test

import (
	"context"
	"path/filepath"
	"testing"

	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestManifestHistoryAndExactLookup(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "history.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	for txnID := uint64(1); txnID <= 3; txnID++ {
		record := manifest.Manifest{
			TxnID: txnID, ParentTxnID: txnID - 1, CommitUnixNano: int64(txnID * 100),
			TableRoots: map[uint64]page.ID{1: 0}, IndexRoots: map[uint64]page.ID{},
		}
		if _, err := database.Publish(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}

	history, err := database.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history length=%d, want 3", len(history))
	}
	for index, record := range history {
		want := uint64(index + 1)
		if record.TxnID != want || record.CommitUnixNano != int64(want*100) {
			t.Fatalf("history[%d]=%+v, want transaction %d", index, record, want)
		}
		found, ok, err := database.ManifestByTxnID(want)
		if err != nil || !ok || found.TxnID != want {
			t.Fatalf("ManifestByTxnID(%d)=%+v found=%t error=%v", want, found, ok, err)
		}
	}
	for _, txnID := range []uint64{0, 4} {
		if record, found, err := database.ManifestByTxnID(txnID); err != nil || found {
			t.Fatalf("ManifestByTxnID(%d)=%+v found=%t error=%v, want absent", txnID, record, found, err)
		}
	}
}

func TestManifestHistoryChoosesLatestPhysicalRetry(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "history-retry.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Publish(context.Background(), manifest.Manifest{
		TxnID: 1, CommitUnixNano: 100, TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}); err != nil {
		t.Fatal(err)
	}

	unpublished := manifest.Manifest{
		TxnID: 2, ParentTxnID: 1, CommitUnixNano: 200,
		TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}
	payload, err := manifest.EncodeManifest(unpublished)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Append(page.TypeManifest, 2, payload); err != nil {
		t.Fatal(err)
	}
	committed := unpublished
	committed.CommitUnixNano = 300
	if _, err := database.Publish(context.Background(), committed); err != nil {
		t.Fatal(err)
	}

	record, found, err := database.ManifestByTxnID(2)
	if err != nil || !found {
		t.Fatalf("ManifestByTxnID(2) found=%t error=%v", found, err)
	}
	if record.CommitUnixNano != 300 {
		t.Fatalf("transaction 2 timestamp=%d, want committed retry 300", record.CommitUnixNano)
	}
	history, err := database.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].TxnID != 1 || history[1].CommitUnixNano != 300 {
		t.Fatalf("history=%+v", history)
	}
}

func TestEmptyManifestHistory(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "empty-history.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	history, err := database.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("empty history=%+v", history)
	}
}

func TestManifestHistorySurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen-history.yoda")
	database, err := manifest.Create(ctx, path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for txnID := uint64(1); txnID <= 2; txnID++ {
		if _, err := database.Publish(ctx, manifest.Manifest{
			TxnID: txnID, ParentTxnID: txnID - 1,
			TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = manifest.Open(ctx, path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	history, err := database.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].TxnID != 1 || history[1].TxnID != 2 {
		t.Fatalf("history after reopen=%+v", history)
	}
}
