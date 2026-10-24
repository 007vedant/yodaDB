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

func TestNamedSnapshotsPinHistoryAndSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retention.yoda")
	database, err := db.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)") // txn 1
	mustExec(t, database, "INSERT INTO users VALUES (1, 'Ada')")                           // txn 2
	mustExec(t, database, "UPDATE users SET name = 'Grace' WHERE id = 1")                  // txn 3

	if err := database.CreateSnapshot(ctx, "ada", 2); err != nil { // txn 4
		t.Fatal(err)
	}
	snapshots, err := database.Snapshots()
	if err != nil || len(snapshots) != 1 || snapshots[0].Name != "ada" || snapshots[0].TxnID != 2 {
		t.Fatalf("snapshots=%+v error=%v", snapshots, err)
	}
	if err := database.RetainHistoryFrom(ctx, 3); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("retention past named snapshot error=%v, want conflict", err)
	}

	read, err := database.OpenSnapshot(ctx, "ada")
	if err != nil {
		t.Fatal(err)
	}
	assertReadStrings(t, read, "SELECT name FROM users WHERE id = 1", []string{"Ada"})
	if err := database.DropSnapshot(ctx, "ada"); err != nil { // txn 5
		t.Fatal(err)
	}
	if err := database.RetainHistoryFrom(ctx, 3); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("retention past active reader error=%v, want conflict", err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.RetainHistoryFrom(ctx, 3); err != nil { // txn 6
		t.Fatal(err)
	}
	if _, err := database.Query(ctx, "SELECT name FROM users AS OF TRANSACTION 2"); !errors.Is(err, dberrors.ErrHistoryNotRetained) {
		t.Fatalf("pruned historical query error=%v, want history not retained", err)
	}
	assertStrings(t, database, "SELECT name FROM users AS OF TRANSACTION 3 WHERE id = 1", []string{"Grace"})
	if err := database.RetainHistoryFrom(ctx, 2); !errors.Is(err, dberrors.ErrHistoryNotRetained) {
		t.Fatalf("retention rollback error=%v, want history not retained", err)
	}
	if err := database.CreateSnapshot(ctx, "grace", 3); err != nil { // txn 7
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err = database.Snapshots()
	if err != nil || len(snapshots) != 1 || snapshots[0].Name != "grace" || snapshots[0].TxnID != 3 {
		t.Fatalf("reopened snapshots=%+v error=%v", snapshots, err)
	}
	read, err = database.OpenSnapshot(ctx, "grace")
	if err != nil {
		t.Fatal(err)
	}
	assertReadStrings(t, read, "SELECT name FROM users", []string{"Grace"})
	read.Close()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.Open(ctx, path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := store.Metadata().OldestRetainedTxnID; got != 3 {
		t.Fatalf("oldest retained transaction=%d, want 3", got)
	}
	if _, found, err := store.ManifestByTxnID(2); !errors.Is(err, dberrors.ErrHistoryNotRetained) || found {
		t.Fatalf("ManifestByTxnID(2) found=%t error=%v", found, err)
	}
	history, err := store.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[0].TxnID != 3 {
		t.Fatalf("retained history begins with %+v, want transaction 3", history)
	}
}

func TestNamedSnapshotValidationErrors(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "snapshot-errors.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY)")
	if err := database.CreateSnapshot(ctx, "future", 99); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("future snapshot error=%v, want constraint", err)
	}
	if _, err := database.OpenSnapshot(ctx, "missing"); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("missing snapshot error=%v, want constraint", err)
	}
}
