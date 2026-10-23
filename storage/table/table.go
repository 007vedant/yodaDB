// Package table stores versioned tuples in immutable B+ tree roots.
package table

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/page"
	"yodadb/types"
)

// PageStore is the table access method's immutable-page boundary.
type PageStore interface {
	btree.PageSource
	btree.PageAppender
}

// Record is one physical table key and decoded logical row.
type Record struct {
	Key []byte
	Row types.Row
}

// Tree is a schema-aware view of one immutable table root.
type Tree struct {
	source          btree.PageSource
	root            page.ID
	schemaVersion   uint64
	primaryKeyIndex int
	columnCount     int
}

// Open validates table access metadata and opens an immutable root view.
func Open(source btree.PageSource, root page.ID, schemaVersion uint64, primaryKeyIndex, columnCount int) (*Tree, error) {
	if schemaVersion == 0 || columnCount <= 0 || primaryKeyIndex < -1 || primaryKeyIndex >= columnCount {
		return nil, dberrors.New(dberrors.CodeConstraint, "table.open", "invalid table schema metadata")
	}
	if _, err := btree.Open(source, root); err != nil {
		return nil, err
	}
	return &Tree{source: source, root: root, schemaVersion: schemaVersion, primaryKeyIndex: primaryKeyIndex, columnCount: columnCount}, nil
}

// Root returns this immutable table view's B+ tree root.
func (t *Tree) Root() page.ID { return t.root }

// Insert appends a row through copy-on-write and returns its new root and
// physical key. hiddenRowID is used only when the table has no declared primary
// key and must be positive and representable as INT64.
func (t *Tree) Insert(ctx context.Context, store PageStore, generation, hiddenRowID uint64, row types.Row) (page.ID, []byte, error) {
	if store == nil {
		return 0, nil, dberrors.New(dberrors.CodeConstraint, "table.insert", "page store cannot be nil")
	}
	if len(row) != t.columnCount {
		return 0, nil, dberrors.New(dberrors.CodeConstraint, "table.insert", "row width does not match table schema")
	}
	key, err := t.keyForRow(row, hiddenRowID)
	if err != nil {
		return 0, nil, err
	}
	value, err := types.EncodeTuple(t.schemaVersion, row, types.TupleLimits{})
	if err != nil {
		return 0, nil, err
	}
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return 0, nil, err
	}
	root, err := tree.Insert(ctx, store, generation, key, value)
	if err != nil {
		return 0, nil, err
	}
	return root, key, nil
}

// Lookup returns the live row stored under an encoded physical key.
func (t *Tree) Lookup(ctx context.Context, key []byte) (types.Row, bool, error) {
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return nil, false, err
	}
	value, found, err := tree.Get(ctx, key)
	if err != nil || !found {
		return nil, found, err
	}
	row, err := t.decodeRow(value)
	return row, err == nil, err
}

// Scan materializes all live records in physical-key order.
func (t *Tree) Scan(ctx context.Context) ([]Record, error) {
	return t.ScanRange(ctx, nil, nil)
}

// ScanRange materializes live records in the half-open physical-key interval
// [start, end). Nil bounds are unbounded.
func (t *Tree) ScanRange(ctx context.Context, start, end []byte) ([]Record, error) {
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return nil, err
	}
	cursor, err := tree.Range(ctx, start, end)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for {
		key, value, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		row, err := t.decodeRow(value)
		if err != nil {
			return nil, err
		}
		records = append(records, Record{Key: key, Row: row})
	}
}

// Delete tombstones one physical key without merging pages.
func (t *Tree) Delete(ctx context.Context, store PageStore, generation uint64, key []byte) (page.ID, bool, error) {
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return 0, false, err
	}
	return tree.Delete(ctx, store, generation, key)
}

// Replace copy-on-write deletes oldKey and inserts replacement. A failure leaves
// the caller's original root valid; any appended pages remain unreachable.
func (t *Tree) Replace(ctx context.Context, store PageStore, generation, hiddenRowID uint64, oldKey []byte, replacement types.Row) (page.ID, []byte, error) {
	deletedRoot, deleted, err := t.Delete(ctx, store, generation, oldKey)
	if err != nil {
		return 0, nil, err
	}
	if !deleted {
		return t.root, nil, dberrors.New(dberrors.CodeConstraint, "table.replace", "row no longer exists")
	}
	deletedTree, err := Open(store, deletedRoot, t.schemaVersion, t.primaryKeyIndex, t.columnCount)
	if err != nil {
		return 0, nil, err
	}
	return deletedTree.Insert(ctx, store, generation, hiddenRowID, replacement)
}

// EncodePrimaryKey returns a table's physical key for one declared primary-key
// value. It is invalid for hidden-RowID tables.
func (t *Tree) EncodePrimaryKey(value types.Value) ([]byte, error) {
	if t.primaryKeyIndex < 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "table.primary_key", "table uses a hidden RowID")
	}
	if value.IsNull() {
		return nil, dberrors.New(dberrors.CodeConstraint, "table.primary_key", "primary key cannot be NULL")
	}
	return types.EncodeKey(types.Row{value}, types.KeyLimits{})
}

func (t *Tree) keyForRow(row types.Row, hiddenRowID uint64) ([]byte, error) {
	if t.primaryKeyIndex >= 0 {
		return t.EncodePrimaryKey(row[t.primaryKeyIndex])
	}
	if hiddenRowID == 0 || hiddenRowID > math.MaxInt64 {
		return nil, dberrors.New(dberrors.CodeConstraint, "table.hidden_rowid", "hidden RowID must be a positive INT64")
	}
	return types.EncodeKey(types.Row{types.Int64(int64(hiddenRowID))}, types.KeyLimits{})
}

func (t *Tree) decodeRow(value []byte) (types.Row, error) {
	version, row, err := types.DecodeTuple(value, types.TupleLimits{})
	if err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeCorruption, "table.decode", "invalid stored tuple")
	}
	if version != t.schemaVersion {
		return nil, dberrors.New(dberrors.CodeCorruption, "table.decode", fmt.Sprintf("tuple schema version %d does not match table version %d", version, t.schemaVersion))
	}
	if len(row) != t.columnCount {
		return nil, dberrors.New(dberrors.CodeCorruption, "table.decode", fmt.Sprintf("tuple has %d columns, expected %d", len(row), t.columnCount))
	}
	return row, nil
}
