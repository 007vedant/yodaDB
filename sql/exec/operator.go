package exec

import (
	"context"
	"io"

	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/types"
)

// Operator is a pull-based row source.
type Operator interface {
	Open(context.Context) error
	Next(context.Context) (types.Row, error)
	Close() error
}

// Values emits a fixed collection of rows.
type Values struct {
	rows   []types.Row
	index  int
	opened bool
	closed bool
}

// NewValues constructs a fixed row source.
func NewValues(rows []types.Row) *Values {
	return &Values{rows: cloneRows(rows)}
}

// Open prepares the value source.
func (v *Values) Open(ctx context.Context) error {
	if err := contextError(ctx, "values.open"); err != nil {
		return err
	}
	v.index, v.opened, v.closed = 0, true, false
	return nil
}

// Next returns the next value row.
func (v *Values) Next(ctx context.Context) (types.Row, error) {
	if err := contextError(ctx, "values.next"); err != nil {
		return nil, err
	}
	if !v.opened || v.closed {
		return nil, dberrors.New(dberrors.CodeTransactionClosed, "values.next", "operator is not open")
	}
	if v.index >= len(v.rows) {
		return nil, io.EOF
	}
	row := append(types.Row(nil), v.rows[v.index]...)
	v.index++
	return row, nil
}

// Close releases the value source and is idempotent.
func (v *Values) Close() error {
	v.closed = true
	return nil
}

// Scan emits an in-memory snapshot of table rows.
type Scan struct{ *Values }

// NewScan constructs an in-memory table scan.
func NewScan(rows []types.Row) *Scan { return &Scan{Values: NewValues(rows)} }

// Filter emits child rows for which predicate is TRUE. FALSE and NULL are not
// selected, matching SQL WHERE semantics.
type Filter struct {
	child     Operator
	predicate *binder.Expression
}

// NewFilter constructs a filtering operator.
func NewFilter(child Operator, predicate *binder.Expression) *Filter {
	return &Filter{child: child, predicate: predicate}
}

// Open opens the child operator.
func (f *Filter) Open(ctx context.Context) error { return f.child.Open(ctx) }

// Next returns the next matching row.
func (f *Filter) Next(ctx context.Context) (types.Row, error) {
	for {
		row, err := f.child.Next(ctx)
		if err != nil {
			return nil, err
		}
		value, err := Evaluate(f.predicate, row)
		if err != nil {
			return nil, err
		}
		selected, ok := value.BoolValue()
		if ok && selected {
			return row, nil
		}
		if !ok && !value.IsNull() {
			return nil, dberrors.New(dberrors.CodeType, "filter.next", "predicate did not produce BOOL")
		}
	}
}

// Close closes the child operator.
func (f *Filter) Close() error { return f.child.Close() }

// Projection evaluates result expressions for every child row.
type Projection struct {
	child       Operator
	expressions []*binder.Expression
}

// NewProjection constructs a projection operator.
func NewProjection(child Operator, expressions []*binder.Expression) *Projection {
	return &Projection{child: child, expressions: expressions}
}

// Open opens the child operator.
func (p *Projection) Open(ctx context.Context) error { return p.child.Open(ctx) }

// Next returns one projected row.
func (p *Projection) Next(ctx context.Context) (types.Row, error) {
	row, err := p.child.Next(ctx)
	if err != nil {
		return nil, err
	}
	projected := make(types.Row, len(p.expressions))
	for index, expression := range p.expressions {
		projected[index], err = Evaluate(expression, row)
		if err != nil {
			return nil, err
		}
	}
	return projected, nil
}

// Close closes the child operator.
func (p *Projection) Close() error { return p.child.Close() }

// Insert consumes rows and invokes a table-owned insertion callback.
type Insert struct {
	child  Operator
	insert func(context.Context, types.Row) error
	done   bool
}

// NewInsert constructs an insertion operator.
func NewInsert(child Operator, insert func(context.Context, types.Row) error) *Insert {
	return &Insert{child: child, insert: insert}
}

// Open opens the input and resets the operator.
func (i *Insert) Open(ctx context.Context) error {
	i.done = false
	return i.child.Open(ctx)
}

// Next consumes all input once and returns a single INT64 affected-row count.
func (i *Insert) Next(ctx context.Context) (types.Row, error) {
	if i.done {
		return nil, io.EOF
	}
	var count int64
	for {
		row, err := i.child.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := i.insert(ctx, row); err != nil {
			return nil, err
		}
		count++
	}
	i.done = true
	return types.Row{types.Int64(count)}, nil
}

// Close closes the input operator and is idempotent when the input is.
func (i *Insert) Close() error { return i.child.Close() }

func contextError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return dberrors.Wrap(err, dberrors.CodeCancelled, operation, "context cancelled")
	}
	return nil
}

func cloneRows(rows []types.Row) []types.Row {
	cloned := make([]types.Row, len(rows))
	for index, row := range rows {
		cloned[index] = append(types.Row(nil), row...)
	}
	return cloned
}
