package exec_test

import (
	"context"
	"errors"
	"testing"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/exec"
	"yodadb/sql/parser"
)

func executeSQL(t *testing.T, schema *catalog.Catalog, engine *exec.Engine, sql string) (*exec.QueryResult, error) {
	t.Helper()
	parsed, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	bound, err := binder.Bind(parsed, schema, nil)
	if err != nil {
		return nil, err
	}
	_, rows, err := engine.Execute(context.Background(), bound)
	return rows, err
}

func TestEngineM1Scenario(t *testing.T) {
	schema := catalog.New()
	engine := exec.NewEngine(schema)
	if _, err := executeSQL(t, schema, engine, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := executeSQL(t, schema, engine, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	rows, err := executeSQL(t, schema, engine, "SELECT name FROM users WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("row count = %d, want 1", len(rows.Rows))
	}
	if got, _ := rows.Rows[0][0].TextValue(); got != "Ada" {
		t.Fatalf("selected name = %q, want Ada", got)
	}
}

func TestEngineRejectsDuplicatePrimaryKey(t *testing.T) {
	schema := catalog.New()
	engine := exec.NewEngine(schema)
	for _, sql := range []string{
		"CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)",
		"INSERT INTO users VALUES (1, 'Ada')",
	} {
		if _, err := executeSQL(t, schema, engine, sql); err != nil {
			t.Fatal(err)
		}
	}
	_, err := executeSQL(t, schema, engine, "INSERT INTO users VALUES (1, 'Grace')")
	if !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate insert error = %v, want constraint", err)
	}
}
