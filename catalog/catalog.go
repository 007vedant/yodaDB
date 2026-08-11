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
	tableNames   map[string]TableID
	tables       map[TableID]Table
}

// New creates an empty catalog.
func New() *Catalog {
	return &Catalog{
		nextTableID:  1,
		nextColumnID: 1,
		tableNames:   make(map[string]TableID),
		tables:       make(map[TableID]Table),
	}
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

func cloneTable(table Table) Table {
	table.Columns = append([]Column(nil), table.Columns...)
	return table
}
