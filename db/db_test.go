package db_test

import (
	"context"
	"errors"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

func TestM1PublicAPIScenario(t *testing.T) {
	database := db.New()
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	if _, err := database.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	result, err := database.Exec(ctx, "INSERT INTO users VALUES (?, ?)", int64(1), "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsAffected() != 1 {
		t.Fatalf("RowsAffected = %d, want 1", result.RowsAffected())
	}
	rows, err := database.Query(ctx, "SELECT name FROM users WHERE id = ?", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if got := rows.Columns(); len(got) != 1 || got[0] != "name" {
		t.Fatalf("Columns = %v, want [name]", got)
	}
	if !rows.Next() {
		t.Fatalf("Next = false, error %v", rows.Err())
	}
	var name string
	if err := rows.Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q, want Ada", name)
	}
	if rows.Next() {
		t.Fatal("unexpected second row")
	}
	if values := rows.Values(); values != nil {
		t.Fatalf("Values after exhaustion = %v, want nil", values)
	}
}

func TestRowsScanRejectsWrongDestinationType(t *testing.T) {
	database := db.New()
	ctx := context.Background()
	if _, err := database.Exec(ctx, "CREATE TABLE values_table (value INT64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO values_table VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Query(ctx, "SELECT value FROM values_table")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing row")
	}
	var wrong string
	if err := rows.Scan(&wrong); !errors.Is(err, dberrors.ErrType) {
		t.Fatalf("Scan error = %v, want type category", err)
	}
}

func TestPublicAPIReturnsStableErrorCategories(t *testing.T) {
	database := db.New()
	ctx := context.Background()
	if _, err := database.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		sql      string
		category error
	}{
		{"malformed", "SELEC name FROM users", dberrors.ErrSyntax},
		{"invalid type", "INSERT INTO users VALUES (2, 99)", dberrors.ErrType},
		{"duplicate key", "INSERT INTO users VALUES (1, 'Grace')", dberrors.ErrConstraint},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := database.Exec(ctx, test.sql)
			if !errors.Is(err, test.category) {
				t.Fatalf("Exec error = %v, want category %v", err, test.category)
			}
		})
	}
}

func TestQueryDoesNotExecuteMutation(t *testing.T) {
	database := db.New()
	_, err := database.Query(context.Background(), "CREATE TABLE users (id INT64)")
	if !errors.Is(err, dberrors.ErrUnsupported) {
		t.Fatalf("Query CREATE error = %v, want unsupported", err)
	}
	if _, err := database.Exec(context.Background(), "CREATE TABLE users (id INT64)"); err != nil {
		t.Fatalf("CREATE was executed by Query: %v", err)
	}
}

func TestClosedDatabaseRejectsOperations(t *testing.T) {
	database := db.New()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := database.Exec(context.Background(), "CREATE TABLE t (id INT64)")
	if !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("Exec after Close error = %v, want closed category", err)
	}
}
