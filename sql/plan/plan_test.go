package plan_test

import (
	"testing"

	"yodadb/catalog"
	"yodadb/sql/binder"
	"yodadb/sql/parser"
	"yodadb/sql/plan"
	"yodadb/types"
)

func TestChooseSelectDeterministically(t *testing.T) {
	schema := catalog.New()
	table, err := schema.CreateTable("users", []catalog.ColumnSpec{
		{Name: "id", Type: types.Int64Kind, PrimaryKey: true},
		{Name: "email", Type: types.TextKind},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.CreateIndex(table.ID, "users_email", []catalog.ColumnID{table.Columns[1].ID}, false); err != nil {
		t.Fatal(err)
	}

	selected := &binder.Select{Table: table, Where: comparison(1, parser.OpEqual, types.Text("a@example.com"))}
	got := plan.ChooseSelect(selected, schema)
	if got.Kind != plan.SecondaryIndexLookup || got.Description != "INDEX LOOKUP users_email ON users(email)" {
		t.Fatalf("unexpected plan: %+v", got)
	}
	selected.Where = comparison(0, parser.OpGreaterEqual, types.Int64(10))
	got = plan.ChooseSelect(selected, schema)
	if got.Kind != plan.PrimaryKeyRange || got.Description != "PRIMARY KEY RANGE SCAN users(id)" {
		t.Fatalf("unexpected plan: %+v", got)
	}
}

func comparison(column int, operator parser.Operator, value types.Value) *binder.Expression {
	return &binder.Expression{Kind: binder.BinaryExpression, Type: types.BoolKind, Operator: operator,
		Left:  &binder.Expression{Kind: binder.ColumnExpression, ColumnIndex: column},
		Right: &binder.Expression{Kind: binder.LiteralExpression, Value: value}}
}
