package btree_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
	"yodadb/types"
)

func TestInsertLeafCreatesAndReplacesImmutableRootLeaf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "insert.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	empty, err := btree.Open(database, 0)
	if err != nil {
		t.Fatal(err)
	}
	key1 := integerKey(t, 1)
	first, err := empty.InsertLeaf(context.Background(), database, 1, key1, []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Old != 0 || first.Left != 2 || first.Split() {
		t.Fatalf("first mutation = %#v", first)
	}
	if _, found, err := empty.Get(context.Background(), key1); err != nil || found {
		t.Fatalf("empty root changed: found=%t error=%v", found, err)
	}

	one, err := btree.Open(database, first.Left)
	if err != nil {
		t.Fatal(err)
	}
	key2 := integerKey(t, 2)
	second, err := one.InsertLeaf(context.Background(), database, 2, key2, []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Old != first.Left || second.Left != 3 || second.Split() {
		t.Fatalf("second mutation = %#v", second)
	}
	if _, found, err := one.Get(context.Background(), key2); err != nil || found {
		t.Fatalf("old root observes new key: found=%t error=%v", found, err)
	}
	two, err := btree.Open(database, second.Left)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{string(key1): "one", string(key2): "two"} {
		value, found, err := two.Get(context.Background(), []byte(key))
		if err != nil || !found || string(value) != want {
			t.Fatalf("new root Get = %q, %t, %v; want %q", value, found, err, want)
		}
	}

	before := database.PageCount()
	if _, err := two.InsertLeaf(context.Background(), database, 3, key2, []byte("duplicate")); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate error = %v, want constraint", err)
	}
	if database.PageCount() != before {
		t.Fatalf("duplicate insert appended pages: count %d -> %d", before, database.PageCount())
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
	persisted, err := btree.Open(reopened, second.Left)
	if err != nil {
		t.Fatal(err)
	}
	if value, found, err := persisted.Get(context.Background(), key2); err != nil || !found || string(value) != "two" {
		t.Fatalf("reopened root Get = %q, %t, %v; want two", value, found, err)
	}
}

func TestInsertLeafSplitPreservesOldRoot(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "split.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	oldBuilder := page.NewLeafBuilder(0)
	largeValue := make([]byte, 1000)
	for _, number := range []int64{1, 3, 5} {
		largeValue[0] = byte(number)
		if err := oldBuilder.Insert(oldBuilder.Count(), integerKey(t, number), largeValue); err != nil {
			t.Fatal(err)
		}
	}
	oldPayload, err := oldBuilder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	oldRoot, err := database.Append(page.TypeBTreeLeaf, 1, oldPayload)
	if err != nil || oldRoot != 2 {
		t.Fatalf("append old root ID=%d error=%v", oldRoot, err)
	}
	oldTree, _ := btree.Open(database, oldRoot)
	insertedValue := bytes.Repeat([]byte{4}, 1000)
	mutation, err := oldTree.InsertLeaf(context.Background(), database, 2, integerKey(t, 4), insertedValue)
	if err != nil {
		t.Fatal(err)
	}
	if !mutation.Split() || mutation.Old != 2 || mutation.Right != 3 || mutation.Left != 4 || !bytes.Equal(mutation.Separator, integerKey(t, 4)) {
		t.Fatalf("split mutation = %#v", mutation)
	}
	leftPage, err := database.ReadPage(mutation.Left)
	if err != nil {
		t.Fatal(err)
	}
	left, err := page.DecodeLeaf(leftPage.Payload)
	if err != nil {
		t.Fatal(err)
	}
	rightPage, err := database.ReadPage(mutation.Right)
	if err != nil {
		t.Fatal(err)
	}
	right, err := page.DecodeLeaf(rightPage.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if left.RightSibling != mutation.Right || right.RightSibling != 0 || len(left.Entries) != 2 || len(right.Entries) != 2 {
		t.Fatalf("split leaves left=%#v right=%#v", left, right)
	}

	rootBuilder := page.NewInternalBuilder(mutation.Left)
	if err := rootBuilder.Insert(0, mutation.Separator, mutation.Right); err != nil {
		t.Fatal(err)
	}
	rootPayload, err := rootBuilder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := database.Append(page.TypeBTreeInternal, 2, rootPayload)
	if err != nil {
		t.Fatal(err)
	}
	newTree, _ := btree.Open(database, newRoot)
	assertTreeKeys(t, newTree, []int64{1, 3, 4, 5})
	assertTreeKeys(t, oldTree, []int64{1, 3, 5})
	if _, found, err := oldTree.Get(context.Background(), integerKey(t, 4)); err != nil || found {
		t.Fatalf("old root sees inserted key: found=%t error=%v", found, err)
	}
	storedOld, err := database.ReadPage(oldRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedOld.Payload, oldPayload) {
		t.Fatal("committed old leaf changed during copy-on-write insertion")
	}
}

func TestInsertLeafRejectsOversizedEntryAndCancellation(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "failure.yoda"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tree, _ := btree.Open(database, 0)
	before := database.PageCount()
	if _, err := tree.InsertLeaf(context.Background(), database, 1, integerKey(t, 1), make([]byte, page.MaxPayloadSize)); !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("oversized insert error = %v, want resource limit", err)
	}
	if database.PageCount() != before {
		t.Fatalf("oversized insert appended pages: count %d -> %d", before, database.PageCount())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tree.InsertLeaf(ctx, database, 1, integerKey(t, 1), []byte("one")); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("cancelled insert error = %v, want cancelled", err)
	}
}

func TestSplitAppendFailureLeavesOldRootReadable(t *testing.T) {
	builder := page.NewLeafBuilder(0)
	for _, key := range [][]byte{[]byte("a"), []byte("c"), []byte("e")} {
		if err := builder.Insert(builder.Count(), key, make([]byte, 1300)); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	source := pageMap{2: treePage(2, page.TypeBTreeLeaf, payload)}
	tree, _ := btree.Open(source, 2)
	appender := &failAfterAppender{successful: 1, nextID: 3}
	if _, err := tree.InsertLeaf(context.Background(), appender, 2, []byte("d"), make([]byte, 1300)); err == nil {
		t.Fatal("split insertion succeeded despite second append failure")
	}
	if len(appender.pages) != 1 {
		t.Fatalf("appended pages = %d, want one unreachable right leaf", len(appender.pages))
	}
	value, found, err := tree.Get(context.Background(), []byte("c"))
	if err != nil || !found || len(value) != 1300 {
		t.Fatalf("old root Get = %d bytes, %t, %v", len(value), found, err)
	}
}

func assertTreeKeys(t *testing.T, tree *btree.Tree, want []int64) {
	t.Helper()
	cursor, err := tree.Seek(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, expected := range want {
		key, _, err := cursor.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", index, err)
		}
		decoded, err := types.DecodeKey(key, types.KeyLimits{})
		if err != nil {
			t.Fatal(err)
		}
		actual, ok := decoded[0].Int64Value()
		if !ok || actual != expected {
			t.Fatalf("key %d = %v, want %d", index, decoded, expected)
		}
	}
	if _, _, err := cursor.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal cursor error = %v, want EOF", err)
	}
}

type failAfterAppender struct {
	successful int
	calls      int
	nextID     page.ID
	pages      []page.Page
}

func (a *failAfterAppender) Append(pageType page.Type, generation uint64, payload []byte) (page.ID, error) {
	if a.calls >= a.successful {
		return 0, errors.New("injected append failure")
	}
	id := a.nextID
	a.nextID++
	a.calls++
	a.pages = append(a.pages, treePage(id, pageType, bytes.Clone(payload)))
	return id, nil
}
