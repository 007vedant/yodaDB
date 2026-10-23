package parser

import (
	"strings"

	"yodadb/sql/lexer"
	"yodadb/types"
)

// Statement is a parsed SQL statement.
type Statement interface {
	SourceSpan() lexer.Span
	statementNode()
}

// Identifier retains source spelling and whether it was quoted.
type Identifier struct {
	Name   string
	Quoted bool
	Span   lexer.Span
}

// Canonical returns the catalog lookup spelling defined by ADR 0006.
func (i Identifier) Canonical() string {
	if i.Quoted {
		return i.Name
	}
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, i.Name)
}

// CreateTableStatement defines a table and its columns.
type CreateTableStatement struct {
	Span    lexer.Span
	Table   Identifier
	Columns []ColumnDefinition
}

// CreateIndexStatement defines one secondary index.
type CreateIndexStatement struct {
	Span    lexer.Span
	Name    Identifier
	Table   Identifier
	Columns []Identifier
	Unique  bool
}

func (*CreateIndexStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *CreateIndexStatement) SourceSpan() lexer.Span { return s.Span }

func (*CreateTableStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *CreateTableStatement) SourceSpan() lexer.Span { return s.Span }

// ColumnDefinition is one parsed CREATE TABLE column.
type ColumnDefinition struct {
	Span       lexer.Span
	Name       Identifier
	TypeName   Identifier
	PrimaryKey bool
	NotNull    bool
}

// InsertStatement inserts one row of expressions.
type InsertStatement struct {
	Span   lexer.Span
	Table  Identifier
	Values []Expression
}

func (*InsertStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *InsertStatement) SourceSpan() lexer.Span { return s.Span }

// SelectStatement reads expressions from one table.
type SelectStatement struct {
	Span        lexer.Span
	Table       Identifier
	Projections []SelectItem
	Where       Expression
}

// Assignment replaces one UPDATE target column.
type Assignment struct {
	Span   lexer.Span
	Column Identifier
	Value  Expression
}

// UpdateStatement updates rows matching an optional predicate.
type UpdateStatement struct {
	Span        lexer.Span
	Table       Identifier
	Assignments []Assignment
	Where       Expression
}

func (*UpdateStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *UpdateStatement) SourceSpan() lexer.Span { return s.Span }

// DeleteStatement deletes rows matching an optional predicate.
type DeleteStatement struct {
	Span  lexer.Span
	Table Identifier
	Where Expression
}

func (*DeleteStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *DeleteStatement) SourceSpan() lexer.Span { return s.Span }

// ExplainStatement requests the deterministic physical plan for a SELECT.
type ExplainStatement struct {
	Span   lexer.Span
	Select *SelectStatement
}

func (*ExplainStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *ExplainStatement) SourceSpan() lexer.Span { return s.Span }

func (*SelectStatement) statementNode() {}

// SourceSpan returns the complete statement span.
func (s *SelectStatement) SourceSpan() lexer.Span { return s.Span }

// SelectItem is one projection expression. Star requests all table columns.
type SelectItem struct {
	Span  lexer.Span
	Expr  Expression
	Alias *Identifier
	Star  bool
}

// Expression is a parsed SQL expression.
type Expression interface {
	SourceSpan() lexer.Span
	expressionNode()
}

// LiteralExpression holds a scalar literal.
type LiteralExpression struct {
	Span  lexer.Span
	Value types.Value
}

func (*LiteralExpression) expressionNode() {}

// SourceSpan returns the expression span.
func (e *LiteralExpression) SourceSpan() lexer.Span { return e.Span }

// IdentifierExpression refers to a column by name.
type IdentifierExpression struct {
	Span lexer.Span
	Name Identifier
}

func (*IdentifierExpression) expressionNode() {}

// SourceSpan returns the expression span.
func (e *IdentifierExpression) SourceSpan() lexer.Span { return e.Span }

// ParameterExpression refers to a zero-based positional ? argument.
type ParameterExpression struct {
	Span  lexer.Span
	Index int
}

func (*ParameterExpression) expressionNode() {}

// SourceSpan returns the expression span.
func (e *ParameterExpression) SourceSpan() lexer.Span { return e.Span }

// Operator identifies a SQL expression operation.
type Operator uint8

const (
	OpOr Operator = iota
	OpAnd
	OpEqual
	OpNotEqual
	OpLess
	OpLessEqual
	OpGreater
	OpGreaterEqual
)

// String returns the SQL spelling of an operator.
func (o Operator) String() string {
	switch o {
	case OpOr:
		return "OR"
	case OpAnd:
		return "AND"
	case OpEqual:
		return "="
	case OpNotEqual:
		return "!="
	case OpLess:
		return "<"
	case OpLessEqual:
		return "<="
	case OpGreater:
		return ">"
	case OpGreaterEqual:
		return ">="
	default:
		return "<operator>"
	}
}

// BinaryExpression combines two expressions.
type BinaryExpression struct {
	Span        lexer.Span
	Operator    Operator
	Left, Right Expression
}

func (*BinaryExpression) expressionNode() {}

// SourceSpan returns the expression span.
func (e *BinaryExpression) SourceSpan() lexer.Span { return e.Span }

// IsNullExpression tests whether a value is NULL.
type IsNullExpression struct {
	Span lexer.Span
	Expr Expression
	Not  bool
}

func (*IsNullExpression) expressionNode() {}

// SourceSpan returns the expression span.
func (e *IsNullExpression) SourceSpan() lexer.Span { return e.Span }
