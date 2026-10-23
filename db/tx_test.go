package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
	"yodadb/storage/manifest"
)

func TestTxReadOwnWritesAndOneManifestCommit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "transaction.yoda")
	database, err := db.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE events (value TEXT NOT NULL)")

	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO events VALUES ('alpha')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO events VALUES ('beta')"); err != nil {
		t.Fatal(err)
	}
	assertTxStrings(t, tx, "SELECT value FROM events", []string{"alpha", "beta"})
	if _, err := database.Query(ctx, "SELECT value FROM events"); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("outside query error=%v, want conflict", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO events VALUES ('late')"); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("Exec after commit error=%v, want transaction closed", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.Open(ctx, path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := store.LatestManifest()
	if err != nil || !found {
		t.Fatalf("latest manifest found=%t error=%v", found, err)
	}
	if record.TxnID != 2 {
		t.Fatalf("transaction ID=%d, want 2 (CREATE plus one multi-statement commit)", record.TxnID)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertStrings(t, database, "SELECT value FROM events", []string{"alpha", "beta"})
}

func TestTxRollbackLeavesNoReachableWrites(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "rollback.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	assertTxStrings(t, tx, "SELECT name FROM users WHERE id = 1", []string{"Ada"})
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertNoRow(t, database, "SELECT name FROM users WHERE id = 1")
	if err := tx.Rollback(); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("second rollback error=%v", err)
	}
}

func TestSQLTransactionCommitRollbackAndErrors(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "sql-transaction.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT NOT NULL)")
	mustExec(t, database, "CREATE UNIQUE INDEX users_email ON users (email)")

	mustExec(t, database, "BEGIN")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'ada@example.com')")
	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (2, 'ada@example.com')"); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate error=%v, want constraint", err)
	}
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'ada@example.com'", 1)
	if _, err := database.Exec(ctx, "CREATE TABLE deferred (id INT64)"); !errors.Is(err, dberrors.ErrUnsupported) {
		t.Fatalf("transactional DDL error=%v, want unsupported", err)
	}
	mustExec(t, database, "ROLLBACK")
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 1")

	mustExec(t, database, "BEGIN")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'ada@example.com')")
	if _, err := database.Exec(ctx, "BEGIN"); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("nested BEGIN error=%v", err)
	}
	mustExec(t, database, "COMMIT")
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'ada@example.com'", 1)
	if _, err := database.Exec(ctx, "COMMIT"); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("COMMIT without transaction error=%v", err)
	}
}

func TestTxCancelledCommitAbortsPrivateState(t *testing.T) {
	database, err := db.Create(context.Background(), filepath.Join(t.TempDir(), "cancel.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	tx, err := database.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tx.Commit(cancelled); !errors.Is(err, dberrors.ErrCancelled) {
		t.Fatalf("Commit error=%v, want cancelled", err)
	}
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 1")
}

func assertTxStrings(t *testing.T, tx *db.Tx, query string, want []string) {
	t.Helper()
	rows, err := tx.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if len(got) != len(want) {
		t.Fatalf("query %q returned %v, want %v", query, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("query %q returned %v, want %v", query, got, want)
		}
	}
}

func assertStrings(t *testing.T, database *db.DB, query string, want []string) {
	t.Helper()
	rows, err := database.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if len(got) != len(want) {
		t.Fatalf("query %q returned %v, want %v", query, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("query %q returned %v, want %v", query, got, want)
		}
	}
}
