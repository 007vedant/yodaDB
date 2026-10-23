package parser_test

import (
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/sql/parser"
)

func TestParseCreateTable(t *testing.T) {
	statement, err := parser.Parse("CREATE TABLE Users (id INT64 PRIMARY KEY, name TEXT NOT NULL);")
	if err != nil {
		t.Fatal(err)
	}
	create, ok := statement.(*parser.CreateTableStatement)
	if !ok {
		t.Fatalf("statement type = %T, want *CreateTableStatement", statement)
	}
	if create.Table.Canonical() != "users" || len(create.Columns) != 2 {
		t.Fatalf("unexpected CREATE AST: %#v", create)
	}
	if !create.Columns[0].PrimaryKey || !create.Columns[0].NotNull || !create.Columns[1].NotNull {
		t.Fatalf("column constraints not preserved: %#v", create.Columns)
	}
}

func TestParseInsertParameters(t *testing.T) {
	statement, err := parser.Parse("INSERT INTO users VALUES (?, 'Ada')")
	if err != nil {
		t.Fatal(err)
	}
	insert := statement.(*parser.InsertStatement)
	if len(insert.Values) != 2 || parser.ParameterCount(statement) != 1 {
		t.Fatalf("unexpected INSERT AST: %#v", insert)
	}
}

func TestParseSelectPrecedence(t *testing.T) {
	statement, err := parser.Parse("SELECT name AS display FROM users WHERE id = 1 OR id = 2 AND name IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	selectStatement := statement.(*parser.SelectStatement)
	root, ok := selectStatement.Where.(*parser.BinaryExpression)
	if !ok || root.Operator != parser.OpOr {
		t.Fatalf("WHERE root = %#v, want OR", selectStatement.Where)
	}
	right, ok := root.Right.(*parser.BinaryExpression)
	if !ok || right.Operator != parser.OpAnd {
		t.Fatalf("OR right = %#v, want AND", root.Right)
	}
	if selectStatement.Projections[0].Alias.Canonical() != "display" {
		t.Fatalf("projection alias = %#v", selectStatement.Projections[0].Alias)
	}
}

func TestParseDiagnosticsGolden(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{"", "line 1, column 1: expected CREATE, INSERT, SELECT, UPDATE, DELETE, or EXPLAIN, found end of input"},
		{"CREATE users", `line 1, column 8: expected TABLE or [UNIQUE] INDEX, found identifier "users"`},
		{"SELECT FROM users", `line 1, column 8: expected expression, found FROM "FROM"`},
		{"INSERT INTO users VALUES (1,)", `line 1, column 29: expected expression, found ) ")"`},
	}
	for _, test := range tests {
		t.Run(test.source, func(t *testing.T) {
			_, err := parser.Parse(test.source)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Parse() error = %q, want %q", err, test.want)
			}
			if !errors.Is(err, dberrors.ErrSyntax) {
				t.Fatalf("Parse() error = %v, want syntax category", err)
			}
		})
	}
}

func TestParseKnownFutureStatementIsUnsupported(t *testing.T) {
	_, err := parser.Parse("DROP TABLE users")
	if !errors.Is(err, dberrors.ErrUnsupported) {
		t.Fatalf("Parse() error = %v, want unsupported category", err)
	}
}

func TestParseM4Statements(t *testing.T) {
	tests := []struct {
		sql        string
		parameters int
		check      func(parser.Statement) bool
	}{
		{"CREATE UNIQUE INDEX users_email ON users (email)", 0, func(statement parser.Statement) bool { return statement.(*parser.CreateIndexStatement).Unique }},
		{"UPDATE users SET name = ? WHERE id = ?", 2, func(statement parser.Statement) bool {
			return len(statement.(*parser.UpdateStatement).Assignments) == 1
		}},
		{"DELETE FROM users WHERE id = ?", 1, func(statement parser.Statement) bool { return statement.(*parser.DeleteStatement).Where != nil }},
		{"EXPLAIN SELECT * FROM users WHERE id = ?", 1, func(statement parser.Statement) bool { return statement.(*parser.ExplainStatement).Select != nil }},
	}
	for _, test := range tests {
		statement, err := parser.Parse(test.sql)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.sql, err)
		}
		if !test.check(statement) || parser.ParameterCount(statement) != test.parameters {
			t.Fatalf("Parse(%q) = %#v, parameters=%d", test.sql, statement, parser.ParameterCount(statement))
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"SELECT * FROM users",
		"CREATE TABLE t (id INT64 PRIMARY KEY)",
		"INSERT INTO t VALUES (?)",
		"SELECT id FROM t WHERE id = 1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		_, _ = parser.Parse(source)
	})
}
