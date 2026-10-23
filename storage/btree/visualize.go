package btree

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

// WriteTree writes a deterministic, human-readable view of the tree's page
// hierarchy. Keys are rendered as their canonical encoded bytes in hexadecimal.
func (t *Tree) WriteTree(ctx context.Context, output io.Writer) error {
	if output == nil {
		return dberrors.New(dberrors.CodeConstraint, "btree.write_tree", "output writer cannot be nil")
	}
	if err := contextError(ctx, "btree.write_tree"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "tree root=%d\n", t.root); err != nil {
		return err
	}
	if t.root == emptyRoot {
		_, err := fmt.Fprintln(output, "empty")
		return err
	}
	visited := make(map[page.ID]struct{})
	return t.writeTreeNode(ctx, output, t.root, "", "", visited, 0)
}

func (t *Tree) writeTreeNode(ctx context.Context, output io.Writer, id page.ID, prefix, connector string, visited map[page.ID]struct{}, depth int) error {
	if err := contextError(ctx, "btree.write_tree"); err != nil {
		return err
	}
	if depth >= maximumDepth {
		return treeCorruption(id, page.TypeBTreeInternal, fmt.Sprintf("tree depth exceeds %d", maximumDepth))
	}
	if id < minimumTreePage {
		return treeCorruption(id, page.TypeBTreeInternal, "child points to a reserved page")
	}
	if _, duplicate := visited[id]; duplicate {
		return treeCorruption(id, page.TypeBTreeInternal, "tree references a page more than once")
	}
	visited[id] = struct{}{}
	stored, err := t.source.ReadPage(id)
	if err != nil {
		return err
	}
	if stored.Header.PageID != id {
		return treeCorruption(id, stored.Header.Type, fmt.Sprintf("page declares ID %d", stored.Header.PageID))
	}

	childPrefix := prefix
	if connector != "" {
		childPrefix += "    "
	}
	switch stored.Header.Type {
	case page.TypeBTreeLeaf:
		leaf, err := decodeLeaf(id, stored.Payload)
		if err != nil {
			return err
		}
		live, tombstones := 0, 0
		for _, entry := range leaf.Entries {
			if entry.Tombstone {
				tombstones++
			} else {
				live++
			}
		}
		first, last := "-", "-"
		if len(leaf.Entries) > 0 {
			first = formatTreeKey(leaf.Entries[0].Key)
			last = formatTreeKey(leaf.Entries[len(leaf.Entries)-1].Key)
		}
		_, err = fmt.Fprintf(output, "%s%sleaf page=%d generation=%d entries=%d live=%d tombstones=%d right=%d keys=%s..%s\n",
			prefix, connector, id, stored.Header.Generation, len(leaf.Entries), live, tombstones, leaf.RightSibling, first, last)
		return err

	case page.TypeBTreeInternal:
		internal, err := page.DecodeInternal(stored.Payload)
		if err != nil {
			return dberrors.Wrap(err, dberrors.CodeCorruption, "btree.write_tree", fmt.Sprintf("internal page %d", id))
		}
		if _, err := fmt.Fprintf(output, "%s%sinternal page=%d generation=%d separators=%d\n",
			prefix, connector, id, stored.Header.Generation, len(internal.Entries)); err != nil {
			return err
		}
		for index := 0; index <= len(internal.Entries); index++ {
			lastChild := index == len(internal.Entries)
			edgeConnector := "├── "
			nestedPrefix := childPrefix + "│   "
			if lastChild {
				edgeConnector = "└── "
				nestedPrefix = childPrefix + "    "
			}
			if _, err := fmt.Fprintf(output, "%s%schild[%d] range=%s\n", childPrefix, edgeConnector, index, internalChildRange(internal, index)); err != nil {
				return err
			}
			if err := t.writeTreeNode(ctx, output, internalChild(internal, index), nestedPrefix, "└── ", visited, depth+1); err != nil {
				return err
			}
		}
		return nil

	default:
		return treeCorruption(id, stored.Header.Type, "tree path references a non-tree page")
	}
}

func internalChildRange(internal page.Internal, index int) string {
	switch {
	case index == 0:
		return fmt.Sprintf("(-inf,%s)", formatTreeKey(internal.Entries[0].Key))
	case index == len(internal.Entries):
		return fmt.Sprintf("[%s,+inf)", formatTreeKey(internal.Entries[index-1].Key))
	default:
		return fmt.Sprintf("[%s,%s)", formatTreeKey(internal.Entries[index-1].Key), formatTreeKey(internal.Entries[index].Key))
	}
}

func formatTreeKey(key []byte) string {
	return "0x" + hex.EncodeToString(key)
}
