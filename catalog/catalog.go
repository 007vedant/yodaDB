package catalog

import (
	"fmt"
	"sync"

	"yodadb/dberrors"
	"yodadb/types"
)

// TableID is a stable table identity independent of its name.
type TableID uint64

// ColumnID is a stable column identity independent of its name.
type ColumnID uint64

// IndexID is a stable secondary-index identity independent of its name.
type IndexID uint64

// Column describes one table column.
type Column struct {
	ID         ColumnID
	Name       string
	Type       types.Kind
	Ordinal    int
	NotNull    bool
	PrimaryKey bool
}

// Table is an immutable snapshot of a table descriptor.
type Table struct {
	ID              TableID
	Name            string
	SchemaVersion   uint64
	Columns         []Column
	PrimaryKeyIndex int
}

// Index is an immutable secondary-index descriptor. Columns contains stable
// column IDs in index-key order.
type Index struct {
	ID      IndexID
	Name    string
	TableID TableID
	Columns []ColumnID
	Unique  bool
}

// ColumnSpec contains validated-independent input for creating a column.
type ColumnSpec struct {
	Name       string
	Type       types.Kind
	NotNull    bool
	PrimaryKey bool
}

// Catalog is the M1 in-memory schema catalog.
type Catalog struct {
	mu           sync.RWMutex
	nextTableID  TableID
	nextColumnID ColumnID
	nextIndexID  IndexID
	tableNames   map[string]TableID
	tables       map[TableID]Table
	indexNames   map[TableID]map[string]IndexID
	indexes      map[IndexID]Index
}

// New creates an empty catalog.
func New() *Catalog {
	return &Catalog{
		nextTableID:  1,
		nextColumnID: 1,
		nextIndexID:  1,
		tableNames:   make(map[string]TableID),
		tables:       make(map[TableID]Table),
		indexNames:   make(map[TableID]map[string]IndexID),
		indexes:      make(map[IndexID]Index),
	}
}

// CreateIndex atomically creates an index descriptor after validating its
// table, name, and stable column IDs.
func (c *Catalog) CreateIndex(tableID TableID, name string, columns []ColumnID, unique bool) (Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	table, exists := c.tables[tableID]
	if !exists {
		return Index{}, dberrors.New(dberrors.CodeBind, "catalog.create_index", fmt.Sprintf("unknown table ID %d", tableID))
	}
	if name == "" {
		return Index{}, dberrors.New(dberrors.CodeBind, "catalog.create_index", "index name cannot be empty")
	}
	if len(columns) == 0 {
		return Index{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_index", "index must contain at least one column")
	}
	names := c.indexNames[tableID]
	if names == nil {
		names = make(map[string]IndexID)
	}
	if _, duplicate := names[name]; duplicate {
		return Index{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_index", fmt.Sprintf("index %q already exists on table %q", name, table.Name))
	}
	available := make(map[ColumnID]struct{}, len(table.Columns))
	for _, column := range table.Columns {
		available[column.ID] = struct{}{}
	}
	seen := make(map[ColumnID]struct{}, len(columns))
	for _, columnID := range columns {
		if _, ok := available[columnID]; !ok {
			return Index{}, dberrors.New(dberrors.CodeBind, "catalog.create_index", fmt.Sprintf("unknown column ID %d on table %q", columnID, table.Name))
		}
		if _, duplicate := seen[columnID]; duplicate {
			return Index{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_index", fmt.Sprintf("column ID %d appears more than once", columnID))
		}
		seen[columnID] = struct{}{}
	}
	index := Index{ID: c.nextIndexID, Name: name, TableID: tableID, Columns: append([]ColumnID(nil), columns...), Unique: unique}
	c.nextIndexID++
	names[name] = index.ID
	c.indexNames[tableID] = names
	c.indexes[index.ID] = index
	return cloneIndex(index), nil
}

// CreateTable atomically creates a descriptor after validating the entire
// definition. Failed creation does not consume stable IDs.
func (c *Catalog) CreateTable(name string, columns []ColumnSpec) (Table, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if name == "" {
		return Table{}, dberrors.New(dberrors.CodeBind, "catalog.create_table", "table name cannot be empty")
	}
	if _, exists := c.tableNames[name]; exists {
		return Table{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_table", fmt.Sprintf("table %q already exists", name))
	}
	if len(columns) == 0 {
		return Table{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_table", "table must have at least one column")
	}

	seenNames := make(map[string]struct{}, len(columns))
	primaryKeyIndex := -1
	for index, column := range columns {
		if column.Name == "" {
			return Table{}, dberrors.New(dberrors.CodeBind, "catalog.create_table", "column name cannot be empty")
		}
		if _, exists := seenNames[column.Name]; exists {
			return Table{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_table", fmt.Sprintf("duplicate column %q", column.Name))
		}
		seenNames[column.Name] = struct{}{}
		if column.Type == types.NullKind || column.Type > types.BytesKind {
			return Table{}, dberrors.New(dberrors.CodeType, "catalog.create_table", fmt.Sprintf("column %q has invalid type %s", column.Name, column.Type))
		}
		if column.PrimaryKey {
			if primaryKeyIndex >= 0 {
				return Table{}, dberrors.New(dberrors.CodeConstraint, "catalog.create_table", "M1 supports one primary-key column")
			}
			primaryKeyIndex = index
		}
	}

	table := Table{
		ID:              c.nextTableID,
		Name:            name,
		SchemaVersion:   1,
		Columns:         make([]Column, len(columns)),
		PrimaryKeyIndex: primaryKeyIndex,
	}
	nextColumnID := c.nextColumnID
	for index, spec := range columns {
		table.Columns[index] = Column{
			ID:         nextColumnID,
			Name:       spec.Name,
			Type:       spec.Type,
			Ordinal:    index,
			NotNull:    spec.NotNull || spec.PrimaryKey,
			PrimaryKey: spec.PrimaryKey,
		}
		nextColumnID++
	}

	c.nextTableID++
	c.nextColumnID = nextColumnID
	c.tableNames[name] = table.ID
	c.tables[table.ID] = table
	return cloneTable(table), nil
}

// LookupTable resolves a canonical table name and returns a descriptor copy.
func (c *Catalog) LookupTable(name string) (Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.tableNames[name]
	if !ok {
		return Table{}, false
	}
	return cloneTable(c.tables[id]), true
}

// TableByID resolves a stable table identity and returns a descriptor copy.
func (c *Catalog) TableByID(id TableID) (Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	table, ok := c.tables[id]
	if !ok {
		return Table{}, false
	}
	return cloneTable(table), true
}

// Tables returns descriptor copies ordered by stable table ID.
func (c *Catalog) Tables() []Table {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tables := make([]Table, 0, len(c.tables))
	for id := TableID(1); id < c.nextTableID; id++ {
		if table, ok := c.tables[id]; ok {
			tables = append(tables, cloneTable(table))
		}
	}
	return tables
}

// LookupIndex resolves an index name within one table.
func (c *Catalog) LookupIndex(tableID TableID, name string) (Index, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.indexNames[tableID][name]
	if !ok {
		return Index{}, false
	}
	return cloneIndex(c.indexes[id]), true
}

// IndexByID resolves a stable index identity.
func (c *Catalog) IndexByID(id IndexID) (Index, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	index, ok := c.indexes[id]
	if !ok {
		return Index{}, false
	}
	return cloneIndex(index), true
}

// Indexes returns descriptor copies ordered by stable index ID.
func (c *Catalog) Indexes() []Index {
	c.mu.RLock()
	defer c.mu.RUnlock()
	indexes := make([]Index, 0, len(c.indexes))
	for id := IndexID(1); id < c.nextIndexID; id++ {
		if index, ok := c.indexes[id]; ok {
			indexes = append(indexes, cloneIndex(index))
		}
	}
	return indexes
}

// IndexesForTable returns a table's indexes ordered by stable index ID.
func (c *Catalog) IndexesForTable(tableID TableID) []Index {
	indexes := c.Indexes()
	result := indexes[:0]
	for _, index := range indexes {
		if index.TableID == tableID {
			result = append(result, index)
		}
	}
	return result
}

// Clone returns an independent mutable catalog with the same stable IDs and
// descriptors.
func (c *Catalog) Clone() *Catalog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cloned := New()
	cloned.nextTableID = c.nextTableID
	cloned.nextColumnID = c.nextColumnID
	cloned.nextIndexID = c.nextIndexID
	for name, id := range c.tableNames {
		cloned.tableNames[name] = id
	}
	for id, table := range c.tables {
		cloned.tables[id] = cloneTable(table)
	}
	for tableID, names := range c.indexNames {
		cloned.indexNames[tableID] = make(map[string]IndexID, len(names))
		for name, id := range names {
			cloned.indexNames[tableID][name] = id
		}
	}
	for id, index := range c.indexes {
		cloned.indexes[id] = cloneIndex(index)
	}
	return cloned
}

func cloneTable(table Table) Table {
	table.Columns = append([]Column(nil), table.Columns...)
	return table
}

func cloneIndex(index Index) Index {
	index.Columns = append([]ColumnID(nil), index.Columns...)
	return index
}
