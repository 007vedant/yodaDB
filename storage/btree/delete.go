package btree

import (
	"bytes"
	"context"
	"sort"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

// Delete copy-on-write marks one key as a tombstone and returns the unpublished
// new root. It does not merge pages. A missing or already deleted key returns
// the current root and deleted=false without appending pages.
func (t *Tree) Delete(ctx context.Context, appender PageAppender, generation uint64, key []byte) (root page.ID, deleted bool, err error) {
	if appender == nil {
		return 0, false, dberrors.New(dberrors.CodeConstraint, "btree.delete", "page appender cannot be nil")
	}
	if len(key) == 0 {
		return 0, false, dberrors.New(dberrors.CodeConstraint, "btree.delete", "key cannot be empty")
	}
	if err := contextError(ctx, "btree.delete"); err != nil {
		return 0, false, err
	}
	if t.root == emptyRoot {
		return emptyRoot, false, nil
	}

	leafID, leaf, path, err := t.descendPath(ctx, key)
	if err != nil {
		return 0, false, err
	}
	position := sort.Search(len(leaf.Entries), func(index int) bool {
		return bytes.Compare(leaf.Entries[index].Key, key) >= 0
	})
	if position == len(leaf.Entries) || !bytes.Equal(leaf.Entries[position].Key, key) || leaf.Entries[position].Tombstone {
		return t.root, false, nil
	}

	builder := page.NewLeafBuilderFromDecoded(leaf)
	if err := builder.Tombstone(position); err != nil {
		return 0, false, err
	}
	payload, err := builder.Encode()
	if err != nil {
		return 0, false, err
	}
	replacement, err := appendLeaf(ctx, appender, generation, payload)
	if err != nil {
		return 0, false, err
	}
	change := pageMutation{old: leafID, left: replacement}
	for index := len(path) - 1; index >= 0; index-- {
		change, err = rewriteInternal(ctx, appender, generation, path[index], change)
		if err != nil {
			return 0, false, err
		}
	}
	if change.split() {
		return 0, false, dberrors.New(dberrors.CodeCorruption, "btree.delete", "tombstone deletion unexpectedly split an internal page")
	}
	return change.left, true, nil
}
