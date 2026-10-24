package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

func TestQueryAsOfTransactionUsesHistoricalDataCatalogAndPlan(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "as-of.yoda")
	database, err := db.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)") // txn 1
	mustExec(t, database, "INSERT INTO users VALUES (1, 'Ada')")                           // txn 2
	mustExec(t, database, "UPDATE users SET name = 'Grace' WHERE id = 1")                  // txn 3
	mustExec(t, database, "CREATE INDEX users_name ON users (name)")                       // txn 4
	mustExec(t, database, "CREATE TABLE later (id INT64 PRIMARY KEY)")                     // txn 5

	assertStrings(t, database, "SELECT name FROM users AS OF TRANSACTION 2 WHERE id = 1", []string{"Ada"})
	assertStrings(t, database, "SELECT name FROM users AS OF TRANSACTION 3 WHERE id = 1", []string{"Grace"})
	assertPlan(t, database, "EXPLAIN SELECT id FROM users AS OF TRANSACTION 3 WHERE name = 'Grace'", "TABLE SCAN users")
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE name = 'Grace'", "INDEX LOOKUP users_name ON users(name)")
	if _, err := database.Query(ctx, "SELECT id FROM later AS OF TRANSACTION 4"); !errors.Is(err, dberrors.ErrBind) {
		t.Fatalf("query table before creation error=%v, want bind", err)
	}
	for _, txnID := range []string{"0", "99"} {
		if _, err := database.Query(ctx, "SELECT name FROM users AS OF TRANSACTION "+txnID); !errors.Is(err, dberrors.ErrConstraint) {
			t.Fatalf("AS OF %s error=%v, want constraint", txnID, err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertStrings(t, database, "SELECT name FROM users AS OF TRANSACTION 2 WHERE id = 1", []string{"Ada"})
}

func TestReadTxAcceptsOnlyItsOwnExplicitSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "read-as-of.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	read, err := database.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	rows, err := read.Query(ctx, "SELECT id FROM users AS OF TRANSACTION 1")
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if _, err := read.Query(ctx, "SELECT id FROM users AS OF TRANSACTION 2"); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("different explicit snapshot error=%v, want constraint", err)
	}
}

func TestAsOfTransactionRejectsInMemoryDatabase(t *testing.T) {
	database := db.New()
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	if _, err := database.Query(context.Background(), "SELECT id FROM users AS OF TRANSACTION 1"); !errors.Is(err, dberrors.ErrUnsupported) {
		t.Fatalf("in-memory AS OF error=%v, want unsupported", err)
	}
}
