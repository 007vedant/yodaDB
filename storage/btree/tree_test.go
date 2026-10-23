package btree_test

import (
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
	"yodadb/types"
)

func TestLookupSeekAndRangeAcrossTreeLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tree.yoda")
	database, root := createTreeFixture(t, path)
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := manifest.Open(context.Background(), path, manifest.Options{ReadOnly: true, CachePages: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	tree, err := btree.Open(reopened, root)
	if err != nil {
		t.Fatal(err)
	}
	for number := int64(1); number <= 8; number++ {
		value, found, err := tree.Get(context.Background(), integerKey(t, number))
		if err != nil {
			t.Fatalf("Get(%d): %v", number, err)
		}
		if !found || len(value) != 1 || value[0] != byte(number) {
			t.Fatalf("Get(%d) = %v, %t; want [%d], true", number, value, found, number)
		}
	}
	for _, number := range []int64{0, 9} {
		if value, found, err := tree.Get(context.Background(), integerKey(t, number)); err != nil || found || value != nil {
			t.Fatalf("Get(%d) = %v, %t, %v; want missing", number, value, found, err)
		}
	}

	seek, err := tree.Seek(context.Background(), integerKey(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	assertCursorNumbers(t, seek, []int64{4, 5, 6, 7, 8})

	ranged, err := tree.Range(context.Background(), integerKey(t, 2), integerKey(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	assertCursorNumbers(t, ranged, []int64{2, 3, 4, 5, 6})
}

func TestEmptyTreeAndInvalidRange(t *testing.T) {
	tree, err := btree.Open(pageMap{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := tree.Get(context.Background(), []byte("key")); err != nil || found {
		t.Fatalf("empty Get found=%t error=%v", found, err)
	}
	cursor, err := tree.Seek(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cursor.Next(context.Background()); err != io.EOF {
		t.Fatalf("empty cursor error = %v, want EOF", err)
	}
	if _, err := tree.Range(context.Background(), []byte("z"), []byte("a")); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("invalid range error = %v, want constraint", err)
	}
	if _, err := btree.Open(pageMap{}, 1); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("reserved root error = %v, want constraint", err)
	}
}

func TestTraversalHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tree, err := btree.Open(pageMap{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Seek(ctx, nil); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("Seek cancellation error = %v, want cancelled", err)
	}
}

func TestTraversalRejectsUnorderedLeafAndInternalCycle(t *testing.T) {
	unorderedBuilder := page.NewLeafBuilder(0)
	_ = unorderedBuilder.Insert(0, []byte("z"), []byte("one"))
	_ = unorderedBuilder.Insert(1, []byte("a"), []byte("two"))
	unorderedPayload, _ := unorderedBuilder.Encode()
	tree, _ := btree.Open(pageMap{2: treePage(2, page.TypeBTreeLeaf, unorderedPayload)}, 2)
	if _, err := tree.Seek(context.Background(), nil); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("unordered leaf error = %v, want corruption", err)
	}

	cycleBuilder := page.NewInternalBuilder(2)
	_ = cycleBuilder.Insert(0, []byte("z"), 2)
	cyclePayload, _ := cycleBuilder.Encode()
	tree, _ = btree.Open(pageMap{2: treePage(2, page.TypeBTreeInternal, cyclePayload)}, 2)
	if _, err := tree.Seek(context.Background(), []byte("a")); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("internal cycle error = %v, want corruption", err)
	}
}

func TestCursorUsesParentPathInsteadOfStaleLeafSiblings(t *testing.T) {
	firstBuilder := page.NewLeafBuilder(3)
	_ = firstBuilder.Insert(0, []byte("a"), []byte("one"))
	firstPayload, _ := firstBuilder.Encode()
	secondBuilder := page.NewLeafBuilder(2)
	_ = secondBuilder.Insert(0, []byte("b"), []byte("two"))
	secondPayload, _ := secondBuilder.Encode()
	rootBuilder := page.NewInternalBuilder(2)
	_ = rootBuilder.Insert(0, []byte("b"), 3)
	rootPayload, _ := rootBuilder.Encode()
	tree, _ := btree.Open(pageMap{
		2: treePage(2, page.TypeBTreeLeaf, firstPayload),
		3: treePage(3, page.TypeBTreeLeaf, secondPayload),
		4: treePage(4, page.TypeBTreeInternal, rootPayload),
	}, 4)
	cursor, err := tree.Seek(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := cursor.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := cursor.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal cursor error = %v, want EOF", err)
	}
}

func TestTraversalRejectsWrongPageIdentityAndType(t *testing.T) {
	leafBuilder := page.NewLeafBuilder(0)
	_ = leafBuilder.Insert(0, []byte("a"), []byte("one"))
	payload, _ := leafBuilder.Encode()
	for name, stored := range map[string]page.Page{
		"identity": treePage(3, page.TypeBTreeLeaf, payload),
		"type":     treePage(2, page.TypeCatalog, nil),
	} {
		t.Run(name, func(t *testing.T) {
			tree, _ := btree.Open(pageMap{2: stored}, 2)
			if _, err := tree.Seek(context.Background(), nil); !errors.Is(err, dberrors.ErrCorruption) {
				t.Fatalf("Seek error = %v, want corruption", err)
			}
		})
	}
}

func createTreeFixture(t *testing.T, path string) (*manifest.Database, page.ID) {
	t.Helper()
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for leafIndex := 0; leafIndex < 4; leafIndex++ {
		rightSibling := page.ID(0)
		if leafIndex < 3 {
			rightSibling = page.ID(3 + leafIndex)
		}
		builder := page.NewLeafBuilder(rightSibling)
		for within := 0; within < 2; within++ {
			number := int64(leafIndex*2 + within + 1)
			if err := builder.Insert(builder.Count(), integerKey(t, number), []byte{byte(number)}); err != nil {
				t.Fatal(err)
			}
		}
		payload, err := builder.Encode()
		if err != nil {
			t.Fatal(err)
		}
		id, err := database.Append(page.TypeBTreeLeaf, 1, payload)
		if err != nil {
			t.Fatal(err)
		}
		if want := page.ID(2 + leafIndex); id != want {
			t.Fatalf("leaf ID = %d, want %d", id, want)
		}
	}

	left := page.NewInternalBuilder(2)
	_ = left.Insert(0, integerKey(t, 3), 3)
	leftPayload, err := left.Encode()
	if err != nil {
		t.Fatal(err)
	}
	leftID, err := database.Append(page.TypeBTreeInternal, 1, leftPayload)
	if err != nil || leftID != 6 {
		t.Fatalf("append left internal ID=%d error=%v", leftID, err)
	}
	right := page.NewInternalBuilder(4)
	_ = right.Insert(0, integerKey(t, 7), 5)
	rightPayload, err := right.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rightID, err := database.Append(page.TypeBTreeInternal, 1, rightPayload)
	if err != nil || rightID != 7 {
		t.Fatalf("append right internal ID=%d error=%v", rightID, err)
	}
	root := page.NewInternalBuilder(leftID)
	_ = root.Insert(0, integerKey(t, 5), rightID)
	rootPayload, err := root.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := database.Append(page.TypeBTreeInternal, 1, rootPayload)
	if err != nil || rootID != 8 {
		t.Fatalf("append root ID=%d error=%v", rootID, err)
	}
	return database, rootID
}

func integerKey(t *testing.T, number int64) []byte {
	t.Helper()
	encoded, err := types.EncodeKey(types.Row{types.Int64(number)}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertCursorNumbers(t *testing.T, cursor *btree.Cursor, want []int64) {
	t.Helper()
	for index, number := range want {
		key, value, err := cursor.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", index, err)
		}
		decoded, err := types.DecodeKey(key, types.KeyLimits{})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := decoded[0].Int64Value()
		if !ok || got != number || len(value) != 1 || value[0] != byte(number) {
			t.Fatalf("Next(%d) = %v/%v, want %d/[%d]", index, decoded, value, number, number)
		}
	}
	if _, _, err := cursor.Next(context.Background()); err != io.EOF {
		t.Fatalf("terminal Next error = %v, want EOF", err)
	}
}

type pageMap map[page.ID]page.Page

func (pages pageMap) ReadPage(id page.ID) (page.Page, error) {
	stored, ok := pages[id]
	if !ok {
		return page.Page{}, fmt.Errorf("missing page %d", id)
	}
	return stored, nil
}

func treePage(id page.ID, pageType page.Type, payload []byte) page.Page {
	return page.Page{Header: page.Header{PageID: id, Type: pageType}, Payload: payload}
}
