package btree

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

type pageMutation struct {
	old       page.ID
	left      page.ID
	right     page.ID
	separator []byte
}

func (m pageMutation) split() bool { return m.right != 0 }

// Insert copy-on-write inserts one unique key, rewrites the affected path, and
// returns the unpublished new root. The old root remains readable. The caller
// controls synchronization and root publication.
func (t *Tree) Insert(ctx context.Context, appender PageAppender, generation uint64, key, value []byte) (page.ID, error) {
	if appender == nil {
		return 0, dberrors.New(dberrors.CodeConstraint, "btree.insert", "page appender cannot be nil")
	}
	if len(key) == 0 {
		return 0, dberrors.New(dberrors.CodeConstraint, "btree.insert", "key cannot be empty")
	}
	if err := contextError(ctx, "btree.insert"); err != nil {
		return 0, err
	}

	oldLeafID := emptyRoot
	leaf := page.Leaf{}
	var path []pathStep
	if t.root != emptyRoot {
		var err error
		oldLeafID, leaf, path, err = t.descendPath(ctx, key)
		if err != nil {
			return 0, err
		}
	}
	leafChange, err := insertIntoLeaf(ctx, appender, generation, oldLeafID, leaf, key, value)
	if err != nil {
		return 0, err
	}
	change := pageMutation{
		old:       leafChange.Old,
		left:      leafChange.Left,
		right:     leafChange.Right,
		separator: bytes.Clone(leafChange.Separator),
	}
	for index := len(path) - 1; index >= 0; index-- {
		change, err = rewriteInternal(ctx, appender, generation, path[index], change)
		if err != nil {
			return 0, err
		}
	}
	if !change.split() {
		return change.left, nil
	}
	return appendNewRoot(ctx, appender, generation, change)
}

func rewriteInternal(ctx context.Context, appender PageAppender, generation uint64, step pathStep, child pageMutation) (pageMutation, error) {
	if step.childIndex < 0 || step.childIndex > len(step.internal.Entries) {
		return pageMutation{}, treeCorruption(step.id, page.TypeBTreeInternal, "selected child index is outside internal page")
	}
	if actual := internalChild(step.internal, step.childIndex); actual != child.old {
		return pageMutation{}, treeCorruption(step.id, page.TypeBTreeInternal, fmt.Sprintf("selected child is %d, expected %d", actual, child.old))
	}
	updated := cloneInternal(step.internal)
	setInternalChild(&updated, step.childIndex, child.left)
	if child.split() {
		entry := page.InternalEntry{Key: bytes.Clone(child.separator), RightChild: child.right}
		updated.Entries = append(updated.Entries, page.InternalEntry{})
		copy(updated.Entries[step.childIndex+1:], updated.Entries[step.childIndex:])
		updated.Entries[step.childIndex] = entry
	}

	builder, err := buildInternal(updated)
	if err == nil {
		payload, encodeErr := builder.Encode()
		if encodeErr != nil {
			return pageMutation{}, encodeErr
		}
		id, appendErr := appendInternal(ctx, appender, generation, payload)
		if appendErr != nil {
			return pageMutation{}, appendErr
		}
		return pageMutation{old: step.id, left: id}, nil
	}
	if !errors.Is(err, dberrors.ErrResourceLimit) {
		return pageMutation{}, err
	}
	left, right, separator, err := chooseInternalSplit(updated)
	if err != nil {
		return pageMutation{}, err
	}
	rightPayload, err := right.Encode()
	if err != nil {
		return pageMutation{}, err
	}
	rightID, err := appendInternal(ctx, appender, generation, rightPayload)
	if err != nil {
		return pageMutation{}, err
	}
	if err := contextError(ctx, "btree.split_internal"); err != nil {
		return pageMutation{}, err
	}
	leftPayload, err := left.Encode()
	if err != nil {
		return pageMutation{}, err
	}
	leftID, err := appendInternal(ctx, appender, generation, leftPayload)
	if err != nil {
		return pageMutation{}, err
	}
	return pageMutation{old: step.id, left: leftID, right: rightID, separator: separator}, nil
}

func chooseInternalSplit(internal page.Internal) (*page.InternalBuilder, *page.InternalBuilder, []byte, error) {
	bestDifference := int(^uint(0) >> 1)
	var bestLeft, bestRight *page.InternalBuilder
	var bestSeparator []byte
	for promoted := 1; promoted+1 < len(internal.Entries); promoted++ {
		leftNode := page.Internal{FirstChild: internal.FirstChild, Entries: cloneInternalEntries(internal.Entries[:promoted])}
		rightNode := page.Internal{FirstChild: internal.Entries[promoted].RightChild, Entries: cloneInternalEntries(internal.Entries[promoted+1:])}
		left, leftErr := buildInternal(leftNode)
		right, rightErr := buildInternal(rightNode)
		if leftErr != nil || rightErr != nil {
			continue
		}
		difference := left.UsedBytes() - right.UsedBytes()
		if difference < 0 {
			difference = -difference
		}
		if difference < bestDifference {
			bestDifference = difference
			bestLeft, bestRight = left, right
			bestSeparator = bytes.Clone(internal.Entries[promoted].Key)
		}
	}
	if bestLeft == nil {
		return nil, nil, nil, dberrors.New(dberrors.CodeResourceLimit, "btree.split_internal", "internal entries cannot be split into two pages")
	}
	return bestLeft, bestRight, bestSeparator, nil
}

func appendNewRoot(ctx context.Context, appender PageAppender, generation uint64, child pageMutation) (page.ID, error) {
	builder := page.NewInternalBuilder(child.left)
	if err := builder.Insert(0, child.separator, child.right); err != nil {
		return 0, err
	}
	payload, err := builder.Encode()
	if err != nil {
		return 0, err
	}
	return appendInternal(ctx, appender, generation, payload)
}

func buildInternal(internal page.Internal) (*page.InternalBuilder, error) {
	builder := page.NewInternalBuilder(internal.FirstChild)
	for _, entry := range internal.Entries {
		if err := builder.Insert(builder.Count(), entry.Key, entry.RightChild); err != nil {
			return nil, err
		}
	}
	return builder, nil
}

func cloneInternal(internal page.Internal) page.Internal {
	return page.Internal{FirstChild: internal.FirstChild, Entries: cloneInternalEntries(internal.Entries)}
}

func cloneInternalEntries(entries []page.InternalEntry) []page.InternalEntry {
	cloned := make([]page.InternalEntry, len(entries))
	for index, entry := range entries {
		cloned[index] = page.InternalEntry{Key: bytes.Clone(entry.Key), RightChild: entry.RightChild}
	}
	return cloned
}

func setInternalChild(internal *page.Internal, index int, child page.ID) {
	if index == 0 {
		internal.FirstChild = child
		return
	}
	internal.Entries[index-1].RightChild = child
}

func appendInternal(ctx context.Context, appender PageAppender, generation uint64, payload []byte) (page.ID, error) {
	if err := contextError(ctx, "btree.append_internal"); err != nil {
		return 0, err
	}
	id, err := appender.Append(page.TypeBTreeInternal, generation, payload)
	if err != nil {
		return 0, err
	}
	if id < minimumTreePage {
		return 0, dberrors.New(dberrors.CodeCorruption, "btree.append_internal", fmt.Sprintf("appender returned reserved page ID %d", id))
	}
	return id, nil
}
