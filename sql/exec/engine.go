package exec

import (
	"context"
	"fmt"
	"io"
	"sync"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/types"
)

// Result describes a non-query execution.
type Result struct {
	RowsAffected int64
}

// QueryResult contains materialized M1 query rows and column names.
type QueryResult struct {
	Columns []string
	Rows    []types.Row
}

// Engine owns M1's ephemeral table rows. The catalog owns schema descriptors.
type Engine struct {
	mu      sync.RWMutex
	catalog *catalog.Catalog
	rows    map[catalog.TableID][]types.Row
}

// NewEngine constructs an empty in-memory execution engine using schema.
func NewEngine(schema *catalog.Catalog) *Engine {
	return &Engine{catalog: schema, rows: make(map[catalog.TableID][]types.Row)}
}

// Execute runs one bound statement. Exactly one of Result or QueryResult is
// meaningful for a successful statement.
func (e *Engine) Execute(ctx context.Context, statement binder.Statement) (Result, *QueryResult, error) {
	if err := contextError(ctx, "engine.execute"); err != nil {
		return Result{}, nil, err
	}
	switch statement := statement.(type) {
	case *binder.CreateTable:
		return e.createTable(statement)
	case *binder.Insert:
		return e.insert(ctx, statement)
	case *binder.Select:
		return e.selectRows(ctx, statement)
	default:
		return Result{}, nil, dberrors.New(dberrors.CodeUnsupported, "engine.execute", fmt.Sprintf("unsupported bound statement %T", statement))
	}
}

func (e *Engine) createTable(statement *binder.CreateTable) (Result, *QueryResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	table, err := e.catalog.CreateTable(statement.Name, statement.Columns)
	if err != nil {
		return Result{}, nil, err
	}
	e.rows[table.ID] = nil
	return Result{}, nil, nil
}

func (e *Engine) insert(ctx context.Context, statement *binder.Insert) (Result, *QueryResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	row := make(types.Row, len(statement.Values))
	for index, expression := range statement.Values {
		value, err := Evaluate(expression, nil)
		if err != nil {
			return Result{}, nil, err
		}
		row[index] = value
	}
	operator := NewInsert(NewValues([]types.Row{row}), func(ctx context.Context, row types.Row) error {
		if err := contextError(ctx, "engine.insert"); err != nil {
			return err
		}
		return e.insertRowLocked(statement.Table, row)
	})
	if err := operator.Open(ctx); err != nil {
		return Result{}, nil, err
	}
	defer operator.Close()
	countRow, err := operator.Next(ctx)
	if err != nil {
		return Result{}, nil, err
	}
	count, ok := countRow[0].Int64Value()
	if !ok {
		return Result{}, nil, dberrors.New(dberrors.CodeUnknown, "engine.insert", "insert operator returned invalid count")
	}
	return Result{RowsAffected: count}, nil, nil
}

func (e *Engine) insertRowLocked(table catalog.Table, row types.Row) error {
	if len(row) != len(table.Columns) {
		return dberrors.New(dberrors.CodeConstraint, "engine.insert", "row width does not match table")
	}
	for index, column := range table.Columns {
		if row[index].IsNull() && column.NotNull {
			return dberrors.New(dberrors.CodeConstraint, "engine.insert", fmt.Sprintf("column %q cannot be NULL", column.Name))
		}
		if !row[index].IsNull() && row[index].Kind() != column.Type {
			return dberrors.New(dberrors.CodeType, "engine.insert", fmt.Sprintf("column %q expects %s", column.Name, column.Type))
		}
	}
	if table.PrimaryKeyIndex >= 0 {
		key := row[table.PrimaryKeyIndex]
		for _, existing := range e.rows[table.ID] {
			comparison, err := types.Compare(existing[table.PrimaryKeyIndex], key)
			if err != nil {
				return err
			}
			if comparison == 0 {
				return dberrors.New(dberrors.CodeConstraint, "engine.insert", fmt.Sprintf("duplicate primary key %s", key))
			}
		}
	}
	e.rows[table.ID] = append(e.rows[table.ID], append(types.Row(nil), row...))
	return nil
}

func (e *Engine) selectRows(ctx context.Context, statement *binder.Select) (Result, *QueryResult, error) {
	e.mu.RLock()
	rows := cloneRows(e.rows[statement.Table.ID])
	e.mu.RUnlock()

	var operator Operator = NewScan(rows)
	if statement.Where != nil {
		operator = NewFilter(operator, statement.Where)
	}
	expressions := make([]*binder.Expression, len(statement.Projections))
	columns := make([]string, len(statement.Projections))
	for index, projection := range statement.Projections {
		expressions[index] = projection.Expr
		columns[index] = projection.Name
	}
	operator = NewProjection(operator, expressions)
	if err := operator.Open(ctx); err != nil {
		return Result{}, nil, err
	}
	defer operator.Close()

	resultRows := make([]types.Row, 0)
	for {
		row, err := operator.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return Result{}, nil, err
		}
		resultRows = append(resultRows, row)
	}
	return Result{}, &QueryResult{Columns: columns, Rows: resultRows}, nil
}
