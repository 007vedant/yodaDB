package exec

import (
	"fmt"

	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/parser"
	"yodadb/types"
)

// Evaluate evaluates a bound expression against row.
func Evaluate(expression *binder.Expression, row types.Row) (types.Value, error) {
	if expression == nil {
		return types.Value{}, dberrors.New(dberrors.CodeUnknown, "exec.evaluate", "nil expression")
	}
	switch expression.Kind {
	case binder.LiteralExpression:
		return expression.Value, nil
	case binder.ColumnExpression:
		if expression.ColumnIndex < 0 || expression.ColumnIndex >= len(row) {
			return types.Value{}, dberrors.New(dberrors.CodeUnknown, "exec.evaluate", fmt.Sprintf("column ordinal %d outside row of %d values", expression.ColumnIndex, len(row)))
		}
		return row[expression.ColumnIndex], nil
	case binder.BinaryExpression:
		left, err := Evaluate(expression.Left, row)
		if err != nil {
			return types.Value{}, err
		}
		right, err := Evaluate(expression.Right, row)
		if err != nil {
			return types.Value{}, err
		}
		if expression.Operator == parser.OpAnd || expression.Operator == parser.OpOr {
			return evaluateBoolean(expression.Operator, left, right)
		}
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		comparison, err := types.Compare(left, right)
		if err != nil {
			return types.Value{}, err
		}
		return types.Bool(comparisonResult(expression.Operator, comparison)), nil
	case binder.IsNullExpression:
		value, err := Evaluate(expression.Left, row)
		if err != nil {
			return types.Value{}, err
		}
		result := value.IsNull()
		if expression.Not {
			result = !result
		}
		return types.Bool(result), nil
	default:
		return types.Value{}, dberrors.New(dberrors.CodeUnsupported, "exec.evaluate", fmt.Sprintf("unsupported bound expression kind %d", expression.Kind))
	}
}

func evaluateBoolean(operator parser.Operator, left, right types.Value) (types.Value, error) {
	leftBool, leftOK := left.BoolValue()
	rightBool, rightOK := right.BoolValue()
	if (!leftOK && !left.IsNull()) || (!rightOK && !right.IsNull()) {
		return types.Value{}, dberrors.New(dberrors.CodeType, "exec.evaluate", fmt.Sprintf("%s operands must be BOOL", operator))
	}
	switch operator {
	case parser.OpAnd:
		if (leftOK && !leftBool) || (rightOK && !rightBool) {
			return types.Bool(false), nil
		}
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		return types.Bool(true), nil
	case parser.OpOr:
		if (leftOK && leftBool) || (rightOK && rightBool) {
			return types.Bool(true), nil
		}
		if left.IsNull() || right.IsNull() {
			return types.Null(), nil
		}
		return types.Bool(false), nil
	default:
		return types.Value{}, dberrors.New(dberrors.CodeUnsupported, "exec.evaluate", fmt.Sprintf("unsupported Boolean operator %s", operator))
	}
}

func comparisonResult(operator parser.Operator, comparison int) bool {
	switch operator {
	case parser.OpEqual:
		return comparison == 0
	case parser.OpNotEqual:
		return comparison != 0
	case parser.OpLess:
		return comparison < 0
	case parser.OpLessEqual:
		return comparison <= 0
	case parser.OpGreater:
		return comparison > 0
	case parser.OpGreaterEqual:
		return comparison >= 0
	default:
		return false
	}
}
