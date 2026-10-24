package parser

import (
	"fmt"
	"strconv"

	"yodadb/dberrors"
	"yodadb/sql/lexer"
	"yodadb/types"
)

// Error describes a parse failure and its stable category.
type Error struct {
	Span     lexer.Span
	Expected string
	Found    lexer.Token
	Category dberrors.Code
}

// Error implements error.
func (e *Error) Error() string {
	message := fmt.Sprintf("line %d, column %d: expected %s, found %s", e.Span.Start.Line, e.Span.Start.Column, e.Expected, e.Found.Kind)
	if e.Found.Lexeme != "" {
		message += fmt.Sprintf(" %q", e.Found.Lexeme)
	}
	return message
}

// Unwrap returns the stable yodaDB error category.
func (e *Error) Unwrap() error {
	return dberrors.New(e.Category, "sql.parse", "")
}

// Parse parses exactly one SQL statement with an optional trailing semicolon.
func Parse(source string) (Statement, error) {
	tokens, err := lexer.Tokenize(source)
	if err != nil {
		return nil, err
	}
	p := parser{tokens: tokens}
	statement, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.match(lexer.Semicolon) {
		// A single trailing semicolon is accepted.
	}
	if _, err := p.expect(lexer.EOF, "end of input"); err != nil {
		return nil, err
	}
	return statement, nil
}

type parser struct {
	tokens        []lexer.Token
	current       int
	nextParameter int
}

func (p *parser) parseStatement() (Statement, error) {
	switch p.peek().Kind {
	case lexer.Create:
		return p.parseCreate()
	case lexer.Insert:
		return p.parseInsert()
	case lexer.Select:
		return p.parseSelect()
	case lexer.Update:
		return p.parseUpdate()
	case lexer.Delete:
		return p.parseDelete()
	case lexer.Explain:
		return p.parseExplain()
	case lexer.Begin, lexer.Commit, lexer.Rollback:
		return p.parseTransaction()
	case lexer.Drop, lexer.Alter:
		found := p.advance()
		return nil, &Error{Span: found.Span, Expected: "a supported SQL statement", Found: found, Category: dberrors.CodeUnsupported}
	default:
		return nil, p.expected("CREATE, INSERT, SELECT, UPDATE, DELETE, EXPLAIN, BEGIN, COMMIT, or ROLLBACK")
	}
}

func (p *parser) parseTransaction() (Statement, error) {
	token := p.advance()
	action := BeginTransaction
	switch token.Kind {
	case lexer.Commit:
		action = CommitTransaction
	case lexer.Rollback:
		action = RollbackTransaction
	}
	return &TransactionStatement{Span: token.Span, Action: action}, nil
}

func (p *parser) parseCreate() (Statement, error) {
	start := p.advance().Span.Start
	if p.match(lexer.Table) {
		return p.parseCreateTable(start)
	}
	unique := p.match(lexer.Unique)
	if _, err := p.expect(lexer.Index, "TABLE or [UNIQUE] INDEX"); err != nil {
		return nil, err
	}
	return p.parseCreateIndex(start, unique)
}

func (p *parser) parseCreateTable(start lexer.Position) (Statement, error) {
	table, err := p.parseIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.LeftParen, "'('"); err != nil {
		return nil, err
	}

	columns := make([]ColumnDefinition, 0, 4)
	for {
		column, err := p.parseColumnDefinition()
		if err != nil {
			return nil, err
		}
		columns = append(columns, column)
		if !p.match(lexer.Comma) {
			break
		}
	}
	end, err := p.expect(lexer.RightParen, "')'")
	if err != nil {
		return nil, err
	}
	return &CreateTableStatement{Span: span(start, end.Span.End), Table: table, Columns: columns}, nil
}

func (p *parser) parseCreateIndex(start lexer.Position, unique bool) (Statement, error) {
	name, err := p.parseIdentifier("index name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.On, "ON"); err != nil {
		return nil, err
	}
	table, err := p.parseIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.LeftParen, "'('"); err != nil {
		return nil, err
	}
	columns := make([]Identifier, 0, 2)
	for {
		column, err := p.parseIdentifier("index column")
		if err != nil {
			return nil, err
		}
		columns = append(columns, column)
		if !p.match(lexer.Comma) {
			break
		}
	}
	end, err := p.expect(lexer.RightParen, "')'")
	if err != nil {
		return nil, err
	}
	return &CreateIndexStatement{Span: span(start, end.Span.End), Name: name, Table: table, Columns: columns, Unique: unique}, nil
}

func (p *parser) parseColumnDefinition() (ColumnDefinition, error) {
	name, err := p.parseIdentifier("column name")
	if err != nil {
		return ColumnDefinition{}, err
	}
	typeName, err := p.parseIdentifier("column type")
	if err != nil {
		return ColumnDefinition{}, err
	}
	column := ColumnDefinition{Name: name, TypeName: typeName, Span: span(name.Span.Start, typeName.Span.End)}
	for {
		switch {
		case p.match(lexer.Primary):
			key, err := p.expect(lexer.Key, "KEY")
			if err != nil {
				return ColumnDefinition{}, err
			}
			column.PrimaryKey = true
			column.NotNull = true
			column.Span.End = key.Span.End
		case p.match(lexer.Not):
			nullToken, err := p.expect(lexer.Null, "NULL")
			if err != nil {
				return ColumnDefinition{}, err
			}
			column.NotNull = true
			column.Span.End = nullToken.Span.End
		default:
			return column, nil
		}
	}
}

func (p *parser) parseInsert() (Statement, error) {
	start := p.advance().Span.Start
	if _, err := p.expect(lexer.Into, "INTO"); err != nil {
		return nil, err
	}
	table, err := p.parseIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.Values, "VALUES"); err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.LeftParen, "'('"); err != nil {
		return nil, err
	}
	values := make([]Expression, 0, 4)
	for {
		value, err := p.parseExpression(precedenceLowest)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		if !p.match(lexer.Comma) {
			break
		}
	}
	end, err := p.expect(lexer.RightParen, "')'")
	if err != nil {
		return nil, err
	}
	return &InsertStatement{Span: span(start, end.Span.End), Table: table, Values: values}, nil
}

func (p *parser) parseSelect() (Statement, error) {
	start := p.advance().Span.Start
	projections := make([]SelectItem, 0, 4)
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		projections = append(projections, item)
		if !p.match(lexer.Comma) {
			break
		}
	}
	if _, err := p.expect(lexer.From, "FROM"); err != nil {
		return nil, err
	}
	table, err := p.parseIdentifier("table name")
	if err != nil {
		return nil, err
	}
	end := table.Span.End
	var asOfTxnID *uint64
	if p.match(lexer.As) {
		if _, err := p.expect(lexer.Of, "OF"); err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.Transaction, "TRANSACTION"); err != nil {
			return nil, err
		}
		token, err := p.expect(lexer.Integer, "transaction ID")
		if err != nil {
			return nil, err
		}
		txnID, err := strconv.ParseUint(token.Value, 10, 64)
		if err != nil {
			return nil, p.invalidLiteral(token, "transaction ID")
		}
		asOfTxnID = &txnID
		end = token.Span.End
	}
	var where Expression
	if p.match(lexer.Where) {
		where, err = p.parseExpression(precedenceLowest)
		if err != nil {
			return nil, err
		}
		end = where.SourceSpan().End
	}
	return &SelectStatement{Span: span(start, end), Table: table, Projections: projections, AsOfTxnID: asOfTxnID, Where: where}, nil
}

func (p *parser) parseUpdate() (Statement, error) {
	start := p.advance().Span.Start
	table, err := p.parseIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.Set, "SET"); err != nil {
		return nil, err
	}
	assignments := make([]Assignment, 0, 2)
	for {
		column, err := p.parseIdentifier("column name")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.Equal, "'='"); err != nil {
			return nil, err
		}
		value, err := p.parseExpression(precedenceLowest)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, Assignment{Span: span(column.Span.Start, value.SourceSpan().End), Column: column, Value: value})
		if !p.match(lexer.Comma) {
			break
		}
	}
	end := assignments[len(assignments)-1].Span.End
	var where Expression
	if p.match(lexer.Where) {
		where, err = p.parseExpression(precedenceLowest)
		if err != nil {
			return nil, err
		}
		end = where.SourceSpan().End
	}
	return &UpdateStatement{Span: span(start, end), Table: table, Assignments: assignments, Where: where}, nil
}

func (p *parser) parseDelete() (Statement, error) {
	start := p.advance().Span.Start
	if _, err := p.expect(lexer.From, "FROM"); err != nil {
		return nil, err
	}
	table, err := p.parseIdentifier("table name")
	if err != nil {
		return nil, err
	}
	end := table.Span.End
	var where Expression
	if p.match(lexer.Where) {
		where, err = p.parseExpression(precedenceLowest)
		if err != nil {
			return nil, err
		}
		end = where.SourceSpan().End
	}
	return &DeleteStatement{Span: span(start, end), Table: table, Where: where}, nil
}

func (p *parser) parseExplain() (Statement, error) {
	start := p.advance().Span.Start
	if p.peek().Kind != lexer.Select {
		return nil, p.expected("SELECT")
	}
	statement, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	selected := statement.(*SelectStatement)
	return &ExplainStatement{Span: span(start, selected.Span.End), Select: selected}, nil
}

func (p *parser) parseSelectItem() (SelectItem, error) {
	if token, ok := p.take(lexer.Star); ok {
		return SelectItem{Span: token.Span, Star: true}, nil
	}
	expression, err := p.parseExpression(precedenceLowest)
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{Span: expression.SourceSpan(), Expr: expression}
	if p.match(lexer.As) {
		alias, err := p.parseIdentifier("projection alias")
		if err != nil {
			return SelectItem{}, err
		}
		item.Alias = &alias
		item.Span.End = alias.Span.End
	}
	return item, nil
}

const (
	precedenceLowest = iota
	precedenceOr
	precedenceAnd
	precedenceComparison
)

func (p *parser) parseExpression(minimum int) (Expression, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for {
		if p.peek().Kind == lexer.Is && precedenceComparison >= minimum {
			p.advance()
			not := p.match(lexer.Not)
			nullToken, err := p.expect(lexer.Null, "NULL")
			if err != nil {
				return nil, err
			}
			left = &IsNullExpression{Span: span(left.SourceSpan().Start, nullToken.Span.End), Expr: left, Not: not}
			continue
		}

		operator, precedence, ok := binaryOperator(p.peek().Kind)
		if !ok || precedence < minimum {
			break
		}
		p.advance()
		right, err := p.parseExpression(precedence + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpression{Span: span(left.SourceSpan().Start, right.SourceSpan().End), Operator: operator, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parsePrimary() (Expression, error) {
	token := p.advance()
	switch token.Kind {
	case lexer.Integer:
		value, err := strconv.ParseInt(token.Value, 10, 64)
		if err != nil {
			return nil, p.invalidLiteral(token, "INT64 literal")
		}
		return &LiteralExpression{Span: token.Span, Value: types.Int64(value)}, nil
	case lexer.Float:
		value, err := strconv.ParseFloat(token.Value, 64)
		if err != nil {
			return nil, p.invalidLiteral(token, "FLOAT64 literal")
		}
		return &LiteralExpression{Span: token.Span, Value: types.Float64(value)}, nil
	case lexer.String:
		return &LiteralExpression{Span: token.Span, Value: types.Text(token.Value)}, nil
	case lexer.True:
		return &LiteralExpression{Span: token.Span, Value: types.Bool(true)}, nil
	case lexer.False:
		return &LiteralExpression{Span: token.Span, Value: types.Bool(false)}, nil
	case lexer.Null:
		return &LiteralExpression{Span: token.Span, Value: types.Null()}, nil
	case lexer.Identifier, lexer.QuotedIdentifier:
		identifier := identifierFromToken(token)
		return &IdentifierExpression{Span: token.Span, Name: identifier}, nil
	case lexer.Parameter:
		parameter := &ParameterExpression{Span: token.Span, Index: p.nextParameter}
		p.nextParameter++
		return parameter, nil
	case lexer.LeftParen:
		expression, err := p.parseExpression(precedenceLowest)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RightParen, "')'"); err != nil {
			return nil, err
		}
		return expression, nil
	case lexer.Plus, lexer.Minus:
		return p.parseSignedNumber(token)
	default:
		p.current--
		return nil, p.expected("expression")
	}
}

func (p *parser) parseSignedNumber(sign lexer.Token) (Expression, error) {
	number := p.advance()
	text := number.Value
	if sign.Kind == lexer.Minus {
		text = "-" + text
	}
	combined := span(sign.Span.Start, number.Span.End)
	switch number.Kind {
	case lexer.Integer:
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, p.invalidLiteral(number, "INT64 literal")
		}
		return &LiteralExpression{Span: combined, Value: types.Int64(value)}, nil
	case lexer.Float:
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, p.invalidLiteral(number, "FLOAT64 literal")
		}
		return &LiteralExpression{Span: combined, Value: types.Float64(value)}, nil
	default:
		p.current--
		return nil, p.expected("numeric literal after sign")
	}
}

func binaryOperator(kind lexer.Kind) (Operator, int, bool) {
	switch kind {
	case lexer.Or:
		return OpOr, precedenceOr, true
	case lexer.And:
		return OpAnd, precedenceAnd, true
	case lexer.Equal:
		return OpEqual, precedenceComparison, true
	case lexer.NotEqual:
		return OpNotEqual, precedenceComparison, true
	case lexer.Less:
		return OpLess, precedenceComparison, true
	case lexer.LessEqual:
		return OpLessEqual, precedenceComparison, true
	case lexer.Greater:
		return OpGreater, precedenceComparison, true
	case lexer.GreaterEqual:
		return OpGreaterEqual, precedenceComparison, true
	default:
		return 0, 0, false
	}
}

func (p *parser) parseIdentifier(expected string) (Identifier, error) {
	token := p.advance()
	if token.Kind != lexer.Identifier && token.Kind != lexer.QuotedIdentifier {
		p.current--
		return Identifier{}, p.expected(expected)
	}
	return identifierFromToken(token), nil
}

func identifierFromToken(token lexer.Token) Identifier {
	return Identifier{Name: token.Value, Quoted: token.Kind == lexer.QuotedIdentifier, Span: token.Span}
}

func (p *parser) expect(kind lexer.Kind, expected string) (lexer.Token, error) {
	if token, ok := p.take(kind); ok {
		return token, nil
	}
	return lexer.Token{}, p.expected(expected)
}

func (p *parser) expected(expected string) *Error {
	found := p.peek()
	return &Error{Span: found.Span, Expected: expected, Found: found, Category: dberrors.CodeSyntax}
}

func (p *parser) invalidLiteral(found lexer.Token, expected string) *Error {
	return &Error{Span: found.Span, Expected: expected + " within range", Found: found, Category: dberrors.CodeSyntax}
}

func (p *parser) match(kind lexer.Kind) bool {
	_, ok := p.take(kind)
	return ok
}

func (p *parser) take(kind lexer.Kind) (lexer.Token, bool) {
	if p.peek().Kind != kind {
		return lexer.Token{}, false
	}
	return p.advance(), true
}

func (p *parser) peek() lexer.Token {
	if p.current >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.current]
}

func (p *parser) advance() lexer.Token {
	token := p.peek()
	if p.current < len(p.tokens) {
		p.current++
	}
	return token
}

func span(start lexer.Position, end lexer.Position) lexer.Span {
	return lexer.Span{Start: start, End: end}
}

// ParameterCount returns the number of positional parameters in a parsed
// statement.
func ParameterCount(statement Statement) int {
	count := 0
	var walk func(Expression)
	walk = func(expression Expression) {
		if expression == nil {
			return
		}
		switch expression := expression.(type) {
		case *ParameterExpression:
			if expression.Index >= count {
				count = expression.Index + 1
			}
		case *BinaryExpression:
			walk(expression.Left)
			walk(expression.Right)
		case *IsNullExpression:
			walk(expression.Expr)
		}
	}
	switch statement := statement.(type) {
	case *InsertStatement:
		for _, expression := range statement.Values {
			walk(expression)
		}
	case *SelectStatement:
		for _, item := range statement.Projections {
			walk(item.Expr)
		}
		walk(statement.Where)
	case *UpdateStatement:
		for _, assignment := range statement.Assignments {
			walk(assignment.Value)
		}
		walk(statement.Where)
	case *DeleteStatement:
		walk(statement.Where)
	case *ExplainStatement:
		return ParameterCount(statement.Select)
	}
	return count
}
