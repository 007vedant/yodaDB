package btree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

// PageAppender append-allocates immutable pages. The interface is implemented
// by manifest.Database and lives here at its consumer boundary.
type PageAppender interface {
	Append(page.Type, uint64, []byte) (page.ID, error)
}

// LeafMutation describes the copy-on-write replacement for one old leaf. A
// zero Right means no split. On a split, Separator is the first key in Right.
// A parent/root publisher consumes this result in M3.4.
type LeafMutation struct {
	Old       page.ID
	Left      page.ID
	Right     page.ID
	Separator []byte
}

// Split reports whether the leaf mutation produced two replacement pages.
func (m LeafMutation) Split() bool { return m.Right != 0 }

// InsertLeaf copy-on-write inserts one unique key into its target leaf and
// appends replacement pages. It does not update parent pages or publish a root.
func (t *Tree) InsertLeaf(ctx context.Context, appender PageAppender, generation uint64, key, value []byte) (LeafMutation, error) {
	if appender == nil {
		return LeafMutation{}, dberrors.New(dberrors.CodeConstraint, "btree.insert_leaf", "page appender cannot be nil")
	}
	if len(key) == 0 {
		return LeafMutation{}, dberrors.New(dberrors.CodeConstraint, "btree.insert_leaf", "key cannot be empty")
	}
	if err := contextError(ctx, "btree.insert_leaf"); err != nil {
		return LeafMutation{}, err
	}

	oldID := emptyRoot
	leaf := page.Leaf{}
	if t.root != emptyRoot {
		var err error
		oldID, leaf, err = t.descend(ctx, key)
		if err != nil {
			return LeafMutation{}, err
		}
	}
	return insertIntoLeaf(ctx, appender, generation, oldID, leaf, key, value)
}

func insertIntoLeaf(ctx context.Context, appender PageAppender, generation uint64, oldID page.ID, leaf page.Leaf, key, value []byte) (LeafMutation, error) {
	position := sort.Search(len(leaf.Entries), func(index int) bool {
		return bytes.Compare(leaf.Entries[index].Key, key) >= 0
	})
	found := position < len(leaf.Entries) && bytes.Equal(leaf.Entries[position].Key, key)
	if found && !leaf.Entries[position].Tombstone {
		return LeafMutation{}, dberrors.New(dberrors.CodeConstraint, "btree.insert_leaf", "duplicate key")
	}

	var entries []page.LeafEntry
	if found {
		entries = replaceEntry(leaf.Entries, position, page.LeafEntry{Key: key, Value: value})
	} else {
		entries = insertEntry(leaf.Entries, position, page.LeafEntry{Key: key, Value: value})
	}
	builder, err := buildLeaf(entries, leaf.RightSibling)
	if err == nil {
		payload, err := builder.Encode()
		if err != nil {
			return LeafMutation{}, err
		}
		leftID, err := appendLeaf(ctx, appender, generation, payload)
		if err != nil {
			return LeafMutation{}, err
		}
		return LeafMutation{Old: oldID, Left: leftID}, nil
	} else if !errors.Is(err, dberrors.ErrResourceLimit) {
		return LeafMutation{}, err
	}

	left, right, err := chooseLeafSplit(entries, leaf.RightSibling)
	if err != nil {
		return LeafMutation{}, err
	}
	rightPayload, err := right.Encode()
	if err != nil {
		return LeafMutation{}, err
	}
	rightID, err := appendLeaf(ctx, appender, generation, rightPayload)
	if err != nil {
		return LeafMutation{}, err
	}
	if err := contextError(ctx, "btree.insert_leaf"); err != nil {
		return LeafMutation{}, err
	}
	left.SetRightSibling(rightID)
	leftPayload, err := left.Encode()
	if err != nil {
		return LeafMutation{}, err
	}
	leftID, err := appendLeaf(ctx, appender, generation, leftPayload)
	if err != nil {
		return LeafMutation{}, err
	}
	separator, err := right.Entry(0)
	if err != nil {
		return LeafMutation{}, err
	}
	return LeafMutation{Old: oldID, Left: leftID, Right: rightID, Separator: bytes.Clone(separator.Key)}, nil
}

func chooseLeafSplit(entries []page.LeafEntry, oldRightSibling page.ID) (*page.LeafBuilder, *page.LeafBuilder, error) {
	bestDifference := int(^uint(0) >> 1)
	var bestLeft, bestRight *page.LeafBuilder
	for split := 1; split < len(entries); split++ {
		left, leftErr := buildLeaf(entries[:split], 0)
		right, rightErr := buildLeaf(entries[split:], oldRightSibling)
		if leftErr != nil || rightErr != nil {
			continue
		}
		difference := left.UsedBytes() - right.UsedBytes()
		if difference < 0 {
			difference = -difference
		}
		if difference < bestDifference {
			bestDifference, bestLeft, bestRight = difference, left, right
		}
	}
	if bestLeft == nil {
		return nil, nil, dberrors.New(dberrors.CodeResourceLimit, "btree.split_leaf", "key/value entry cannot fit in a leaf page")
	}
	return bestLeft, bestRight, nil
}

func buildLeaf(entries []page.LeafEntry, rightSibling page.ID) (*page.LeafBuilder, error) {
	builder := page.NewLeafBuilder(rightSibling)
	for _, entry := range entries {
		if err := builder.Insert(builder.Count(), entry.Key, entry.Value); err != nil {
			return nil, err
		}
		if entry.Tombstone {
			if err := builder.Tombstone(builder.Count() - 1); err != nil {
				return nil, err
			}
		}
	}
	return builder, nil
}

func replaceEntry(entries []page.LeafEntry, position int, replacement page.LeafEntry) []page.LeafEntry {
	result := make([]page.LeafEntry, len(entries))
	for index, entry := range entries {
		result[index] = cloneEntry(entry)
	}
	result[position] = cloneEntry(replacement)
	return result
}

func insertEntry(entries []page.LeafEntry, position int, inserted page.LeafEntry) []page.LeafEntry {
	result := make([]page.LeafEntry, len(entries)+1)
	for index, entry := range entries[:position] {
		result[index] = cloneEntry(entry)
	}
	result[position] = cloneEntry(inserted)
	for index, entry := range entries[position:] {
		result[position+1+index] = cloneEntry(entry)
	}
	return result
}

func cloneEntry(entry page.LeafEntry) page.LeafEntry {
	return page.LeafEntry{Key: bytes.Clone(entry.Key), Value: bytes.Clone(entry.Value), Tombstone: entry.Tombstone}
}

func appendLeaf(ctx context.Context, appender PageAppender, generation uint64, payload []byte) (page.ID, error) {
	if err := contextError(ctx, "btree.append_leaf"); err != nil {
		return 0, err
	}
	id, err := appender.Append(page.TypeBTreeLeaf, generation, payload)
	if err != nil {
		return 0, err
	}
	if id < minimumTreePage {
		return 0, dberrors.New(dberrors.CodeCorruption, "btree.append_leaf", fmt.Sprintf("appender returned reserved page ID %d", id))
	}
	return id, nil
}
