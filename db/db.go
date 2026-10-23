package db

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	sqlexec "yodadb/sql/exec"
	"yodadb/sql/parser"
	"yodadb/sql/plan"
	storageindex "yodadb/storage/index"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
	"yodadb/storage/table"
	"yodadb/types"
)

// DB is a yodaDB handle backed either by M1 in-memory execution or an M4 page
// file opened through Create or Open.
type DB struct {
	mu       sync.RWMutex
	closed   bool
	catalog  *catalog.Catalog
	engine   *sqlexec.Engine
	storage  *manifest.Database
	manifest manifest.Manifest
}

// New creates an empty in-memory database.
func New() *DB {
	schema := catalog.New()
	return &DB{catalog: schema, engine: sqlexec.NewEngine(schema)}
}

// Create creates an empty persistent database file.
func Create(ctx context.Context, path string) (*DB, error) {
	storage, err := manifest.Create(ctx, path, manifest.Options{})
	if err != nil {
		return nil, err
	}
	return &DB{
		catalog: catalog.New(),
		storage: storage,
		manifest: manifest.Manifest{
			TableRoots: make(map[uint64]page.ID),
			IndexRoots: make(map[uint64]page.ID),
		},
	}, nil
}

// Open opens a persistent database and reconstructs its latest catalog/root
// snapshot.
func Open(ctx context.Context, path string) (*DB, error) {
	storage, err := manifest.Open(ctx, path, manifest.Options{})
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = storage.Close()
		}
	}()
	record, found, err := storage.LatestManifest()
	if err != nil {
		return nil, err
	}
	if !found {
		record = manifest.Manifest{TableRoots: make(map[uint64]page.ID), IndexRoots: make(map[uint64]page.ID)}
	}
	schema, err := catalog.Load(ctx, storage, record.CatalogRoot)
	if err != nil {
		return nil, err
	}
	keep = true
	return &DB{catalog: schema, storage: storage, manifest: cloneManifest(record)}, nil
}

// Result describes a successfully executed non-query statement.
type Result struct {
	rowsAffected int64
}

// RowsAffected returns the number of rows changed by the statement.
func (r Result) RowsAffected() int64 { return r.rowsAffected }

// Outcome is the general result used by interactive adapters. Rows is non-nil
// for a query; Result describes a non-query.
type Outcome struct {
	Result Result
	Rows   *Rows
}

// Execute parses, binds, and executes any supported statement.
func (db *DB) Execute(ctx context.Context, sql string, arguments ...any) (Outcome, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return Outcome{}, dberrors.New(dberrors.CodeTransactionClosed, "db.execute", "database is closed")
	}
	statement, err := db.prepare(sql, arguments)
	if err != nil {
		return Outcome{}, err
	}
	return db.executeBound(ctx, statement)
}

// Exec executes a supported statement that does not return rows.
func (db *DB) Exec(ctx context.Context, sql string, arguments ...any) (Result, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return Result{}, dberrors.New(dberrors.CodeTransactionClosed, "db.exec", "database is closed")
	}
	statement, err := db.prepare(sql, arguments)
	if err != nil {
		return Result{}, err
	}
	if isQuery(statement) {
		return Result{}, dberrors.New(dberrors.CodeUnsupported, "db.exec", "SELECT returns rows; use Query")
	}
	outcome, err := db.executeBound(ctx, statement)
	return outcome.Result, err
}

// Query executes a supported SELECT or EXPLAIN statement.
func (db *DB) Query(ctx context.Context, sql string, arguments ...any) (*Rows, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "db.query", "database is closed")
	}
	statement, err := db.prepare(sql, arguments)
	if err != nil {
		return nil, err
	}
	if !isQuery(statement) {
		return nil, dberrors.New(dberrors.CodeUnsupported, "db.query", "statement does not return rows; use Exec")
	}
	outcome, err := db.executeBound(ctx, statement)
	return outcome.Rows, err
}

// Close releases the in-memory handle. It is idempotent.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.closed = true
	if db.storage != nil {
		return db.storage.Close()
	}
	return nil
}

func (db *DB) prepare(sql string, arguments []any) (binder.Statement, error) {
	parsed, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	parameters := make([]types.Value, len(arguments))
	for index, argument := range arguments {
		parameters[index], err = types.FromGo(argument)
		if err != nil {
			return nil, dberrors.Wrap(err, dberrors.CodeType, "db.bind_argument", fmt.Sprintf("argument %d", index+1))
		}
	}
	return binder.Bind(parsed, db.catalog, parameters)
}

func (db *DB) executeBound(ctx context.Context, statement binder.Statement) (Outcome, error) {
	if db.storage != nil {
		return db.executePersistent(ctx, statement)
	}
	result, query, err := db.engine.Execute(ctx, statement)
	if err != nil {
		return Outcome{}, err
	}
	if query != nil {
		return Outcome{Rows: newRows(query.Columns, query.Rows)}, nil
	}
	return Outcome{Result: Result{rowsAffected: result.RowsAffected}}, nil
}

func (db *DB) executePersistent(ctx context.Context, statement binder.Statement) (Outcome, error) {
	switch statement := statement.(type) {
	case *binder.CreateTable:
		return db.createPersistentTable(ctx, statement)
	case *binder.CreateIndex:
		return db.createPersistentIndex(ctx, statement)
	case *binder.Insert:
		return db.insertPersistent(ctx, statement)
	case *binder.Select:
		return db.selectPersistent(ctx, statement)
	case *binder.Update:
		return db.updatePersistent(ctx, statement)
	case *binder.Delete:
		return db.deletePersistent(ctx, statement)
	case *binder.Explain:
		access := plan.ChooseSelect(statement.Select, db.catalog)
		return Outcome{Rows: newRows([]string{"plan"}, []types.Row{{types.Text(access.Description)}})}, nil
	default:
		return Outcome{}, dberrors.New(dberrors.CodeUnsupported, "db.execute_persistent", fmt.Sprintf("unsupported statement %T", statement))
	}
}

func (db *DB) createPersistentIndex(ctx context.Context, statement *binder.CreateIndex) (Outcome, error) {
	nextCatalog := db.catalog.Clone()
	descriptor, err := nextCatalog.CreateIndex(statement.Table.ID, statement.Name, statement.ColumnIDs, statement.Unique)
	if err != nil {
		return Outcome{}, err
	}
	definition, err := indexDefinition(statement.Table, descriptor)
	if err != nil {
		return Outcome{}, err
	}
	tableTree, err := db.openTable(statement.Table, db.manifest.TableRoots[uint64(statement.Table.ID)])
	if err != nil {
		return Outcome{}, err
	}
	records, err := tableTree.Scan(ctx)
	if err != nil {
		return Outcome{}, err
	}
	txnID := db.manifest.TxnID + 1
	indexRoot := page.ID(0)
	for _, record := range records {
		indexTree, err := storageindex.Open(db.storage, indexRoot, definition)
		if err != nil {
			return Outcome{}, err
		}
		indexRoot, err = indexTree.Insert(ctx, db.storage, txnID, record.Row, record.Key)
		if err != nil {
			return Outcome{}, err
		}
	}
	catalogRoot, err := catalog.Save(ctx, db.storage, txnID, nextCatalog)
	if err != nil {
		return Outcome{}, err
	}
	next := db.nextManifest(txnID)
	next.CatalogRoot = catalogRoot
	next.IndexRoots[uint64(descriptor.ID)] = indexRoot
	if err := db.publish(ctx, next); err != nil {
		return Outcome{}, err
	}
	db.catalog = nextCatalog
	return Outcome{}, nil
}

func (db *DB) createPersistentTable(ctx context.Context, statement *binder.CreateTable) (Outcome, error) {
	nextCatalog := db.catalog.Clone()
	created, err := nextCatalog.CreateTable(statement.Name, statement.Columns)
	if err != nil {
		return Outcome{}, err
	}
	txnID := db.manifest.TxnID + 1
	catalogRoot, err := catalog.Save(ctx, db.storage, txnID, nextCatalog)
	if err != nil {
		return Outcome{}, err
	}
	next := cloneManifest(db.manifest)
	next.TxnID, next.ParentTxnID = txnID, db.manifest.TxnID
	next.CommitUnixNano = time.Now().UnixNano()
	next.CatalogRoot = catalogRoot
	next.TableRoots[uint64(created.ID)] = 0
	if err := db.publish(ctx, next); err != nil {
		return Outcome{}, err
	}
	db.catalog = nextCatalog
	return Outcome{}, nil
}

func (db *DB) insertPersistent(ctx context.Context, statement *binder.Insert) (Outcome, error) {
	row := make(types.Row, len(statement.Values))
	for index, expression := range statement.Values {
		value, err := sqlexec.Evaluate(expression, nil)
		if err != nil {
			return Outcome{}, err
		}
		row[index] = value
	}
	txnID := db.manifest.TxnID + 1
	root := db.manifest.TableRoots[uint64(statement.Table.ID)]
	tableTree, err := table.Open(db.storage, root, statement.Table.SchemaVersion, statement.Table.PrimaryKeyIndex, len(statement.Table.Columns))
	if err != nil {
		return Outcome{}, err
	}
	newRoot, tableKey, err := tableTree.Insert(ctx, db.storage, txnID, txnID, row)
	if err != nil {
		return Outcome{}, err
	}
	next := db.nextManifest(txnID)
	next.TableRoots[uint64(statement.Table.ID)] = newRoot
	for _, descriptor := range db.catalog.IndexesForTable(statement.Table.ID) {
		definition, err := indexDefinition(statement.Table, descriptor)
		if err != nil {
			return Outcome{}, err
		}
		indexTree, err := storageindex.Open(db.storage, next.IndexRoots[uint64(descriptor.ID)], definition)
		if err != nil {
			return Outcome{}, err
		}
		root, err := indexTree.Insert(ctx, db.storage, txnID, row, tableKey)
		if err != nil {
			return Outcome{}, err
		}
		next.IndexRoots[uint64(descriptor.ID)] = root
	}
	if err := db.publish(ctx, next); err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: Result{rowsAffected: 1}}, nil
}

func (db *DB) selectPersistent(ctx context.Context, statement *binder.Select) (Outcome, error) {
	tableTree, err := db.openTable(statement.Table, db.manifest.TableRoots[uint64(statement.Table.ID)])
	if err != nil {
		return Outcome{}, err
	}
	records, err := db.selectRecords(ctx, statement, tableTree)
	if err != nil {
		return Outcome{}, err
	}
	columns := make([]string, len(statement.Projections))
	for index, projection := range statement.Projections {
		columns[index] = projection.Name
	}
	result := make([]types.Row, 0, len(records))
	for _, record := range records {
		if statement.Where != nil {
			predicate, err := sqlexec.Evaluate(statement.Where, record.Row)
			if err != nil {
				return Outcome{}, err
			}
			matched, ok := predicate.BoolValue()
			if !ok || !matched {
				continue
			}
		}
		projected := make(types.Row, len(statement.Projections))
		for index, projection := range statement.Projections {
			projected[index], err = sqlexec.Evaluate(projection.Expr, record.Row)
			if err != nil {
				return Outcome{}, err
			}
		}
		result = append(result, projected)
	}
	return Outcome{Rows: newRows(columns, result)}, nil
}

func (db *DB) updatePersistent(ctx context.Context, statement *binder.Update) (Outcome, error) {
	tableRoot := db.manifest.TableRoots[uint64(statement.Table.ID)]
	tableTree, err := db.openTable(statement.Table, tableRoot)
	if err != nil {
		return Outcome{}, err
	}
	records, err := tableTree.Scan(ctx)
	if err != nil {
		return Outcome{}, err
	}
	txnID := db.manifest.TxnID + 1
	next := db.nextManifest(txnID)
	var affected int64
	for _, record := range records {
		matched, err := matches(statement.Where, record.Row)
		if err != nil {
			return Outcome{}, err
		}
		if !matched {
			continue
		}
		replacement := append(types.Row(nil), record.Row...)
		for _, assignment := range statement.Assignments {
			replacement[assignment.ColumnIndex], err = sqlexec.Evaluate(assignment.Value, record.Row)
			if err != nil {
				return Outcome{}, err
			}
		}
		if err := validateRow(statement.Table, replacement); err != nil {
			return Outcome{}, err
		}
		for _, descriptor := range db.catalog.IndexesForTable(statement.Table.ID) {
			root, err := db.deleteIndexEntry(ctx, txnID, statement.Table, descriptor, next.IndexRoots[uint64(descriptor.ID)], record.Row, record.Key)
			if err != nil {
				return Outcome{}, err
			}
			next.IndexRoots[uint64(descriptor.ID)] = root
		}
		currentTable, err := db.openTable(statement.Table, tableRoot)
		if err != nil {
			return Outcome{}, err
		}
		hiddenID, err := hiddenRowID(statement.Table, record.Key)
		if err != nil {
			return Outcome{}, err
		}
		var newKey []byte
		tableRoot, newKey, err = currentTable.Replace(ctx, db.storage, txnID, hiddenID, record.Key, replacement)
		if err != nil {
			return Outcome{}, err
		}
		for _, descriptor := range db.catalog.IndexesForTable(statement.Table.ID) {
			root, err := db.insertIndexEntry(ctx, txnID, statement.Table, descriptor, next.IndexRoots[uint64(descriptor.ID)], replacement, newKey)
			if err != nil {
				return Outcome{}, err
			}
			next.IndexRoots[uint64(descriptor.ID)] = root
		}
		affected++
	}
	if affected == 0 {
		return Outcome{Result: Result{}}, nil
	}
	next.TableRoots[uint64(statement.Table.ID)] = tableRoot
	if err := db.publish(ctx, next); err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: Result{rowsAffected: affected}}, nil
}

func (db *DB) deletePersistent(ctx context.Context, statement *binder.Delete) (Outcome, error) {
	tableRoot := db.manifest.TableRoots[uint64(statement.Table.ID)]
	tableTree, err := db.openTable(statement.Table, tableRoot)
	if err != nil {
		return Outcome{}, err
	}
	records, err := tableTree.Scan(ctx)
	if err != nil {
		return Outcome{}, err
	}
	txnID := db.manifest.TxnID + 1
	next := db.nextManifest(txnID)
	var affected int64
	for _, record := range records {
		matched, err := matches(statement.Where, record.Row)
		if err != nil {
			return Outcome{}, err
		}
		if !matched {
			continue
		}
		for _, descriptor := range db.catalog.IndexesForTable(statement.Table.ID) {
			root, err := db.deleteIndexEntry(ctx, txnID, statement.Table, descriptor, next.IndexRoots[uint64(descriptor.ID)], record.Row, record.Key)
			if err != nil {
				return Outcome{}, err
			}
			next.IndexRoots[uint64(descriptor.ID)] = root
		}
		currentTable, err := db.openTable(statement.Table, tableRoot)
		if err != nil {
			return Outcome{}, err
		}
		tableRoot, _, err = currentTable.Delete(ctx, db.storage, txnID, record.Key)
		if err != nil {
			return Outcome{}, err
		}
		affected++
	}
	if affected == 0 {
		return Outcome{Result: Result{}}, nil
	}
	next.TableRoots[uint64(statement.Table.ID)] = tableRoot
	if err := db.publish(ctx, next); err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: Result{rowsAffected: affected}}, nil
}

func (db *DB) selectRecords(ctx context.Context, statement *binder.Select, tableTree *table.Tree) ([]table.Record, error) {
	access := plan.ChooseSelect(statement, db.catalog)
	switch access.Kind {
	case plan.PrimaryKeyLookup:
		key, err := tableTree.EncodePrimaryKey(access.Value)
		if err != nil {
			return nil, err
		}
		row, found, err := tableTree.Lookup(ctx, key)
		if err != nil || !found {
			return nil, err
		}
		return []table.Record{{Key: key, Row: row}}, nil
	case plan.PrimaryKeyRange:
		key, err := tableTree.EncodePrimaryKey(access.Value)
		if err != nil {
			return nil, err
		}
		start, end := rangeBounds(key, access.Operator)
		return tableTree.ScanRange(ctx, start, end)
	case plan.SecondaryIndexLookup, plan.SecondaryIndexRange:
		definition, err := indexDefinition(statement.Table, access.Index)
		if err != nil {
			return nil, err
		}
		indexTree, err := storageindex.Open(db.storage, db.manifest.IndexRoots[uint64(access.Index.ID)], definition)
		if err != nil {
			return nil, err
		}
		var keys [][]byte
		if access.Kind == plan.SecondaryIndexLookup {
			keys, err = indexTree.Lookup(ctx, types.Row{access.Value})
		} else {
			encoded, encodeErr := types.EncodeKey(types.Row{access.Value}, types.KeyLimits{})
			if encodeErr != nil {
				return nil, encodeErr
			}
			start, end := rangeBounds(encoded, access.Operator)
			keys, err = indexTree.ScanRange(ctx, start, end)
		}
		if err != nil {
			return nil, err
		}
		records := make([]table.Record, 0, len(keys))
		for _, key := range keys {
			row, found, err := tableTree.Lookup(ctx, key)
			if err != nil {
				return nil, err
			}
			if found {
				records = append(records, table.Record{Key: bytes.Clone(key), Row: row})
			}
		}
		return records, nil
	default:
		return tableTree.Scan(ctx)
	}
}

func (db *DB) openTable(descriptor catalog.Table, root page.ID) (*table.Tree, error) {
	return table.Open(db.storage, root, descriptor.SchemaVersion, descriptor.PrimaryKeyIndex, len(descriptor.Columns))
}

func (db *DB) nextManifest(txnID uint64) manifest.Manifest {
	next := cloneManifest(db.manifest)
	next.TxnID, next.ParentTxnID = txnID, db.manifest.TxnID
	next.CommitUnixNano = time.Now().UnixNano()
	return next
}

func (db *DB) insertIndexEntry(ctx context.Context, txnID uint64, tableDescriptor catalog.Table, descriptor catalog.Index, root page.ID, row types.Row, key []byte) (page.ID, error) {
	definition, err := indexDefinition(tableDescriptor, descriptor)
	if err != nil {
		return 0, err
	}
	tree, err := storageindex.Open(db.storage, root, definition)
	if err != nil {
		return 0, err
	}
	return tree.Insert(ctx, db.storage, txnID, row, key)
}

func (db *DB) deleteIndexEntry(ctx context.Context, txnID uint64, tableDescriptor catalog.Table, descriptor catalog.Index, root page.ID, row types.Row, key []byte) (page.ID, error) {
	definition, err := indexDefinition(tableDescriptor, descriptor)
	if err != nil {
		return 0, err
	}
	tree, err := storageindex.Open(db.storage, root, definition)
	if err != nil {
		return 0, err
	}
	newRoot, deleted, err := tree.Delete(ctx, db.storage, txnID, row, key)
	if err != nil {
		return 0, err
	}
	if !deleted {
		return 0, dberrors.New(dberrors.CodeCorruption, "db.delete_index", "table row is missing from secondary index")
	}
	return newRoot, nil
}

func indexDefinition(tableDescriptor catalog.Table, descriptor catalog.Index) (storageindex.Definition, error) {
	ordinals := make([]int, len(descriptor.Columns))
	for index, columnID := range descriptor.Columns {
		ordinals[index] = -1
		for ordinal, column := range tableDescriptor.Columns {
			if column.ID == columnID {
				ordinals[index] = ordinal
				break
			}
		}
		if ordinals[index] < 0 {
			return storageindex.Definition{}, dberrors.New(dberrors.CodeCorruption, "db.index_definition", "index references an unknown column")
		}
	}
	return storageindex.Definition{Columns: ordinals, Unique: descriptor.Unique}, nil
}

func matches(where *binder.Expression, row types.Row) (bool, error) {
	if where == nil {
		return true, nil
	}
	value, err := sqlexec.Evaluate(where, row)
	if err != nil {
		return false, err
	}
	matched, ok := value.BoolValue()
	return ok && matched, nil
}

func validateRow(tableDescriptor catalog.Table, row types.Row) error {
	for index, column := range tableDescriptor.Columns {
		if row[index].IsNull() && column.NotNull {
			return dberrors.New(dberrors.CodeConstraint, "db.update", fmt.Sprintf("column %q cannot be NULL", column.Name))
		}
		if !row[index].IsNull() && row[index].Kind() != column.Type {
			return dberrors.New(dberrors.CodeType, "db.update", fmt.Sprintf("column %q expects %s", column.Name, column.Type))
		}
	}
	return nil
}

func hiddenRowID(tableDescriptor catalog.Table, key []byte) (uint64, error) {
	if tableDescriptor.PrimaryKeyIndex >= 0 {
		return 0, nil
	}
	values, err := types.DecodeKey(key, types.KeyLimits{})
	if err != nil {
		return 0, err
	}
	if len(values) != 1 {
		return 0, dberrors.New(dberrors.CodeCorruption, "db.hidden_rowid", "invalid hidden RowID key")
	}
	value, ok := values[0].Int64Value()
	if !ok || value <= 0 {
		return 0, dberrors.New(dberrors.CodeCorruption, "db.hidden_rowid", "invalid hidden RowID value")
	}
	return uint64(value), nil
}

func rangeBounds(key []byte, operator parser.Operator) ([]byte, []byte) {
	switch operator {
	case parser.OpLess:
		return nil, key
	case parser.OpLessEqual:
		return nil, prefixSuccessor(key)
	case parser.OpGreater:
		return prefixSuccessor(key), nil
	default:
		return key, nil
	}
}

func prefixSuccessor(key []byte) []byte {
	result := bytes.Clone(key)
	for index := len(result) - 1; index >= 0; index-- {
		if result[index] != 0xff {
			result[index]++
			return result[:index+1]
		}
	}
	return nil
}

func isQuery(statement binder.Statement) bool {
	switch statement.(type) {
	case *binder.Select, *binder.Explain:
		return true
	default:
		return false
	}
}

func (db *DB) publish(ctx context.Context, next manifest.Manifest) error {
	if next.TableRoots == nil {
		next.TableRoots = make(map[uint64]page.ID)
	}
	if next.IndexRoots == nil {
		next.IndexRoots = make(map[uint64]page.ID)
	}
	if _, err := db.storage.Publish(ctx, next); err != nil {
		return err
	}
	db.manifest = cloneManifest(next)
	return nil
}

func cloneManifest(record manifest.Manifest) manifest.Manifest {
	cloned := record
	cloned.TableRoots = make(map[uint64]page.ID, len(record.TableRoots))
	for id, root := range record.TableRoots {
		cloned.TableRoots[id] = root
	}
	cloned.IndexRoots = make(map[uint64]page.ID, len(record.IndexRoots))
	for id, root := range record.IndexRoots {
		cloned.IndexRoots[id] = root
	}
	return cloned
}
