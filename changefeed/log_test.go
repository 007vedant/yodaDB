package changefeed_test

import (
	"context"
	"path/filepath"
	"testing"

	"yodadb/changefeed"
	"yodadb/storage/manifest"
	"yodadb/types"
)

func TestAppendPersistsTransactionAndChunkOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "changes.yoda")
	store, err := manifest.Create(ctx, path, manifest.Options{Durability: manifest.DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	large := types.Bytes(make([]byte, 9000))
	root, err := changefeed.Append(ctx, store, 0, 1, []changefeed.Change{
		{TableID: 4, Kind: changefeed.Insert, AfterKey: []byte("a"), After: &changefeed.Image{SchemaVersion: 1, Row: types.Row{large}}},
		{TableID: 4, Kind: changefeed.Update, BeforeKey: []byte("a"), AfterKey: []byte("b"), Before: image(1, "before"), After: image(1, "after")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, manifest.Manifest{TxnID: 1, ChangeRoot: root}); err != nil {
		t.Fatal(err)
	}
	root, err = changefeed.Append(ctx, store, root, 2, []changefeed.Change{
		{TableID: 4, Kind: changefeed.Delete, BeforeKey: []byte("b"), Before: image(1, "after")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, manifest.Manifest{TxnID: 2, ParentTxnID: 1, ChangeRoot: root}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = manifest.Open(ctx, path, manifest.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, found, err := store.LatestManifest()
	if err != nil || !found {
		t.Fatalf("latest manifest found=%t error=%v", found, err)
	}
	changes, err := changefeed.ReadAll(ctx, store, record.ChangeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("change count=%d, want 3", len(changes))
	}
	if changes[0].TxnID != 1 || changes[0].Sequence != 0 || changes[1].TxnID != 1 || changes[1].Sequence != 1 || changes[2].TxnID != 2 || changes[2].Sequence != 0 {
		t.Fatalf("change order=%+v", changes)
	}
	bytesValue, ok := changes[0].After.Row[0].BytesValue()
	if !ok || len(bytesValue) != 9000 {
		t.Fatalf("chunked image length=%d, ok=%t", len(bytesValue), ok)
	}
}
