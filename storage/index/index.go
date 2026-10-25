// Package index maintains immutable secondary-index B+ tree roots.
package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/page"
	"yodadb/types"
)

// PageStore is the index access method's immutable-page boundary.
type PageStore interface {
	btree.PageSource
	btree.PageAppender
}

// Definition describes index-key column ordinals and uniqueness.
type Definition struct {
	Columns []int
	Unique  bool
}

// Tree is an immutable secondary-index root view.
type Tree struct {
	source btree.PageSource
	root   page.ID
	def    Definition
}

// Open validates an index definition and opens one immutable root.
func Open(source btree.PageSource, root page.ID, definition Definition) (*Tree, error) {
	if len(definition.Columns) == 0 {
		return nil, dberrors.New(dberrors.CodeConstraint, "index.open", "index requires at least one column")
	}
	seen := make(map[int]struct{}, len(definition.Columns))
	for _, ordinal := range definition.Columns {
		if ordinal < 0 {
			return nil, dberrors.New(dberrors.CodeConstraint, "index.open", "column ordinal cannot be negative")
		}
		if _, duplicate := seen[ordinal]; duplicate {
			return nil, dberrors.New(dberrors.CodeConstraint, "index.open", "index repeats a column ordinal")
		}
		seen[ordinal] = struct{}{}
	}
	if _, err := btree.Open(source, root); err != nil {
		return nil, err
	}
	definition.Columns = append([]int(nil), definition.Columns...)
	return &Tree{source: source, root: root, def: definition}, nil
}

// Root returns this immutable index view's root page ID.
func (t *Tree) Root() page.ID { return t.root }

// Insert adds one row-to-table-key mapping through copy-on-write.
func (t *Tree) Insert(ctx context.Context, store PageStore, generation uint64, row types.Row, tableKey []byte) (page.ID, error) {
	if store == nil || len(tableKey) == 0 {
		return 0, dberrors.New(dberrors.CodeConstraint, "index.insert", "page store and table key are required")
	}
	key, exactUnique, err := t.entryKey(row, tableKey)
	if err != nil {
		return 0, err
	}
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return 0, err
	}
	if exactUnique {
		if _, found, err := tree.Get(ctx, key); err != nil {
			return 0, err
		} else if found {
			return 0, dberrors.New(dberrors.CodeConstraint, "index.insert", "duplicate unique index key")
		}
	}
	return tree.Insert(ctx, store, generation, key, tableKey)
}

// Delete tombstones one row-to-table-key mapping.
func (t *Tree) Delete(ctx context.Context, store PageStore, generation uint64, row types.Row, tableKey []byte) (page.ID, bool, error) {
	key, _, err := t.entryKey(row, tableKey)
	if err != nil {
		return 0, false, err
	}
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return 0, false, err
	}
	return tree.Delete(ctx, store, generation, key)
}

// Lookup returns table keys whose full indexed-column values equal values.
func (t *Tree) Lookup(ctx context.Context, values types.Row) ([][]byte, error) {
	if len(values) != len(t.def.Columns) {
		return nil, dberrors.New(dberrors.CodeConstraint, "index.lookup", "lookup value count does not match index")
	}
	prefix, err := types.EncodeKey(values, types.KeyLimits{})
	if err != nil {
		return nil, err
	}
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return nil, err
	}
	if t.def.Unique && !containsNull(values) {
		value, found, err := tree.Get(ctx, prefix)
		if err != nil || !found {
			return nil, err
		}
		return [][]byte{value}, nil
	}
	cursor, err := tree.Seek(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var keys [][]byte
	for {
		key, value, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) || err == nil && !bytes.HasPrefix(key, prefix) {
			return keys, nil
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, bytes.Clone(value))
	}
}

// ScanRange returns table keys from the half-open encoded index-key interval
// [start, end). It is used by the rule planner for one-column range scans.
func (t *Tree) ScanRange(ctx context.Context, start, end []byte) ([][]byte, error) {
	tree, err := btree.Open(t.source, t.root)
	if err != nil {
		return nil, err
	}
	cursor, err := tree.Range(ctx, start, end)
	if err != nil {
		return nil, err
	}
	var keys [][]byte
	for {
		_, value, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return keys, nil
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, bytes.Clone(value))
	}
}

// EncodeUniqueKey returns the canonical logical key protected by a unique
// index. applicable is false for non-unique indexes and for rows containing a
// NULL indexed value, because SQL permits multiple such entries.
func EncodeUniqueKey(definition Definition, row types.Row) (key []byte, applicable bool, err error) {
	if !definition.Unique {
		return nil, false, nil
	}
	values := make(types.Row, len(definition.Columns))
	seen := make(map[int]struct{}, len(definition.Columns))
	for index, ordinal := range definition.Columns {
		if ordinal < 0 || ordinal >= len(row) {
			return nil, false, dberrors.New(dberrors.CodeConstraint, "index.unique_key", fmt.Sprintf("column ordinal %d outside row width %d", ordinal, len(row)))
		}
		if _, duplicate := seen[ordinal]; duplicate {
			return nil, false, dberrors.New(dberrors.CodeConstraint, "index.unique_key", "index repeats a column ordinal")
		}
		seen[ordinal] = struct{}{}
		values[index] = row[ordinal]
	}
	if len(values) == 0 {
		return nil, false, dberrors.New(dberrors.CodeConstraint, "index.unique_key", "index requires at least one column")
	}
	if containsNull(values) {
		return nil, false, nil
	}
	key, err = types.EncodeKey(values, types.KeyLimits{})
	return key, err == nil, err
}

func (t *Tree) entryKey(row types.Row, tableKey []byte) ([]byte, bool, error) {
	values := make(types.Row, len(t.def.Columns), len(t.def.Columns)+1)
	for index, ordinal := range t.def.Columns {
		if ordinal >= len(row) {
			return nil, false, dberrors.New(dberrors.CodeConstraint, "index.key", fmt.Sprintf("column ordinal %d outside row width %d", ordinal, len(row)))
		}
		values[index] = row[ordinal]
	}
	exactUnique := t.def.Unique && !containsNull(values)
	if !exactUnique {
		values = append(values, types.Bytes(tableKey))
	}
	key, err := types.EncodeKey(values, types.KeyLimits{})
	return key, exactUnique, err
}

func containsNull(values types.Row) bool {
	for _, value := range values {
		if value.IsNull() {
			return true
		}
	}
	return false
}
