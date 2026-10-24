package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

func TestPersistentDatabaseAcceptsExplicitProcessDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process.yoda")
	database, err := db.CreateWithOptions(context.Background(), path, db.Options{Durability: db.DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE values_table (value INT64)")
	mustExec(t, database, "INSERT INTO values_table VALUES (42)")
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := db.OpenWithOptions(context.Background(), path, db.Options{Durability: db.DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertOneInt64(t, reopened, "SELECT value FROM values_table", 42)
}

func TestPersistentDatabaseRejectsInvalidDurabilityBeforeCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yoda")
	_, err := db.CreateWithOptions(context.Background(), path, db.Options{Durability: db.Durability(99)})
	if !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("CreateWithOptions error = %v, want constraint", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid create left file: %v", err)
	}
}
