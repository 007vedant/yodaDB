package exec_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/exec"
	"yodadb/sql/parser"
	"yodadb/types"
)

func TestScanFilterProjectionPipeline(t *testing.T) {
	rows := []types.Row{
		{types.Int64(1), types.Text("Ada")},
		{types.Int64(2), types.Text("Grace")},
	}
	id := &binder.Expression{Kind: binder.ColumnExpression, Type: types.Int64Kind, ColumnIndex: 0}
	predicate := &binder.Expression{
		Kind: binder.BinaryExpression, Type: types.BoolKind, Operator: parser.OpEqual,
		Left: id, Right: &binder.Expression{Kind: binder.LiteralExpression, Type: types.Int64Kind, Value: types.Int64(1)},
	}
	name := &binder.Expression{Kind: binder.ColumnExpression, Type: types.TextKind, ColumnIndex: 1}
	operator := exec.NewProjection(exec.NewFilter(exec.NewScan(rows), predicate), []*binder.Expression{name})
	if err := operator.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	row, err := operator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := row[0].TextValue(); got != "Ada" {
		t.Fatalf("projected value = %q, want Ada", got)
	}
	if _, err := operator.Next(context.Background()); err != io.EOF {
		t.Fatalf("second Next error = %v, want EOF", err)
	}
}

func TestFilterTreatsNullAsNotSelected(t *testing.T) {
	predicate := &binder.Expression{Kind: binder.LiteralExpression, Type: types.NullKind, Nullable: true, Value: types.Null()}
	operator := exec.NewFilter(exec.NewValues([]types.Row{{types.Int64(1)}}), predicate)
	if err := operator.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	if _, err := operator.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next error = %v, want EOF", err)
	}
}

func TestOperatorCancellationAndIdempotentClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	operator := exec.NewValues([]types.Row{{types.Int64(1)}})
	if err := operator.Open(ctx); !errors.Is(err, dberrors.ErrCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want categorized context cancellation", err)
	}
	if err := operator.Close(); err != nil {
		t.Fatal(err)
	}
	if err := operator.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestBooleanThreeValuedLogic(t *testing.T) {
	tests := []struct {
		operator parser.Operator
		left     types.Value
		right    types.Value
		want     types.Value
	}{
		{parser.OpAnd, types.Bool(false), types.Null(), types.Bool(false)},
		{parser.OpAnd, types.Bool(true), types.Null(), types.Null()},
		{parser.OpOr, types.Bool(true), types.Null(), types.Bool(true)},
		{parser.OpOr, types.Bool(false), types.Null(), types.Null()},
	}
	for _, test := range tests {
		expression := &binder.Expression{
			Kind: binder.BinaryExpression, Type: types.BoolKind, Operator: test.operator,
			Left:  &binder.Expression{Kind: binder.LiteralExpression, Type: test.left.Kind(), Value: test.left},
			Right: &binder.Expression{Kind: binder.LiteralExpression, Type: test.right.Kind(), Value: test.right},
		}
		got, err := exec.Evaluate(expression, nil)
		if err != nil {
			t.Fatal(err)
		}
		comparison, err := types.Compare(got, test.want)
		if err != nil || comparison != 0 {
			t.Fatalf("%v %s %v = %v, want %v (comparison error %v)", test.left, test.operator, test.right, got, test.want, err)
		}
	}
}
