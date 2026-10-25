// Package yodadb exposes the native embedded database API.
package yodadb

import (
	"context"

	"yodadb/db"
)

// DB is an embedded database handle.
type DB = db.DB

// Tx is an explicit single-writer transaction handle.
type Tx = db.Tx

// ReadTx is a read-only transaction bound to one committed snapshot.
type ReadTx = db.ReadTx

// NamedSnapshot describes a durable name for one committed transaction.
type NamedSnapshot = db.NamedSnapshot

// Result describes a successfully executed non-query statement.
type Result = db.Result

// Rows is a query result cursor.
type Rows = db.Rows

// Outcome is the general result used by interactive adapters.
type Outcome = db.Outcome

// New creates an empty in-memory database.
func New() *DB { return db.New() }

// Create creates an empty persistent database file.
func Create(ctx context.Context, path string) (*DB, error) { return db.Create(ctx, path) }

// Open opens a persistent database file.
func Open(ctx context.Context, path string) (*DB, error) { return db.Open(ctx, path) }
