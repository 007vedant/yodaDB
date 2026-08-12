package db

import (
	"fmt"
	"reflect"

	"yodadb/dberrors"
	"yodadb/types"
)

// Rows is a materialized M1 query result cursor.
type Rows struct {
	columns []string
	rows    []types.Row
	index   int
	closed  bool
	err     error
}

func newRows(columns []string, rows []types.Row) *Rows {
	return &Rows{columns: append([]string(nil), columns...), rows: cloneRows(rows), index: -1}
}

// Columns returns the result column names.
func (r *Rows) Columns() []string { return append([]string(nil), r.columns...) }

// Next advances to the next result row.
func (r *Rows) Next() bool {
	if r == nil || r.closed || r.err != nil {
		return false
	}
	if r.index+1 >= len(r.rows) {
		r.index = len(r.rows)
		return false
	}
	r.index++
	return true
}

// Values returns the current row as ordinary Go values. It returns nil before
// the first successful Next or after Close.
func (r *Rows) Values() []any {
	if r == nil || r.closed || r.index < 0 || r.index >= len(r.rows) {
		return nil
	}
	values := make([]any, len(r.rows[r.index]))
	for index, value := range r.rows[r.index] {
		values[index] = value.GoValue()
	}
	return values
}

// Scan copies the current row into pointers to supported Go scalar types or
// *any. NULL can be scanned into *any.
func (r *Rows) Scan(destinations ...any) error {
	if r == nil || r.closed || r.index < 0 || r.index >= len(r.rows) {
		return dberrors.New(dberrors.CodeTransactionClosed, "rows.scan", "rows are not positioned on a result")
	}
	if len(destinations) != len(r.rows[r.index]) {
		return dberrors.New(dberrors.CodeConstraint, "rows.scan", fmt.Sprintf("row has %d values but Scan received %d destinations", len(r.rows[r.index]), len(destinations)))
	}
	for index, destination := range destinations {
		if err := scanValue(r.rows[r.index][index], destination); err != nil {
			return dberrors.Wrap(err, dberrors.CodeType, "rows.scan", fmt.Sprintf("column %d", index+1))
		}
	}
	return nil
}

// Err returns the terminal cursor error, if any.
func (r *Rows) Err() error {
	if r == nil {
		return nil
	}
	return r.err
}

// Close releases the result rows and is idempotent.
func (r *Rows) Close() error {
	if r != nil {
		r.closed = true
		r.rows = nil
	}
	return nil
}

func scanValue(value types.Value, destination any) error {
	if destination == nil || reflect.ValueOf(destination).Kind() != reflect.Pointer || reflect.ValueOf(destination).IsNil() {
		return dberrors.New(dberrors.CodeType, "rows.scan_value", "destination must be a non-nil pointer")
	}
	if target, ok := destination.(*any); ok {
		*target = value.GoValue()
		return nil
	}
	if value.IsNull() {
		return dberrors.New(dberrors.CodeType, "rows.scan_value", "cannot scan NULL into a typed destination")
	}
	switch target := destination.(type) {
	case *bool:
		payload, ok := value.BoolValue()
		if !ok {
			break
		}
		*target = payload
		return nil
	case *int64:
		payload, ok := value.Int64Value()
		if !ok {
			break
		}
		*target = payload
		return nil
	case *float64:
		payload, ok := value.Float64Value()
		if !ok {
			break
		}
		*target = payload
		return nil
	case *string:
		payload, ok := value.TextValue()
		if !ok {
			break
		}
		*target = payload
		return nil
	case *[]byte:
		payload, ok := value.BytesValue()
		if !ok {
			break
		}
		*target = payload
		return nil
	}
	return dberrors.New(dberrors.CodeType, "rows.scan_value", fmt.Sprintf("cannot scan %s into %T", value.Kind(), destination))
}

func cloneRows(rows []types.Row) []types.Row {
	cloned := make([]types.Row, len(rows))
	for index, row := range rows {
		cloned[index] = append(types.Row(nil), row...)
	}
	return cloned
}
