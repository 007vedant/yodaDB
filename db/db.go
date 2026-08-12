package db

import (
	"context"
	"fmt"
	"sync"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	sqlexec "yodadb/sql/exec"
	"yodadb/sql/parser"
	"yodadb/types"
)

// DB is an in-memory yodaDB handle. Persistence is introduced in M4.
type DB struct {
	mu      sync.RWMutex
	closed  bool
	catalog *catalog.Catalog
	engine  *sqlexec.Engine
}

// New creates an empty in-memory database.
func New() *DB {
	schema := catalog.New()
	return &DB{catalog: schema, engine: sqlexec.NewEngine(schema)}
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

// Execute parses, binds, and executes any supported M1 statement.
func (db *DB) Execute(ctx context.Context, sql string, arguments ...any) (Outcome, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
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
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return Result{}, dberrors.New(dberrors.CodeTransactionClosed, "db.exec", "database is closed")
	}
	statement, err := db.prepare(sql, arguments)
	if err != nil {
		return Result{}, err
	}
	if _, query := statement.(*binder.Select); query {
		return Result{}, dberrors.New(dberrors.CodeUnsupported, "db.exec", "SELECT returns rows; use Query")
	}
	outcome, err := db.executeBound(ctx, statement)
	return outcome.Result, err
}

// Query executes a supported SELECT statement.
func (db *DB) Query(ctx context.Context, sql string, arguments ...any) (*Rows, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "db.query", "database is closed")
	}
	statement, err := db.prepare(sql, arguments)
	if err != nil {
		return nil, err
	}
	if _, query := statement.(*binder.Select); !query {
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
	result, query, err := db.engine.Execute(ctx, statement)
	if err != nil {
		return Outcome{}, err
	}
	if query != nil {
		return Outcome{Rows: newRows(query.Columns, query.Rows)}, nil
	}
	return Outcome{Result: Result{rowsAffected: result.RowsAffected}}, nil
}
