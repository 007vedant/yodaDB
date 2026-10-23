package btree_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestDeleteTombstonesAcrossArbitraryHeightAndPreservesSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delete.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := btree.Open(database, 0)
	const total = 170
	for number := 1; number <= total; number++ {
		root, err := tree.Insert(context.Background(), database, uint64(number), integerKey(t, int64(number)), bytes.Repeat([]byte{byte(number)}, 3000))
		if err != nil {
			t.Fatalf("Insert(%d): %v", number, err)
		}
		tree, _ = btree.Open(database, root)
	}
	original := tree
	originalHeight := treeHeight(t, database, original.Root())
	if originalHeight < 3 {
		t.Fatalf("original height = %d, want arbitrary-height tree", originalHeight)
	}

	for _, number := range []int64{1, 85, 170} {
		root, deleted, err := tree.Delete(context.Background(), database, uint64(total+number), integerKey(t, number))
		if err != nil || !deleted {
			t.Fatalf("Delete(%d) = root %d, deleted %t, error %v", number, root, deleted, err)
		}
		tree, _ = btree.Open(database, root)
		if _, found, err := tree.Get(context.Background(), integerKey(t, number)); err != nil || found {
			t.Fatalf("deleted key %d: found=%t error=%v", number, found, err)
		}
	}
	if height := treeHeight(t, database, tree.Root()); height != originalHeight {
		t.Fatalf("height after delete = %d, want unchanged %d", height, originalHeight)
	}
	assertTreeCount(t, tree, total-3)
	assertTreeCount(t, original, total)
	for _, number := range []int64{1, 85, 170} {
		if value, found, err := original.Get(context.Background(), integerKey(t, number)); err != nil || !found || len(value) != 3000 {
			t.Fatalf("old snapshot key %d = %d bytes, %t, %v", number, len(value), found, err)
		}
	}
	leaf := leafForKey(t, database, tree.Root(), integerKey(t, 85))
	position := sort.Search(len(leaf.Entries), func(index int) bool {
		return bytes.Compare(leaf.Entries[index].Key, integerKey(t, 85)) >= 0
	})
	if position == len(leaf.Entries) || !leaf.Entries[position].Tombstone || leaf.Entries[position].Value != nil {
		t.Fatalf("deleted entry is not a value-free tombstone: %#v", leaf.Entries)
	}

	before := database.PageCount()
	for _, missing := range []int64{85, 999} {
		root, deleted, err := tree.Delete(context.Background(), database, 999, integerKey(t, missing))
		if err != nil || deleted || root != tree.Root() {
			t.Fatalf("Delete missing %d = root %d, deleted %t, error %v", missing, root, deleted, err)
		}
	}
	if database.PageCount() != before {
		t.Fatalf("missing deletes appended pages: count %d -> %d", before, database.PageCount())
	}

	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := manifest.Open(context.Background(), path, manifest.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, _ := btree.Open(reopened, tree.Root())
	assertTreeCount(t, persisted, total-3)
}

func TestInsertRevivesTombstonedKeyWithoutChangingOldRoots(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "revive.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	empty, _ := btree.Open(database, 0)
	key := integerKey(t, 7)
	liveRoot, err := empty.Insert(context.Background(), database, 1, key, []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	live, _ := btree.Open(database, liveRoot)
	deletedRoot, deleted, err := live.Delete(context.Background(), database, 2, key)
	if err != nil || !deleted {
		t.Fatalf("Delete = root %d, deleted %t, error %v", deletedRoot, deleted, err)
	}
	deletedTree, _ := btree.Open(database, deletedRoot)
	revivedRoot, err := deletedTree.Insert(context.Background(), database, 3, key, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	revived, _ := btree.Open(database, revivedRoot)
	if value, found, err := revived.Get(context.Background(), key); err != nil || !found || string(value) != "new" {
		t.Fatalf("revived Get = %q, %t, %v", value, found, err)
	}
	if _, found, err := deletedTree.Get(context.Background(), key); err != nil || found {
		t.Fatalf("deleted snapshot found=%t error=%v", found, err)
	}
	if value, found, err := live.Get(context.Background(), key); err != nil || !found || string(value) != "old" {
		t.Fatalf("original snapshot Get = %q, %t, %v", value, found, err)
	}
}

func TestDeleteParentAppendFailureAndCancellation(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "delete-failure.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tree, _ := btree.Open(database, 0)
	for number := int64(1); number <= 2; number++ {
		root, err := tree.Insert(context.Background(), database, uint64(number), integerKey(t, number), bytes.Repeat([]byte{byte(number)}, 3000))
		if err != nil {
			t.Fatal(err)
		}
		tree, _ = btree.Open(database, root)
	}
	before := database.PageCount()
	failing := &delegatingFailAppender{delegate: database, successful: 1}
	if _, _, err := tree.Delete(context.Background(), failing, 3, integerKey(t, 2)); err == nil {
		t.Fatal("delete succeeded despite parent append failure")
	}
	if database.PageCount() != before+1 {
		t.Fatalf("page count = %d, want one unreachable leaf", database.PageCount())
	}
	if _, found, err := tree.Get(context.Background(), integerKey(t, 2)); err != nil || !found {
		t.Fatalf("old root key found=%t error=%v", found, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := tree.Delete(ctx, database, 4, integerKey(t, 1)); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("cancelled delete error = %v, want cancelled", err)
	}
}

func leafForKey(t *testing.T, source interface {
	ReadPage(page.ID) (page.Page, error)
}, root page.ID, key []byte) page.Leaf {
	t.Helper()
	for {
		stored, err := source.ReadPage(root)
		if err != nil {
			t.Fatal(err)
		}
		switch stored.Header.Type {
		case page.TypeBTreeLeaf:
			leaf, err := page.DecodeLeaf(stored.Payload)
			if err != nil {
				t.Fatal(err)
			}
			return leaf
		case page.TypeBTreeInternal:
			internal, err := page.DecodeInternal(stored.Payload)
			if err != nil {
				t.Fatal(err)
			}
			index := sort.Search(len(internal.Entries), func(index int) bool {
				return bytes.Compare(key, internal.Entries[index].Key) < 0
			})
			if index == 0 {
				root = internal.FirstChild
			} else {
				root = internal.Entries[index-1].RightChild
			}
		default:
			t.Fatalf("tree page %d has type %s", root, stored.Header.Type)
		}
	}
}
