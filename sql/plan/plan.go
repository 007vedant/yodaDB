// Package plan chooses small, deterministic physical access paths for bound SQL.
package plan

import (
	"fmt"

	"yodadb/catalog"
	"yodadb/sql/binder"
	"yodadb/sql/parser"
	"yodadb/types"
)

// AccessKind identifies the storage access used for a SELECT.
type AccessKind uint8

const (
	TableScan AccessKind = iota
	PrimaryKeyLookup
	PrimaryKeyRange
	SecondaryIndexLookup
	SecondaryIndexRange
)

// Access is the chosen access path. Predicate evaluation still happens after
// retrieval, so a bounded scan cannot change query semantics.
type Access struct {
	Kind        AccessKind
	Index       catalog.Index
	ColumnIndex int
	Operator    parser.Operator
	Value       types.Value
	Description string
}

// ChooseSelect applies stable rules: primary key first, then indexes by stable
// ID, and finally a table scan. M4 considers simple column-to-literal clauses.
func ChooseSelect(statement *binder.Select, schema *catalog.Catalog) Access {
	fallback := Access{Kind: TableScan, ColumnIndex: -1, Description: fmt.Sprintf("TABLE SCAN %s", statement.Table.Name)}
	column, operator, value, ok := searchablePredicate(statement.Where)
	if !ok || value.IsNull() || operator == parser.OpNotEqual {
		return fallback
	}
	if column == statement.Table.PrimaryKeyIndex {
		kind, label := PrimaryKeyRange, "PRIMARY KEY RANGE SCAN"
		if operator == parser.OpEqual {
			kind, label = PrimaryKeyLookup, "PRIMARY KEY LOOKUP"
		}
		return Access{Kind: kind, ColumnIndex: column, Operator: operator, Value: value,
			Description: fmt.Sprintf("%s %s(%s)", label, statement.Table.Name, statement.Table.Columns[column].Name)}
	}
	for _, descriptor := range schema.IndexesForTable(statement.Table.ID) {
		if len(descriptor.Columns) != 1 || descriptor.Columns[0] != statement.Table.Columns[column].ID {
			continue
		}
		kind, label := SecondaryIndexRange, "INDEX RANGE SCAN"
		if operator == parser.OpEqual {
			kind, label = SecondaryIndexLookup, "INDEX LOOKUP"
		}
		return Access{Kind: kind, Index: descriptor, ColumnIndex: column, Operator: operator, Value: value,
			Description: fmt.Sprintf("%s %s ON %s(%s)", label, descriptor.Name, statement.Table.Name, statement.Table.Columns[column].Name)}
	}
	return fallback
}

func searchablePredicate(expression *binder.Expression) (int, parser.Operator, types.Value, bool) {
	if expression == nil || expression.Kind != binder.BinaryExpression {
		return 0, 0, types.Value{}, false
	}
	if expression.Left.Kind == binder.ColumnExpression && expression.Right.Kind == binder.LiteralExpression {
		return expression.Left.ColumnIndex, expression.Operator, expression.Right.Value, true
	}
	if expression.Right.Kind == binder.ColumnExpression && expression.Left.Kind == binder.LiteralExpression {
		return expression.Right.ColumnIndex, reverse(expression.Operator), expression.Left.Value, true
	}
	return 0, 0, types.Value{}, false
}

func reverse(operator parser.Operator) parser.Operator {
	switch operator {
	case parser.OpLess:
		return parser.OpGreater
	case parser.OpLessEqual:
		return parser.OpGreaterEqual
	case parser.OpGreater:
		return parser.OpLess
	case parser.OpGreaterEqual:
		return parser.OpLessEqual
	default:
		return operator
	}
}
