package btree_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/page"
)

func TestWriteTreeDisplaysHierarchyAndTombstones(t *testing.T) {
	leftBuilder := page.NewLeafBuilder(3)
	_ = leftBuilder.Insert(0, []byte("a"), []byte("one"))
	_ = leftBuilder.Insert(1, []byte("b"), []byte("two"))
	_ = leftBuilder.Tombstone(1)
	leftPayload, _ := leftBuilder.Encode()
	rightBuilder := page.NewLeafBuilder(0)
	_ = rightBuilder.Insert(0, []byte("c"), []byte("three"))
	_ = rightBuilder.Insert(1, []byte("d"), []byte("four"))
	rightPayload, _ := rightBuilder.Encode()
	rootBuilder := page.NewInternalBuilder(2)
	_ = rootBuilder.Insert(0, []byte("c"), 3)
	rootPayload, _ := rootBuilder.Encode()
	pages := pageMap{
		2: {Header: page.Header{PageID: 2, Type: page.TypeBTreeLeaf, Generation: 1}, Payload: leftPayload},
		3: {Header: page.Header{PageID: 3, Type: page.TypeBTreeLeaf, Generation: 1}, Payload: rightPayload},
		4: {Header: page.Header{PageID: 4, Type: page.TypeBTreeInternal, Generation: 2}, Payload: rootPayload},
	}
	tree, _ := btree.Open(pages, 4)
	var output bytes.Buffer
	if err := tree.WriteTree(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	want := "tree root=4\n" +
		"internal page=4 generation=2 separators=1\n" +
		"├── child[0] range=(-inf,0x63)\n" +
		"│   └── leaf page=2 generation=1 entries=2 live=1 tombstones=1 right=3 keys=0x61..0x62\n" +
		"└── child[1] range=[0x63,+inf)\n" +
		"    └── leaf page=3 generation=1 entries=2 live=2 tombstones=0 right=0 keys=0x63..0x64\n"
	if output.String() != want {
		t.Fatalf("WriteTree output:\n%s\nwant:\n%s", output.String(), want)
	}
}

func TestWriteTreeEmptyAndFailures(t *testing.T) {
	empty, _ := btree.Open(pageMap{}, 0)
	var output bytes.Buffer
	if err := empty.WriteTree(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "tree root=0\nempty\n"; got != want {
		t.Fatalf("empty output = %q, want %q", got, want)
	}
	if err := empty.WriteTree(context.Background(), nil); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("nil writer error = %v, want constraint", err)
	}

	cycleBuilder := page.NewInternalBuilder(2)
	_ = cycleBuilder.Insert(0, []byte("z"), 2)
	cyclePayload, _ := cycleBuilder.Encode()
	cycle, _ := btree.Open(pageMap{2: treePage(2, page.TypeBTreeInternal, cyclePayload)}, 2)
	if err := cycle.WriteTree(context.Background(), &output); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("cycle error = %v, want corruption", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := empty.WriteTree(ctx, &output); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("cancelled WriteTree error = %v, want cancelled", err)
	}
}
