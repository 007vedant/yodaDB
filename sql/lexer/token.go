package lexer

import "fmt"

// Position identifies a location in SQL source. Offset is zero-based; Line and
// Column are one-based.
type Position struct {
	Offset int
	Line   int
	Column int
}

// Span identifies a half-open range in SQL source.
type Span struct {
	Start Position
	End   Position
}

// Kind identifies a lexical token.
type Kind uint8

const (
	EOF Kind = iota
	Identifier
	QuotedIdentifier
	Integer
	Float
	String
	Parameter
	Comma
	Semicolon
	LeftParen
	RightParen
	Star
	Equal
	NotEqual
	Less
	LessEqual
	Greater
	GreaterEqual
	Plus
	Minus
	Create
	Table
	Insert
	Into
	Values
	Select
	From
	Where
	Primary
	Key
	Not
	Null
	True
	False
	And
	Or
	Is
	As
	Of
	Transaction
	Update
	Delete
	Drop
	Alter
	Index
	Unique
	On
	Set
	Explain
	Begin
	Commit
	Rollback
)

var kindNames = [...]string{
	EOF:              "end of input",
	Identifier:       "identifier",
	QuotedIdentifier: "quoted identifier",
	Integer:          "integer",
	Float:            "float",
	String:           "string",
	Parameter:        "?",
	Comma:            ",",
	Semicolon:        ";",
	LeftParen:        "(",
	RightParen:       ")",
	Star:             "*",
	Equal:            "=",
	NotEqual:         "!=",
	Less:             "<",
	LessEqual:        "<=",
	Greater:          ">",
	GreaterEqual:     ">=",
	Plus:             "+",
	Minus:            "-",
	Create:           "CREATE",
	Table:            "TABLE",
	Insert:           "INSERT",
	Into:             "INTO",
	Values:           "VALUES",
	Select:           "SELECT",
	From:             "FROM",
	Where:            "WHERE",
	Primary:          "PRIMARY",
	Key:              "KEY",
	Not:              "NOT",
	Null:             "NULL",
	True:             "TRUE",
	False:            "FALSE",
	And:              "AND",
	Or:               "OR",
	Is:               "IS",
	As:               "AS",
	Of:               "OF",
	Transaction:      "TRANSACTION",
	Update:           "UPDATE",
	Delete:           "DELETE",
	Drop:             "DROP",
	Alter:            "ALTER",
	Index:            "INDEX",
	Unique:           "UNIQUE",
	On:               "ON",
	Set:              "SET",
	Explain:          "EXPLAIN",
	Begin:            "BEGIN",
	Commit:           "COMMIT",
	Rollback:         "ROLLBACK",
}

// String returns a diagnostic token name.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return fmt.Sprintf("token(%d)", k)
}

// Token is one lexical unit. Lexeme is the original source text and Value is
// the decoded string or identifier text where applicable.
type Token struct {
	Kind   Kind
	Lexeme string
	Value  string
	Span   Span
}
