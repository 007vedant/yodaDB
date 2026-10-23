package exec

import (
	"context"
	"fmt"
	"io"
	"sync"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/binder"
	"yodadb/sql/plan"
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
	case *binder.CreateIndex:
		return e.createIndex(statement)
	case *binder.Insert:
		return e.insert(ctx, statement)
	case *binder.Select:
		return e.selectRows(ctx, statement)
	case *binder.Update:
		return e.update(ctx, statement)
	case *binder.Delete:
		return e.deleteRows(ctx, statement)
	case *binder.Explain:
		access := plan.ChooseSelect(statement.Select, e.catalog)
		return Result{}, &QueryResult{Columns: []string{"plan"}, Rows: []types.Row{{types.Text(access.Description)}}}, nil
	default:
		return Result{}, nil, dberrors.New(dberrors.CodeUnsupported, "engine.execute", fmt.Sprintf("unsupported bound statement %T", statement))
	}
}

func (e *Engine) createIndex(statement *binder.CreateIndex) (Result, *QueryResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if statement.Unique {
		ordinals, err := columnOrdinals(statement.Table, statement.ColumnIDs)
		if err != nil {
			return Result{}, nil, err
		}
		if err := validateUniqueRows(e.rows[statement.Table.ID], ordinals); err != nil {
			return Result{}, nil, err
		}
	}
	_, err := e.catalog.CreateIndex(statement.Table.ID, statement.Name, statement.ColumnIDs, statement.Unique)
	return Result{}, nil, err
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
	candidate := append(cloneRows(e.rows[table.ID]), append(types.Row(nil), row...))
	if err := e.validateRows(table, candidate); err != nil {
		return err
	}
	e.rows[table.ID] = candidate
	return nil
}

func (e *Engine) update(ctx context.Context, statement *binder.Update) (Result, *QueryResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	candidate := cloneRows(e.rows[statement.Table.ID])
	var affected int64
	for index, row := range candidate {
		matched, err := rowMatches(statement.Where, row)
		if err != nil {
			return Result{}, nil, err
		}
		if !matched {
			continue
		}
		original := append(types.Row(nil), row...)
		for _, assignment := range statement.Assignments {
			row[assignment.ColumnIndex], err = Evaluate(assignment.Value, original)
			if err != nil {
				return Result{}, nil, err
			}
		}
		candidate[index] = row
		affected++
		if err := contextError(ctx, "engine.update"); err != nil {
			return Result{}, nil, err
		}
	}
	if err := e.validateRows(statement.Table, candidate); err != nil {
		return Result{}, nil, err
	}
	e.rows[statement.Table.ID] = candidate
	return Result{RowsAffected: affected}, nil, nil
}

func (e *Engine) deleteRows(ctx context.Context, statement *binder.Delete) (Result, *QueryResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := e.rows[statement.Table.ID]
	kept := make([]types.Row, 0, len(rows))
	var affected int64
	for _, row := range rows {
		matched, err := rowMatches(statement.Where, row)
		if err != nil {
			return Result{}, nil, err
		}
		if matched {
			affected++
		} else {
			kept = append(kept, append(types.Row(nil), row...))
		}
		if err := contextError(ctx, "engine.delete"); err != nil {
			return Result{}, nil, err
		}
	}
	e.rows[statement.Table.ID] = kept
	return Result{RowsAffected: affected}, nil, nil
}

func (e *Engine) validateRows(table catalog.Table, rows []types.Row) error {
	for _, row := range rows {
		if len(row) != len(table.Columns) {
			return dberrors.New(dberrors.CodeConstraint, "engine.write", "row width does not match table")
		}
		for index, column := range table.Columns {
			if row[index].IsNull() && column.NotNull {
				return dberrors.New(dberrors.CodeConstraint, "engine.write", fmt.Sprintf("column %q cannot be NULL", column.Name))
			}
			if !row[index].IsNull() && row[index].Kind() != column.Type {
				return dberrors.New(dberrors.CodeType, "engine.write", fmt.Sprintf("column %q expects %s", column.Name, column.Type))
			}
		}
	}
	if table.PrimaryKeyIndex >= 0 {
		if err := validateUniqueRows(rows, []int{table.PrimaryKeyIndex}); err != nil {
			return err
		}
	}
	for _, descriptor := range e.catalog.IndexesForTable(table.ID) {
		if !descriptor.Unique {
			continue
		}
		ordinals, err := columnOrdinals(table, descriptor.Columns)
		if err != nil {
			return err
		}
		if err := validateUniqueRows(rows, ordinals); err != nil {
			return err
		}
	}
	return nil
}

func validateUniqueRows(rows []types.Row, ordinals []int) error {
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		values := make(types.Row, len(ordinals))
		hasNull := false
		for index, ordinal := range ordinals {
			values[index] = row[ordinal]
			hasNull = hasNull || values[index].IsNull()
		}
		if hasNull {
			continue
		}
		key, err := types.EncodeKey(values, types.KeyLimits{})
		if err != nil {
			return err
		}
		if _, duplicate := seen[string(key)]; duplicate {
			return dberrors.New(dberrors.CodeConstraint, "engine.unique", "duplicate unique key")
		}
		seen[string(key)] = struct{}{}
	}
	return nil
}

func columnOrdinals(table catalog.Table, columns []catalog.ColumnID) ([]int, error) {
	result := make([]int, len(columns))
	for index, id := range columns {
		result[index] = -1
		for ordinal, column := range table.Columns {
			if column.ID == id {
				result[index] = ordinal
				break
			}
		}
		if result[index] < 0 {
			return nil, dberrors.New(dberrors.CodeBind, "engine.index", "index references unknown column")
		}
	}
	return result, nil
}

func rowMatches(where *binder.Expression, row types.Row) (bool, error) {
	if where == nil {
		return true, nil
	}
	value, err := Evaluate(where, row)
	if err != nil {
		return false, err
	}
	matched, ok := value.BoolValue()
	return ok && matched, nil
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
