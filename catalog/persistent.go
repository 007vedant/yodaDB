package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"unicode/utf8"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/page"
	"yodadb/types"
)

const (
	catalogDescriptorVersion = uint64(1)
	catalogObjectTable       = int64(1)
	catalogObjectIndex       = int64(2)
	catalogObjectSnapshot    = int64(3)
)

// PageStore is the catalog's narrow boundary to immutable B+ tree storage.
type PageStore interface {
	btree.PageSource
	btree.PageAppender
}

// Save writes a complete immutable catalog snapshot and returns its unpublished
// B+ tree root. Existing roots are not changed.
func Save(ctx context.Context, store PageStore, generation uint64, schema *Catalog) (page.ID, error) {
	if store == nil || schema == nil {
		return 0, dberrors.New(dberrors.CodeConstraint, "catalog.save", "store and catalog are required")
	}
	tree, err := btree.Open(store, 0)
	if err != nil {
		return 0, err
	}
	for _, table := range schema.Tables() {
		key, err := catalogKey(catalogObjectTable, uint64(table.ID))
		if err != nil {
			return 0, err
		}
		value, err := encodeTableDescriptor(table)
		if err != nil {
			return 0, err
		}
		root, err := tree.Insert(ctx, store, generation, key, value)
		if err != nil {
			return 0, err
		}
		tree, err = btree.Open(store, root)
		if err != nil {
			return 0, err
		}
	}
	for _, index := range schema.Indexes() {
		key, err := catalogKey(catalogObjectIndex, uint64(index.ID))
		if err != nil {
			return 0, err
		}
		value, err := encodeIndexDescriptor(index)
		if err != nil {
			return 0, err
		}
		root, err := tree.Insert(ctx, store, generation, key, value)
		if err != nil {
			return 0, err
		}
		tree, err = btree.Open(store, root)
		if err != nil {
			return 0, err
		}
	}
	for _, snapshot := range schema.Snapshots() {
		key, err := catalogKey(catalogObjectSnapshot, uint64(snapshot.ID))
		if err != nil {
			return 0, err
		}
		value, err := encodeSnapshotDescriptor(snapshot)
		if err != nil {
			return 0, err
		}
		root, err := tree.Insert(ctx, store, generation, key, value)
		if err != nil {
			return 0, err
		}
		tree, err = btree.Open(store, root)
		if err != nil {
			return 0, err
		}
	}
	return tree.Root(), nil
}

// Load reconstructs a catalog snapshot from a B+ tree root.
func Load(ctx context.Context, source btree.PageSource, root page.ID) (*Catalog, error) {
	tree, err := btree.Open(source, root)
	if err != nil {
		return nil, err
	}
	cursor, err := tree.Seek(ctx, nil)
	if err != nil {
		return nil, err
	}
	var tables []Table
	var indexes []Index
	var snapshots []NamedSnapshot
	for {
		key, value, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		kind, id, err := decodeCatalogKey(key)
		if err != nil {
			return nil, err
		}
		switch kind {
		case catalogObjectTable:
			table, err := decodeTableDescriptor(value)
			if err != nil {
				return nil, err
			}
			if uint64(table.ID) != id {
				return nil, catalogCorruption("table key ID does not match descriptor")
			}
			tables = append(tables, table)
		case catalogObjectIndex:
			index, err := decodeIndexDescriptor(value)
			if err != nil {
				return nil, err
			}
			if uint64(index.ID) != id {
				return nil, catalogCorruption("index key ID does not match descriptor")
			}
			indexes = append(indexes, index)
		case catalogObjectSnapshot:
			snapshot, err := decodeSnapshotDescriptor(value)
			if err != nil {
				return nil, err
			}
			if uint64(snapshot.ID) != id {
				return nil, catalogCorruption("snapshot key ID does not match descriptor")
			}
			snapshots = append(snapshots, snapshot)
		default:
			return nil, catalogCorruption(fmt.Sprintf("unknown catalog object kind %d", kind))
		}
	}
	return catalogFromDescriptors(tables, indexes, snapshots)
}

func encodeSnapshotDescriptor(snapshot NamedSnapshot) ([]byte, error) {
	txnID := make([]byte, 8)
	binary.LittleEndian.PutUint64(txnID, snapshot.TxnID)
	return types.EncodeTuple(catalogDescriptorVersion, types.Row{
		types.Int64(int64(snapshot.ID)), types.Text(snapshot.Name), types.Bytes(txnID),
	}, types.TupleLimits{})
}

func decodeSnapshotDescriptor(encoded []byte) (NamedSnapshot, error) {
	version, row, err := types.DecodeTuple(encoded, types.TupleLimits{})
	if err != nil {
		return NamedSnapshot{}, dberrors.Wrap(err, dberrors.CodeCorruption, "catalog.decode_snapshot", "invalid snapshot descriptor tuple")
	}
	if version != catalogDescriptorVersion || len(row) != 3 {
		return NamedSnapshot{}, catalogCorruption("invalid snapshot descriptor header")
	}
	id, idOK := positiveInt(row[0])
	name, nameOK := row[1].TextValue()
	txnID, txnOK := row[2].BytesValue()
	if !idOK || !nameOK || name == "" || len(name) > 255 || !txnOK || len(txnID) != 8 {
		return NamedSnapshot{}, catalogCorruption("invalid snapshot descriptor fields")
	}
	target := binary.LittleEndian.Uint64(txnID)
	if target == 0 {
		return NamedSnapshot{}, catalogCorruption("snapshot transaction ID must be nonzero")
	}
	return NamedSnapshot{ID: SnapshotID(id), Name: name, TxnID: target}, nil
}

func catalogKey(kind int64, id uint64) ([]byte, error) {
	if id == 0 || id > math.MaxInt64 {
		return nil, dberrors.New(dberrors.CodeConstraint, "catalog.key", "object ID is outside persisted range")
	}
	return types.EncodeKey(types.Row{types.Int64(kind), types.Int64(int64(id))}, types.KeyLimits{})
}

func decodeCatalogKey(encoded []byte) (int64, uint64, error) {
	row, err := types.DecodeKey(encoded, types.KeyLimits{})
	if err != nil {
		return 0, 0, dberrors.Wrap(err, dberrors.CodeCorruption, "catalog.decode_key", "invalid catalog key")
	}
	if len(row) != 2 {
		return 0, 0, catalogCorruption("catalog key must have two components")
	}
	kind, kindOK := row[0].Int64Value()
	id, idOK := row[1].Int64Value()
	if !kindOK || !idOK || id <= 0 {
		return 0, 0, catalogCorruption("catalog key components must be positive INT64 values")
	}
	return kind, uint64(id), nil
}

func encodeTableDescriptor(table Table) ([]byte, error) {
	row := types.Row{
		types.Int64(int64(table.ID)), types.Text(table.Name), types.Int64(int64(table.SchemaVersion)),
		types.Int64(int64(table.PrimaryKeyIndex)), types.Int64(int64(len(table.Columns))),
	}
	for _, column := range table.Columns {
		row = append(row,
			types.Int64(int64(column.ID)), types.Text(column.Name), types.Int64(int64(column.Type)),
			types.Int64(int64(column.Ordinal)), types.Bool(column.NotNull), types.Bool(column.PrimaryKey),
		)
	}
	return types.EncodeTuple(catalogDescriptorVersion, row, types.TupleLimits{})
}

func decodeTableDescriptor(encoded []byte) (Table, error) {
	version, row, err := types.DecodeTuple(encoded, types.TupleLimits{})
	if err != nil {
		return Table{}, dberrors.Wrap(err, dberrors.CodeCorruption, "catalog.decode_table", "invalid table descriptor tuple")
	}
	if version != catalogDescriptorVersion || len(row) < 5 {
		return Table{}, catalogCorruption("invalid table descriptor header")
	}
	id, ok := positiveInt(row[0])
	name, nameOK := row[1].TextValue()
	schemaVersion, schemaOK := positiveInt(row[2])
	primary, primaryOK := row[3].Int64Value()
	columnCount, countOK := row[4].Int64Value()
	if !ok || !nameOK || name == "" || !schemaOK || !primaryOK || !countOK || columnCount <= 0 || columnCount > 1024 || len(row) != 5+int(columnCount)*6 {
		return Table{}, catalogCorruption("invalid table descriptor fields")
	}
	table := Table{ID: TableID(id), Name: name, SchemaVersion: uint64(schemaVersion), PrimaryKeyIndex: int(primary), Columns: make([]Column, int(columnCount))}
	for index := range table.Columns {
		offset := 5 + index*6
		columnID, idOK := positiveInt(row[offset])
		columnName, columnNameOK := row[offset+1].TextValue()
		kind, kindOK := row[offset+2].Int64Value()
		ordinal, ordinalOK := row[offset+3].Int64Value()
		notNull, notNullOK := row[offset+4].BoolValue()
		primaryKey, primaryKeyOK := row[offset+5].BoolValue()
		if !idOK || !columnNameOK || columnName == "" || !kindOK || kind <= int64(types.NullKind) || kind > int64(types.BytesKind) ||
			!ordinalOK || ordinal != int64(index) || !notNullOK || !primaryKeyOK {
			return Table{}, catalogCorruption(fmt.Sprintf("invalid table column %d", index))
		}
		table.Columns[index] = Column{ID: ColumnID(columnID), Name: columnName, Type: types.Kind(kind), Ordinal: index, NotNull: notNull, PrimaryKey: primaryKey}
	}
	return table, nil
}

func encodeIndexDescriptor(index Index) ([]byte, error) {
	row := types.Row{
		types.Int64(int64(index.ID)), types.Text(index.Name), types.Int64(int64(index.TableID)),
		types.Bool(index.Unique), types.Int64(int64(len(index.Columns))),
	}
	for _, columnID := range index.Columns {
		row = append(row, types.Int64(int64(columnID)))
	}
	return types.EncodeTuple(catalogDescriptorVersion, row, types.TupleLimits{})
}

func decodeIndexDescriptor(encoded []byte) (Index, error) {
	version, row, err := types.DecodeTuple(encoded, types.TupleLimits{})
	if err != nil {
		return Index{}, dberrors.Wrap(err, dberrors.CodeCorruption, "catalog.decode_index", "invalid index descriptor tuple")
	}
	if version != catalogDescriptorVersion || len(row) < 5 {
		return Index{}, catalogCorruption("invalid index descriptor header")
	}
	id, idOK := positiveInt(row[0])
	name, nameOK := row[1].TextValue()
	tableID, tableOK := positiveInt(row[2])
	unique, uniqueOK := row[3].BoolValue()
	columnCount, countOK := row[4].Int64Value()
	if !idOK || !nameOK || name == "" || !tableOK || !uniqueOK || !countOK || columnCount <= 0 || columnCount > 1024 || len(row) != 5+int(columnCount) {
		return Index{}, catalogCorruption("invalid index descriptor fields")
	}
	index := Index{ID: IndexID(id), Name: name, TableID: TableID(tableID), Unique: unique, Columns: make([]ColumnID, int(columnCount))}
	for column := range index.Columns {
		columnID, ok := positiveInt(row[5+column])
		if !ok {
			return Index{}, catalogCorruption(fmt.Sprintf("invalid index column %d", column))
		}
		index.Columns[column] = ColumnID(columnID)
	}
	return index, nil
}

func catalogFromDescriptors(tables []Table, indexes []Index, snapshots []NamedSnapshot) (*Catalog, error) {
	schema := New()
	sort.Slice(tables, func(left, right int) bool { return tables[left].ID < tables[right].ID })
	for _, table := range tables {
		if err := schema.installTable(table); err != nil {
			return nil, err
		}
	}
	sort.Slice(indexes, func(left, right int) bool { return indexes[left].ID < indexes[right].ID })
	for _, index := range indexes {
		if err := schema.installIndex(index); err != nil {
			return nil, err
		}
	}
	sort.Slice(snapshots, func(left, right int) bool { return snapshots[left].ID < snapshots[right].ID })
	for _, snapshot := range snapshots {
		if err := schema.installSnapshot(snapshot); err != nil {
			return nil, err
		}
	}
	return schema, nil
}

func (c *Catalog) installSnapshot(snapshot NamedSnapshot) error {
	if snapshot.ID == 0 || snapshot.Name == "" || len(snapshot.Name) > 255 || !utf8.ValidString(snapshot.Name) || snapshot.TxnID == 0 {
		return catalogCorruption("invalid persisted snapshot descriptor")
	}
	if _, duplicate := c.snapshots[snapshot.ID]; duplicate {
		return catalogCorruption("duplicate persisted snapshot ID")
	}
	if _, duplicate := c.snapshotNames[snapshot.Name]; duplicate {
		return catalogCorruption("duplicate persisted snapshot name")
	}
	c.snapshots[snapshot.ID] = snapshot
	c.snapshotNames[snapshot.Name] = snapshot.ID
	if snapshot.ID >= c.nextSnapshotID {
		c.nextSnapshotID = snapshot.ID + 1
	}
	return nil
}

func (c *Catalog) installTable(table Table) error {
	if table.ID == 0 || table.Name == "" || table.SchemaVersion == 0 || len(table.Columns) == 0 || table.PrimaryKeyIndex < -1 || table.PrimaryKeyIndex >= len(table.Columns) {
		return catalogCorruption("invalid persisted table descriptor")
	}
	if _, duplicate := c.tables[table.ID]; duplicate {
		return catalogCorruption("duplicate persisted table ID")
	}
	if _, duplicate := c.tableNames[table.Name]; duplicate {
		return catalogCorruption("duplicate persisted table name")
	}
	seenColumns := make(map[ColumnID]struct{}, len(table.Columns))
	seenNames := make(map[string]struct{}, len(table.Columns))
	for ordinal, column := range table.Columns {
		if column.ID == 0 || column.Name == "" || column.Ordinal != ordinal || column.Type <= types.NullKind || column.Type > types.BytesKind {
			return catalogCorruption("invalid persisted column descriptor")
		}
		if column.PrimaryKey != (ordinal == table.PrimaryKeyIndex) || column.PrimaryKey && !column.NotNull {
			return catalogCorruption("persisted primary-key metadata disagrees")
		}
		if _, duplicate := seenColumns[column.ID]; duplicate {
			return catalogCorruption("duplicate persisted column ID")
		}
		if _, duplicate := seenNames[column.Name]; duplicate {
			return catalogCorruption("duplicate persisted column name")
		}
		seenColumns[column.ID] = struct{}{}
		seenNames[column.Name] = struct{}{}
		if column.ID >= c.nextColumnID {
			c.nextColumnID = column.ID + 1
		}
	}
	c.tables[table.ID] = cloneTable(table)
	c.tableNames[table.Name] = table.ID
	if table.ID >= c.nextTableID {
		c.nextTableID = table.ID + 1
	}
	return nil
}

func (c *Catalog) installIndex(index Index) error {
	table, ok := c.tables[index.TableID]
	if index.ID == 0 || index.Name == "" || !ok || len(index.Columns) == 0 {
		return catalogCorruption("invalid persisted index descriptor")
	}
	if _, duplicate := c.indexes[index.ID]; duplicate {
		return catalogCorruption("duplicate persisted index ID")
	}
	names := c.indexNames[index.TableID]
	if names == nil {
		names = make(map[string]IndexID)
	}
	if _, duplicate := names[index.Name]; duplicate {
		return catalogCorruption("duplicate persisted index name")
	}
	columns := make(map[ColumnID]struct{}, len(table.Columns))
	for _, column := range table.Columns {
		columns[column.ID] = struct{}{}
	}
	seen := make(map[ColumnID]struct{}, len(index.Columns))
	for _, columnID := range index.Columns {
		if _, exists := columns[columnID]; !exists {
			return catalogCorruption("persisted index references unknown column")
		}
		if _, duplicate := seen[columnID]; duplicate {
			return catalogCorruption("persisted index repeats a column")
		}
		seen[columnID] = struct{}{}
	}
	names[index.Name] = index.ID
	c.indexNames[index.TableID] = names
	c.indexes[index.ID] = cloneIndex(index)
	if index.ID >= c.nextIndexID {
		c.nextIndexID = index.ID + 1
	}
	return nil
}

func positiveInt(value types.Value) (int64, bool) {
	number, ok := value.Int64Value()
	return number, ok && number > 0
}

func catalogCorruption(message string) error {
	return dberrors.New(dberrors.CodeCorruption, "catalog.decode", message)
}
