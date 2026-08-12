// Package yodadb exposes the native embedded database API.
package yodadb

import "yodadb/db"

// DB is an in-memory database handle in M1.
type DB = db.DB

// Result describes a successfully executed non-query statement.
type Result = db.Result

// Rows is a query result cursor.
type Rows = db.Rows

// Outcome is the general result used by interactive adapters.
type Outcome = db.Outcome

// New creates an empty in-memory database. Persistent Open is introduced by a
// later milestone.
func New() *DB { return db.New() }
