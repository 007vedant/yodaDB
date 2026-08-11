package binder_test

import (
	"errors"
	"testing"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/parser"
	"yodadb/types"
)

func parseAndBind(t *testing.T, schema *catalog.Catalog, sql string, parameters ...types.Value) (binder.Statement, error) {
	t.Helper()
	statement, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return binder.Bind(statement, schema, parameters)
}

func usersCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	schema := catalog.New()
	_, err := schema.CreateTable("users", []catalog.ColumnSpec{
		{Name: "id", Type: types.Int64Kind, PrimaryKey: true},
		{Name: "name", Type: types.TextKind, NotNull: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestBindCreateCanonicalizesNamesAndTypes(t *testing.T) {
	bound, err := parseAndBind(t, catalog.New(), `CREATE TABLE Users (ID INT64 PRIMARY KEY, "Display" TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	create := bound.(*binder.CreateTable)
	if create.Name != "users" || create.Columns[0].Name != "id" || create.Columns[1].Name != "Display" {
		t.Fatalf("canonical names = %#v", create)
	}
	if create.Columns[0].Type != types.Int64Kind || !create.Columns[0].NotNull {
		t.Fatalf("column metadata = %#v", create.Columns[0])
	}
}

func TestBindInsertChecksTypesNullsAndParameters(t *testing.T) {
	schema := usersCatalog(t)
	bound, err := parseAndBind(t, schema, "INSERT INTO users VALUES (?, ?)", types.Int64(1), types.Text("Ada"))
	if err != nil {
		t.Fatal(err)
	}
	insert := bound.(*binder.Insert)
	if got, _ := insert.Values[1].Value.TextValue(); got != "Ada" {
		t.Fatalf("bound parameter = %q, want Ada", got)
	}

	tests := []struct {
		name       string
		sql        string
		parameters []types.Value
		category   error
	}{
		{"wrong type", "INSERT INTO users VALUES (1, 7)", nil, dberrors.ErrType},
		{"not null", "INSERT INTO users VALUES (1, NULL)", nil, dberrors.ErrConstraint},
		{"missing parameter", "INSERT INTO users VALUES (?, 'Ada')", nil, dberrors.ErrBind},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseAndBind(t, schema, test.sql, test.parameters...)
			if !errors.Is(err, test.category) {
				t.Fatalf("Bind() error = %v, want category %v", err, test.category)
			}
		})
	}
}

func TestBindSelectResolvesStarPredicateAndAlias(t *testing.T) {
	bound, err := parseAndBind(t, usersCatalog(t), "SELECT *, name AS display FROM users WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	selectStatement := bound.(*binder.Select)
	if len(selectStatement.Projections) != 3 {
		t.Fatalf("projection count = %d, want 3", len(selectStatement.Projections))
	}
	if selectStatement.Projections[2].Name != "display" || selectStatement.Where.Type != types.BoolKind {
		t.Fatalf("bound SELECT = %#v", selectStatement)
	}
}

func TestBindReportsUnknownNamesAndInvalidWhere(t *testing.T) {
	schema := usersCatalog(t)
	for _, test := range []struct {
		sql      string
		category error
	}{
		{"SELECT id FROM missing", dberrors.ErrBind},
		{"SELECT missing FROM users", dberrors.ErrBind},
		{"SELECT name FROM users WHERE id", dberrors.ErrType},
	} {
		_, err := parseAndBind(t, schema, test.sql)
		if !errors.Is(err, test.category) {
			t.Fatalf("Bind(%q) error = %v, want %v", test.sql, err, test.category)
		}
	}
}

func TestBindAllowsNullWherePredicate(t *testing.T) {
	bound, err := parseAndBind(t, usersCatalog(t), "SELECT name FROM users WHERE NULL")
	if err != nil {
		t.Fatal(err)
	}
	if where := bound.(*binder.Select).Where; where.Type != types.BoolKind || !where.Value.IsNull() {
		t.Fatalf("bound WHERE NULL = %#v", where)
	}
}

func TestBindRejectsUnknownTypeAndDuplicateColumn(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE bad (value MYSTERY)",
		"CREATE TABLE bad (id INT64, ID INT64)",
	} {
		_, err := parseAndBind(t, catalog.New(), sql)
		if err == nil {
			t.Fatalf("Bind(%q) unexpectedly succeeded", sql)
		}
	}
}
