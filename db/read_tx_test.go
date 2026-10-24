package db_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

func TestReadTxPinsManifestAndCatalog(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "read-snapshot.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'Ada')")

	read, err := database.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.TxnID() != 2 {
		t.Fatalf("snapshot transaction ID=%d, want 2", read.TxnID())
	}
	mustExec(t, database, "UPDATE users SET name = 'Grace' WHERE id = 1")
	mustExec(t, database, "CREATE TABLE later (id INT64 PRIMARY KEY)")

	assertReadStrings(t, read, "SELECT name FROM users WHERE id = 1", []string{"Ada"})
	assertReadStrings(t, read, "EXPLAIN SELECT name FROM users WHERE id = 1", []string{"PRIMARY KEY LOOKUP users(id)"})
	if _, err := read.Query(ctx, "SELECT id FROM later"); !errors.Is(err, dberrors.ErrBind) {
		t.Fatalf("snapshot query for later table error=%v, want bind error", err)
	}
	assertStrings(t, database, "SELECT name FROM users WHERE id = 1", []string{"Grace"})

	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := read.Query(ctx, "SELECT name FROM users"); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("Query after Close error=%v, want transaction closed", err)
	}
}

func TestReadTxHandlesShareSnapshotPinIndependently(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "shared-pin.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE values_table (value INT64 PRIMARY KEY)")

	first, err := database.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.TxnID() != second.TxnID() {
		t.Fatalf("snapshot IDs differ: %d and %d", first.TxnID(), second.TxnID())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	assertReadStrings(t, second, "EXPLAIN SELECT value FROM values_table", []string{"TABLE SCAN values_table"})
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginReadRejectsInMemoryDatabase(t *testing.T) {
	database := db.New()
	defer database.Close()
	if _, err := database.BeginRead(context.Background()); !errors.Is(err, dberrors.ErrUnsupported) {
		t.Fatalf("BeginRead error=%v, want unsupported", err)
	}
}

func assertReadStrings(t *testing.T, tx *db.ReadTx, query string, want []string) {
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
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
