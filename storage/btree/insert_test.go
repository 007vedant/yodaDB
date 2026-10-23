package btree_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestInsertBuildsArbitraryHeightAndPreservesSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arbitrary-height.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}

	tree, err := btree.Open(database, 0)
	if err != nil {
		t.Fatal(err)
	}
	const total = 200
	checkpoints := make(map[int]page.ID)
	for number := 1; number <= total; number++ {
		root, err := tree.Insert(
			context.Background(),
			database,
			uint64(number),
			integerKey(t, int64(number)),
			bytes.Repeat([]byte{byte(number)}, 3000),
		)
		if err != nil {
			t.Fatalf("Insert(%d): %v", number, err)
		}
		tree, err = btree.Open(database, root)
		if err != nil {
			t.Fatal(err)
		}
		if number == 1 || number == 50 || number == total {
			checkpoints[number] = root
		}
	}

	if height := treeHeight(t, database, checkpoints[total]); height < 3 {
		t.Fatalf("tree height = %d, want at least 3 after internal root split", height)
	}
	for count, root := range checkpoints {
		snapshot, err := btree.Open(database, root)
		if err != nil {
			t.Fatal(err)
		}
		assertTreeCount(t, snapshot, count)
		if value, found, err := snapshot.Get(context.Background(), integerKey(t, int64(count))); err != nil || !found || len(value) != 3000 {
			t.Fatalf("snapshot %d last key = %d bytes, %t, %v", count, len(value), found, err)
		}
		if count < total {
			if _, found, err := snapshot.Get(context.Background(), integerKey(t, int64(count+1))); err != nil || found {
				t.Fatalf("snapshot %d observes later key: found=%t error=%v", count, found, err)
			}
		}
	}

	before := database.PageCount()
	if _, err := tree.Insert(context.Background(), database, total+1, integerKey(t, total), []byte("duplicate")); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate insert error = %v, want constraint", err)
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
	for _, count := range []int{50, total} {
		snapshot, err := btree.Open(reopened, checkpoints[count])
		if err != nil {
			t.Fatal(err)
		}
		assertTreeCount(t, snapshot, count)
	}
}

func TestInsertParentAppendFailureLeavesOldRootReadable(t *testing.T) {
	database, err := manifest.Create(context.Background(), filepath.Join(t.TempDir(), "parent-failure.yoda"), manifest.Options{})
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
	oldRoot := tree.Root()
	before := database.PageCount()
	failing := &delegatingFailAppender{delegate: database, successful: 1}
	if _, err := tree.Insert(context.Background(), failing, 3, integerKey(t, 3), []byte("three")); err == nil {
		t.Fatal("insert succeeded despite parent append failure")
	}
	if database.PageCount() != before+1 {
		t.Fatalf("page count = %d, want one unreachable leaf after failure", database.PageCount())
	}
	if tree.Root() != oldRoot {
		t.Fatalf("tree root changed from %d to %d", oldRoot, tree.Root())
	}
	assertTreeCount(t, tree, 2)
	if _, found, err := tree.Get(context.Background(), integerKey(t, 3)); err != nil || found {
		t.Fatalf("old root observes failed key: found=%t error=%v", found, err)
	}
}

func TestInsertHonorsCancellation(t *testing.T) {
	tree, err := btree.Open(pageMap{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tree.Insert(ctx, &failAfterAppender{successful: 1, nextID: 2}, 1, []byte("key"), []byte("value")); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("cancelled insert error = %v, want cancelled", err)
	}
}

func treeHeight(t *testing.T, source interface {
	ReadPage(page.ID) (page.Page, error)
}, root page.ID) int {
	t.Helper()
	height := 0
	for root != 0 {
		height++
		stored, err := source.ReadPage(root)
		if err != nil {
			t.Fatal(err)
		}
		switch stored.Header.Type {
		case page.TypeBTreeLeaf:
			return height
		case page.TypeBTreeInternal:
			internal, err := page.DecodeInternal(stored.Payload)
			if err != nil {
				t.Fatal(err)
			}
			root = internal.FirstChild
		default:
			t.Fatalf("page %d has type %s", root, stored.Header.Type)
		}
	}
	t.Fatal("tree has empty root")
	return 0
}

func assertTreeCount(t *testing.T, tree *btree.Tree, want int) {
	t.Helper()
	cursor, err := tree.Seek(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; ; count++ {
		_, _, err := cursor.Next(context.Background())
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if count != want {
				t.Fatalf("tree contains %d entries, want %d", count, want)
			}
			return
		}
	}
}

type delegatingFailAppender struct {
	delegate interface {
		Append(page.Type, uint64, []byte) (page.ID, error)
	}
	successful int
	calls      int
}

func (a *delegatingFailAppender) Append(pageType page.Type, generation uint64, payload []byte) (page.ID, error) {
	if a.calls >= a.successful {
		return 0, fmt.Errorf("injected append failure")
	}
	a.calls++
	return a.delegate.Append(pageType, generation, payload)
}
