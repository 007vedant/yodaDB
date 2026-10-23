package btree

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

const (
	emptyRoot       page.ID = 0
	minimumTreePage page.ID = 2
	maximumDepth            = 64
)

// PageSource supplies validated common pages. The interface lives at the
// consumer boundary and is implemented by manifest.Database.
type PageSource interface {
	ReadPage(page.ID) (page.Page, error)
}

// Tree is a read-only view rooted at one immutable B+ tree page.
type Tree struct {
	source PageSource
	root   page.ID
}

// Open constructs a read-only tree. Root zero represents an empty tree.
func Open(source PageSource, root page.ID) (*Tree, error) {
	if source == nil {
		return nil, dberrors.New(dberrors.CodeConstraint, "btree.open", "page source cannot be nil")
	}
	if root != emptyRoot && root < minimumTreePage {
		return nil, dberrors.New(dberrors.CodeConstraint, "btree.open", fmt.Sprintf("root page %d is reserved", root))
	}
	return &Tree{source: source, root: root}, nil
}

// Root returns the immutable root page ID used by this tree view.
func (t *Tree) Root() page.ID { return t.root }

// Get looks up one exact key.
func (t *Tree) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	cursor, err := t.Seek(ctx, key)
	if err != nil {
		return nil, false, err
	}
	foundKey, value, err := cursor.Next(ctx)
	if err == io.EOF {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !bytes.Equal(foundKey, key) {
		return nil, false, nil
	}
	return value, true, nil
}

// Seek returns a cursor positioned at the first key greater than or equal to
// start. The cursor has no upper bound.
func (t *Tree) Seek(ctx context.Context, start []byte) (*Cursor, error) {
	return t.newCursor(ctx, start, nil)
}

// Range returns a cursor over the half-open interval [start, end). A nil end is
// unbounded. A nil start begins at the first key.
func (t *Tree) Range(ctx context.Context, start, end []byte) (*Cursor, error) {
	if end != nil && bytes.Compare(start, end) > 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "btree.range", "range start is greater than end")
	}
	return t.newCursor(ctx, start, end)
}

func (t *Tree) newCursor(ctx context.Context, start, end []byte) (*Cursor, error) {
	if err := contextError(ctx, "btree.seek"); err != nil {
		return nil, err
	}
	cursor := &Cursor{tree: t, end: bytes.Clone(end), hasEnd: end != nil, visited: make(map[page.ID]struct{})}
	if t.root == emptyRoot {
		cursor.exhausted = true
		return cursor, nil
	}
	leafID, leaf, err := t.descend(ctx, start)
	if err != nil {
		return nil, err
	}
	cursor.pageID = leafID
	cursor.leaf = leaf
	cursor.index = sort.Search(len(leaf.Entries), func(index int) bool {
		return bytes.Compare(leaf.Entries[index].Key, start) >= 0
	})
	cursor.visited[leafID] = struct{}{}
	return cursor, nil
}

func (t *Tree) descend(ctx context.Context, key []byte) (page.ID, page.Leaf, error) {
	visited := make(map[page.ID]struct{})
	current := t.root
	for depth := 0; depth < maximumDepth; depth++ {
		if err := contextError(ctx, "btree.descend"); err != nil {
			return 0, page.Leaf{}, err
		}
		if current < minimumTreePage {
			return 0, page.Leaf{}, treeCorruption(current, page.TypeBTreeInternal, "child points to a reserved page")
		}
		if _, duplicate := visited[current]; duplicate {
			return 0, page.Leaf{}, treeCorruption(current, page.TypeBTreeInternal, "cycle detected during descent")
		}
		visited[current] = struct{}{}
		decoded, err := t.source.ReadPage(current)
		if err != nil {
			return 0, page.Leaf{}, err
		}
		if decoded.Header.PageID != current {
			return 0, page.Leaf{}, treeCorruption(current, decoded.Header.Type, fmt.Sprintf("page declares ID %d", decoded.Header.PageID))
		}
		switch decoded.Header.Type {
		case page.TypeBTreeLeaf:
			leaf, err := decodeLeaf(current, decoded.Payload)
			return current, leaf, err
		case page.TypeBTreeInternal:
			internal, err := page.DecodeInternal(decoded.Payload)
			if err != nil {
				return 0, page.Leaf{}, dberrors.Wrap(err, dberrors.CodeCorruption, "btree.descend", fmt.Sprintf("internal page %d", current))
			}
			index := sort.Search(len(internal.Entries), func(index int) bool {
				return bytes.Compare(key, internal.Entries[index].Key) < 0
			})
			if index == 0 {
				current = internal.FirstChild
			} else {
				current = internal.Entries[index-1].RightChild
			}
		default:
			return 0, page.Leaf{}, treeCorruption(current, decoded.Header.Type, "tree path references a non-tree page")
		}
	}
	return 0, page.Leaf{}, treeCorruption(current, page.TypeBTreeInternal, fmt.Sprintf("tree depth exceeds %d", maximumDepth))
}

// Cursor streams entries in global key order.
type Cursor struct {
	tree      *Tree
	pageID    page.ID
	leaf      page.Leaf
	index     int
	end       []byte
	hasEnd    bool
	lastKey   []byte
	hasLast   bool
	visited   map[page.ID]struct{}
	exhausted bool
}

// Next returns the next key/value pair or io.EOF. Returned bytes are defensive
// copies and remain valid after the next call.
func (c *Cursor) Next(ctx context.Context) ([]byte, []byte, error) {
	if c == nil || c.tree == nil || c.exhausted {
		return nil, nil, io.EOF
	}
	for {
		if err := contextError(ctx, "btree.cursor.next"); err != nil {
			return nil, nil, err
		}
		if c.index < len(c.leaf.Entries) {
			entry := c.leaf.Entries[c.index]
			c.index++
			if c.hasEnd && bytes.Compare(entry.Key, c.end) >= 0 {
				c.exhausted = true
				return nil, nil, io.EOF
			}
			if c.hasLast && bytes.Compare(c.lastKey, entry.Key) >= 0 {
				return nil, nil, treeCorruption(c.pageID, page.TypeBTreeLeaf, "leaf chain is not strictly increasing")
			}
			c.lastKey = bytes.Clone(entry.Key)
			c.hasLast = true
			return bytes.Clone(entry.Key), bytes.Clone(entry.Value), nil
		}

		next := c.leaf.RightSibling
		if next == 0 {
			c.exhausted = true
			return nil, nil, io.EOF
		}
		if next < minimumTreePage {
			return nil, nil, treeCorruption(c.pageID, page.TypeBTreeLeaf, fmt.Sprintf("right sibling %d is reserved", next))
		}
		if _, duplicate := c.visited[next]; duplicate {
			return nil, nil, treeCorruption(next, page.TypeBTreeLeaf, "cycle detected in leaf chain")
		}
		decoded, err := c.tree.source.ReadPage(next)
		if err != nil {
			return nil, nil, err
		}
		if decoded.Header.PageID != next {
			return nil, nil, treeCorruption(next, decoded.Header.Type, fmt.Sprintf("page declares ID %d", decoded.Header.PageID))
		}
		if decoded.Header.Type != page.TypeBTreeLeaf {
			return nil, nil, treeCorruption(next, decoded.Header.Type, "leaf sibling references a non-leaf page")
		}
		leaf, err := decodeLeaf(next, decoded.Payload)
		if err != nil {
			return nil, nil, err
		}
		c.visited[next] = struct{}{}
		c.pageID, c.leaf, c.index = next, leaf, 0
	}
}

func decodeLeaf(id page.ID, payload []byte) (page.Leaf, error) {
	leaf, err := page.DecodeLeaf(payload)
	if err != nil {
		return page.Leaf{}, dberrors.Wrap(err, dberrors.CodeCorruption, "btree.read_leaf", fmt.Sprintf("leaf page %d", id))
	}
	for index := 1; index < len(leaf.Entries); index++ {
		if bytes.Compare(leaf.Entries[index-1].Key, leaf.Entries[index].Key) >= 0 {
			return page.Leaf{}, treeCorruption(id, page.TypeBTreeLeaf, "leaf keys must be strictly increasing")
		}
	}
	return leaf, nil
}

func treeCorruption(id page.ID, pageType page.Type, invariant string) error {
	return &page.CorruptionError{PageID: id, PageType: pageType, Invariant: invariant}
}

func contextError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return dberrors.Wrap(err, dberrors.CodeCancelled, operation, "context cancelled")
	}
	return nil
}
