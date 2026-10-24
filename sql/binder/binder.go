package binder

import (
	"fmt"

	"yodadb/catalog"
	"yodadb/dberrors"
	"yodadb/sql/lexer"
	"yodadb/sql/parser"
	"yodadb/types"
)

// Catalog is the read boundary the binder needs from the schema catalog.
type Catalog interface {
	LookupTable(name string) (catalog.Table, bool)
	LookupIndex(tableID catalog.TableID, name string) (catalog.Index, bool)
}

// Statement is a fully name-resolved and typed statement.
type Statement interface {
	boundStatement()
}

// TransactionControl is a bound BEGIN, COMMIT, or ROLLBACK statement.
type TransactionControl struct {
	Action parser.TransactionAction
}

func (*TransactionControl) boundStatement() {}

// CreateTable is a validated table definition ready for catalog publication.
type CreateTable struct {
	Name    string
	Columns []catalog.ColumnSpec
}

// CreateIndex is a validated secondary-index definition.
type CreateIndex struct {
	Name      string
	Table     catalog.Table
	ColumnIDs []catalog.ColumnID
	Unique    bool
}

func (*CreateIndex) boundStatement() {}

func (*CreateTable) boundStatement() {}

// Insert is a typed single-row insertion.
type Insert struct {
	Table  catalog.Table
	Values []*Expression
}

func (*Insert) boundStatement() {}

// Select is a typed single-table query.
type Select struct {
	Table       catalog.Table
	Projections []Projection
	AsOfTxnID   *uint64
	Where       *Expression
}

// Assignment is one resolved UPDATE target and value expression.
type Assignment struct {
	ColumnIndex int
	Value       *Expression
}

// Update is a bound single-table UPDATE.
type Update struct {
	Table       catalog.Table
	Assignments []Assignment
	Where       *Expression
}

func (*Update) boundStatement() {}

// Delete is a bound single-table DELETE.
type Delete struct {
	Table catalog.Table
	Where *Expression
}

func (*Delete) boundStatement() {}

// Explain is a bound SELECT whose deterministic physical plan is requested.
type Explain struct {
	Select *Select
}

func (*Explain) boundStatement() {}

func (*Select) boundStatement() {}

// Projection is one named result expression.
type Projection struct {
	Name string
	Expr *Expression
}

// ExpressionKind identifies a bound expression node.
type ExpressionKind uint8

const (
	// LiteralExpression is a constant scalar value.
	LiteralExpression ExpressionKind = iota
	// ColumnExpression reads a row column by ordinal.
	ColumnExpression
	// BinaryExpression evaluates a Boolean or comparison operator.
	BinaryExpression
	// IsNullExpression evaluates an IS [NOT] NULL test.
	IsNullExpression
)

// Expression is a resolved expression tree.
type Expression struct {
	Kind        ExpressionKind
	Type        types.Kind
	Nullable    bool
	Value       types.Value
	ColumnIndex int
	Operator    parser.Operator
	Left        *Expression
	Right       *Expression
	Not         bool
}

// Error describes a bind or type-checking failure at a source span.
type Error struct {
	Span     lexer.Span
	Category dberrors.Code
	Message  string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Span.Start.Line, e.Span.Start.Column, e.Message)
}

// Unwrap returns the stable yodaDB error category.
func (e *Error) Unwrap() error {
	return dberrors.New(e.Category, "sql.bind", "")
}

// Bind resolves and type-checks one parsed statement with positional argument
// values. The number of arguments must exactly match the number of ? tokens.
func Bind(statement parser.Statement, schema Catalog, parameters []types.Value) (Statement, error) {
	if got, want := len(parameters), parser.ParameterCount(statement); got != want {
		return nil, &Error{
			Span:     statement.SourceSpan(),
			Category: dberrors.CodeBind,
			Message:  fmt.Sprintf("statement has %d parameters but %d arguments were supplied", want, got),
		}
	}
	b := binder{catalog: schema, parameters: parameters}
	switch statement := statement.(type) {
	case *parser.CreateTableStatement:
		return b.bindCreateTable(statement)
	case *parser.CreateIndexStatement:
		return b.bindCreateIndex(statement)
	case *parser.InsertStatement:
		return b.bindInsert(statement)
	case *parser.SelectStatement:
		return b.bindSelect(statement)
	case *parser.UpdateStatement:
		return b.bindUpdate(statement)
	case *parser.DeleteStatement:
		return b.bindDelete(statement)
	case *parser.ExplainStatement:
		selected, err := b.bindSelect(statement.Select)
		if err != nil {
			return nil, err
		}
		return &Explain{Select: selected.(*Select)}, nil
	case *parser.TransactionStatement:
		return &TransactionControl{Action: statement.Action}, nil
	default:
		return nil, &Error{Span: statement.SourceSpan(), Category: dberrors.CodeUnsupported, Message: fmt.Sprintf("unsupported statement %T", statement)}
	}
}

func (b *binder) bindCreateIndex(statement *parser.CreateIndexStatement) (Statement, error) {
	table, ok := b.catalog.LookupTable(statement.Table.Canonical())
	if !ok {
		return nil, bindError(statement.Table.Span, dberrors.CodeBind, fmt.Sprintf("unknown table %q", statement.Table.Name))
	}
	name := statement.Name.Canonical()
	if _, exists := b.catalog.LookupIndex(table.ID, name); exists {
		return nil, bindError(statement.Name.Span, dberrors.CodeConstraint, fmt.Sprintf("index %q already exists", name))
	}
	columns := make([]catalog.ColumnID, len(statement.Columns))
	seen := make(map[catalog.ColumnID]struct{}, len(columns))
	for index, identifier := range statement.Columns {
		columnIndex := tableColumn(table, identifier.Canonical())
		if columnIndex < 0 {
			return nil, bindError(identifier.Span, dberrors.CodeBind, fmt.Sprintf("unknown column %q", identifier.Name))
		}
		columnID := table.Columns[columnIndex].ID
		if _, duplicate := seen[columnID]; duplicate {
			return nil, bindError(identifier.Span, dberrors.CodeConstraint, fmt.Sprintf("column %q appears more than once", identifier.Name))
		}
		seen[columnID] = struct{}{}
		columns[index] = columnID
	}
	return &CreateIndex{Name: name, Table: table, ColumnIDs: columns, Unique: statement.Unique}, nil
}

type binder struct {
	catalog    Catalog
	parameters []types.Value
}

func (b *binder) bindCreateTable(statement *parser.CreateTableStatement) (Statement, error) {
	name := statement.Table.Canonical()
	if _, exists := b.catalog.LookupTable(name); exists {
		return nil, bindError(statement.Table.Span, dberrors.CodeConstraint, fmt.Sprintf("table %q already exists", name))
	}
	if len(statement.Columns) == 0 {
		return nil, bindError(statement.Span, dberrors.CodeConstraint, "table must have at least one column")
	}

	columns := make([]catalog.ColumnSpec, len(statement.Columns))
	seen := make(map[string]struct{}, len(statement.Columns))
	primaryKeys := 0
	for index, definition := range statement.Columns {
		columnName := definition.Name.Canonical()
		if _, exists := seen[columnName]; exists {
			return nil, bindError(definition.Name.Span, dberrors.CodeConstraint, fmt.Sprintf("duplicate column %q", columnName))
		}
		seen[columnName] = struct{}{}
		kind, ok := scalarKind(definition.TypeName.Canonical())
		if !ok {
			return nil, bindError(definition.TypeName.Span, dberrors.CodeType, fmt.Sprintf("unknown column type %q", definition.TypeName.Name))
		}
		if definition.PrimaryKey {
			primaryKeys++
			if primaryKeys > 1 {
				return nil, bindError(definition.Span, dberrors.CodeConstraint, "M1 supports one primary-key column")
			}
		}
		columns[index] = catalog.ColumnSpec{
			Name:       columnName,
			Type:       kind,
			NotNull:    definition.NotNull || definition.PrimaryKey,
			PrimaryKey: definition.PrimaryKey,
		}
	}
	return &CreateTable{Name: name, Columns: columns}, nil
}

func (b *binder) bindInsert(statement *parser.InsertStatement) (Statement, error) {
	table, ok := b.catalog.LookupTable(statement.Table.Canonical())
	if !ok {
		return nil, bindError(statement.Table.Span, dberrors.CodeBind, fmt.Sprintf("unknown table %q", statement.Table.Name))
	}
	if len(statement.Values) != len(table.Columns) {
		return nil, bindError(statement.Span, dberrors.CodeConstraint, fmt.Sprintf("table %q has %d columns but INSERT supplied %d values", table.Name, len(table.Columns), len(statement.Values)))
	}

	values := make([]*Expression, len(statement.Values))
	for index, parsed := range statement.Values {
		expression, err := b.bindExpression(parsed, nil)
		if err != nil {
			return nil, err
		}
		column := table.Columns[index]
		if expression.Kind != LiteralExpression {
			return nil, bindError(parsed.SourceSpan(), dberrors.CodeUnsupported, "M1 INSERT values must be literals or parameters")
		}
		coerced, err := types.CoerceLossless(expression.Value, column.Type)
		if err != nil {
			return nil, bindError(parsed.SourceSpan(), dberrors.CodeType, fmt.Sprintf("column %q expects %s but value is %s", column.Name, column.Type, expression.Value.Kind()))
		}
		if coerced.IsNull() && column.NotNull {
			return nil, bindError(parsed.SourceSpan(), dberrors.CodeConstraint, fmt.Sprintf("column %q cannot be NULL", column.Name))
		}
		expression.Value = coerced
		expression.Type = column.Type
		expression.Nullable = coerced.IsNull()
		values[index] = expression
	}
	return &Insert{Table: table, Values: values}, nil
}

func (b *binder) bindSelect(statement *parser.SelectStatement) (Statement, error) {
	table, ok := b.catalog.LookupTable(statement.Table.Canonical())
	if !ok {
		return nil, bindError(statement.Table.Span, dberrors.CodeBind, fmt.Sprintf("unknown table %q", statement.Table.Name))
	}

	projections := make([]Projection, 0, len(statement.Projections))
	for _, item := range statement.Projections {
		if item.Star {
			for index, column := range table.Columns {
				projections = append(projections, Projection{
					Name: column.Name,
					Expr: &Expression{Kind: ColumnExpression, Type: column.Type, Nullable: !column.NotNull, ColumnIndex: index},
				})
			}
			continue
		}
		expression, err := b.bindExpression(item.Expr, &table)
		if err != nil {
			return nil, err
		}
		name := projectionName(item, expression, table)
		projections = append(projections, Projection{Name: name, Expr: expression})
	}

	var where *Expression
	if statement.Where != nil {
		var err error
		where, err = b.bindExpression(statement.Where, &table)
		if err != nil {
			return nil, err
		}
		if where.Type == types.NullKind && where.Kind == LiteralExpression {
			where.Type = types.BoolKind
		}
		if where.Type != types.BoolKind {
			return nil, bindError(statement.Where.SourceSpan(), dberrors.CodeType, fmt.Sprintf("WHERE expression must be BOOL, got %s", where.Type))
		}
	}
	return &Select{Table: table, Projections: projections, AsOfTxnID: cloneTxnID(statement.AsOfTxnID), Where: where}, nil
}

func cloneTxnID(txnID *uint64) *uint64 {
	if txnID == nil {
		return nil
	}
	cloned := *txnID
	return &cloned
}

func (b *binder) bindUpdate(statement *parser.UpdateStatement) (Statement, error) {
	table, ok := b.catalog.LookupTable(statement.Table.Canonical())
	if !ok {
		return nil, bindError(statement.Table.Span, dberrors.CodeBind, fmt.Sprintf("unknown table %q", statement.Table.Name))
	}
	assignments := make([]Assignment, len(statement.Assignments))
	seen := make(map[int]struct{}, len(assignments))
	for index, parsed := range statement.Assignments {
		ordinal := tableColumn(table, parsed.Column.Canonical())
		if ordinal < 0 {
			return nil, bindError(parsed.Column.Span, dberrors.CodeBind, fmt.Sprintf("unknown column %q", parsed.Column.Name))
		}
		if _, duplicate := seen[ordinal]; duplicate {
			return nil, bindError(parsed.Column.Span, dberrors.CodeConstraint, fmt.Sprintf("column %q assigned more than once", parsed.Column.Name))
		}
		seen[ordinal] = struct{}{}
		value, err := b.bindExpression(parsed.Value, &table)
		if err != nil {
			return nil, err
		}
		column := table.Columns[ordinal]
		if value.Type != column.Type && value.Type != types.NullKind {
			if value.Kind != LiteralExpression {
				return nil, bindError(parsed.Value.SourceSpan(), dberrors.CodeType, fmt.Sprintf("column %q expects %s", column.Name, column.Type))
			}
			coerced, err := types.CoerceLossless(value.Value, column.Type)
			if err != nil {
				return nil, bindError(parsed.Value.SourceSpan(), dberrors.CodeType, fmt.Sprintf("column %q expects %s", column.Name, column.Type))
			}
			value.Value, value.Type = coerced, column.Type
		}
		if value.Type == types.NullKind && column.NotNull {
			return nil, bindError(parsed.Value.SourceSpan(), dberrors.CodeConstraint, fmt.Sprintf("column %q cannot be NULL", column.Name))
		}
		assignments[index] = Assignment{ColumnIndex: ordinal, Value: value}
	}
	where, err := b.bindWhere(statement.Where, table)
	if err != nil {
		return nil, err
	}
	return &Update{Table: table, Assignments: assignments, Where: where}, nil
}

func (b *binder) bindDelete(statement *parser.DeleteStatement) (Statement, error) {
	table, ok := b.catalog.LookupTable(statement.Table.Canonical())
	if !ok {
		return nil, bindError(statement.Table.Span, dberrors.CodeBind, fmt.Sprintf("unknown table %q", statement.Table.Name))
	}
	where, err := b.bindWhere(statement.Where, table)
	if err != nil {
		return nil, err
	}
	return &Delete{Table: table, Where: where}, nil
}

func (b *binder) bindWhere(parsed parser.Expression, table catalog.Table) (*Expression, error) {
	if parsed == nil {
		return nil, nil
	}
	where, err := b.bindExpression(parsed, &table)
	if err != nil {
		return nil, err
	}
	if where.Type == types.NullKind && where.Kind == LiteralExpression {
		where.Type = types.BoolKind
	}
	if where.Type != types.BoolKind {
		return nil, bindError(parsed.SourceSpan(), dberrors.CodeType, fmt.Sprintf("WHERE expression must be BOOL, got %s", where.Type))
	}
	return where, nil
}

func tableColumn(table catalog.Table, name string) int {
	for index, column := range table.Columns {
		if column.Name == name {
			return index
		}
	}
	return -1
}

func (b *binder) bindExpression(expression parser.Expression, table *catalog.Table) (*Expression, error) {
	switch expression := expression.(type) {
	case *parser.LiteralExpression:
		return &Expression{Kind: LiteralExpression, Type: expression.Value.Kind(), Nullable: expression.Value.IsNull(), Value: expression.Value}, nil
	case *parser.ParameterExpression:
		value := b.parameters[expression.Index]
		return &Expression{Kind: LiteralExpression, Type: value.Kind(), Nullable: value.IsNull(), Value: value}, nil
	case *parser.IdentifierExpression:
		if table == nil {
			return nil, bindError(expression.Span, dberrors.CodeBind, fmt.Sprintf("column %q is not available in this context", expression.Name.Name))
		}
		name := expression.Name.Canonical()
		for index, column := range table.Columns {
			if column.Name == name {
				return &Expression{Kind: ColumnExpression, Type: column.Type, Nullable: !column.NotNull, ColumnIndex: index}, nil
			}
		}
		return nil, bindError(expression.Span, dberrors.CodeBind, fmt.Sprintf("unknown column %q", expression.Name.Name))
	case *parser.BinaryExpression:
		left, err := b.bindExpression(expression.Left, table)
		if err != nil {
			return nil, err
		}
		right, err := b.bindExpression(expression.Right, table)
		if err != nil {
			return nil, err
		}
		if expression.Operator == parser.OpAnd || expression.Operator == parser.OpOr {
			if !isBooleanOrNull(left.Type) || !isBooleanOrNull(right.Type) {
				return nil, bindError(expression.Span, dberrors.CodeType, fmt.Sprintf("%s operands must be BOOL", expression.Operator))
			}
			return &Expression{Kind: BinaryExpression, Type: types.BoolKind, Nullable: left.Nullable || right.Nullable || left.Type == types.NullKind || right.Type == types.NullKind, Operator: expression.Operator, Left: left, Right: right}, nil
		}
		if err := makeComparable(left, right); err != nil {
			return nil, bindError(expression.Span, dberrors.CodeType, err.Error())
		}
		return &Expression{Kind: BinaryExpression, Type: types.BoolKind, Nullable: left.Nullable || right.Nullable || left.Type == types.NullKind || right.Type == types.NullKind, Operator: expression.Operator, Left: left, Right: right}, nil
	case *parser.IsNullExpression:
		inner, err := b.bindExpression(expression.Expr, table)
		if err != nil {
			return nil, err
		}
		return &Expression{Kind: IsNullExpression, Type: types.BoolKind, Nullable: false, Left: inner, Not: expression.Not}, nil
	default:
		return nil, bindError(expression.SourceSpan(), dberrors.CodeUnsupported, fmt.Sprintf("unsupported expression %T", expression))
	}
}

func makeComparable(left, right *Expression) error {
	if left.Type == types.NullKind || right.Type == types.NullKind || left.Type == right.Type {
		return nil
	}
	if left.Kind == LiteralExpression {
		if value, err := types.CoerceLossless(left.Value, right.Type); err == nil {
			left.Value, left.Type = value, right.Type
			return nil
		}
	}
	if right.Kind == LiteralExpression {
		if value, err := types.CoerceLossless(right.Value, left.Type); err == nil {
			right.Value, right.Type = value, left.Type
			return nil
		}
	}
	return fmt.Errorf("cannot compare %s and %s", left.Type, right.Type)
}

func projectionName(item parser.SelectItem, expression *Expression, table catalog.Table) string {
	if item.Alias != nil {
		return item.Alias.Canonical()
	}
	if expression.Kind == ColumnExpression && expression.ColumnIndex >= 0 && expression.ColumnIndex < len(table.Columns) {
		return table.Columns[expression.ColumnIndex].Name
	}
	return "expression"
}

func scalarKind(name string) (types.Kind, bool) {
	switch name {
	case "bool":
		return types.BoolKind, true
	case "int64":
		return types.Int64Kind, true
	case "float64":
		return types.Float64Kind, true
	case "text":
		return types.TextKind, true
	case "bytes":
		return types.BytesKind, true
	default:
		return 0, false
	}
}

func isBooleanOrNull(kind types.Kind) bool {
	return kind == types.BoolKind || kind == types.NullKind
}

func bindError(span lexer.Span, category dberrors.Code, message string) *Error {
	return &Error{Span: span, Category: category, Message: message}
}
