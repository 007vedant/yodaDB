package lexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"yodadb/dberrors"
)

var keywords = map[string]Kind{
	"create":   Create,
	"table":    Table,
	"insert":   Insert,
	"into":     Into,
	"values":   Values,
	"select":   Select,
	"from":     From,
	"where":    Where,
	"primary":  Primary,
	"key":      Key,
	"not":      Not,
	"null":     Null,
	"true":     True,
	"false":    False,
	"and":      And,
	"or":       Or,
	"is":       Is,
	"as":       As,
	"update":   Update,
	"delete":   Delete,
	"drop":     Drop,
	"alter":    Alter,
	"index":    Index,
	"unique":   Unique,
	"on":       On,
	"set":      Set,
	"explain":  Explain,
	"begin":    Begin,
	"commit":   Commit,
	"rollback": Rollback,
}

// Error describes a lexical failure at a source span.
type Error struct {
	Span    Span
	Message string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Span.Start.Line, e.Span.Start.Column, e.Message)
}

// Unwrap categorizes lexical failures as syntax errors.
func (e *Error) Unwrap() error { return dberrors.ErrSyntax }

// Tokenize scans SQL source and appends an EOF token.
func Tokenize(source string) ([]Token, error) {
	scanner := scanner{source: source, line: 1, column: 1}
	return scanner.scanAll()
}

type scanner struct {
	source string
	offset int
	line   int
	column int
}

func (s *scanner) scanAll() ([]Token, error) {
	tokens := make([]Token, 0, len(s.source)/3+1)
	for {
		s.skipSpaceAndComments()
		start := s.position()
		if s.offset == len(s.source) {
			tokens = append(tokens, Token{Kind: EOF, Span: Span{Start: start, End: start}})
			return tokens, nil
		}

		r, size := s.peek()
		if r == utf8.RuneError && size == 1 {
			return nil, s.syntax(start, "invalid UTF-8")
		}

		var token Token
		var err error
		switch {
		case isIdentifierStart(r):
			token = s.scanIdentifier(start)
		case unicode.IsDigit(r):
			token = s.scanNumber(start)
		default:
			token, err = s.scanSymbolOrQuoted(start, r)
		}
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
}

func (s *scanner) skipSpaceAndComments() {
	for {
		for s.offset < len(s.source) {
			r, _ := s.peek()
			if !unicode.IsSpace(r) {
				break
			}
			s.advance()
		}
		if !strings.HasPrefix(s.source[s.offset:], "--") {
			return
		}
		for s.offset < len(s.source) {
			r, _ := s.advance()
			if r == '\n' {
				break
			}
		}
	}
}

func (s *scanner) scanIdentifier(start Position) Token {
	startOffset := s.offset
	for s.offset < len(s.source) {
		r, _ := s.peek()
		if !isIdentifierContinue(r) {
			break
		}
		s.advance()
	}
	lexeme := s.source[startOffset:s.offset]
	kind := Identifier
	if keyword, ok := keywords[asciiLower(lexeme)]; ok {
		kind = keyword
	}
	return Token{Kind: kind, Lexeme: lexeme, Value: lexeme, Span: Span{Start: start, End: s.position()}}
}

func (s *scanner) scanNumber(start Position) Token {
	startOffset := s.offset
	for s.offset < len(s.source) {
		r, _ := s.peek()
		if !unicode.IsDigit(r) {
			break
		}
		s.advance()
	}
	kind := Integer
	if s.offset < len(s.source) && s.source[s.offset] == '.' {
		kind = Float
		s.advance()
		for s.offset < len(s.source) {
			r, _ := s.peek()
			if !unicode.IsDigit(r) {
				break
			}
			s.advance()
		}
	}
	lexeme := s.source[startOffset:s.offset]
	return Token{Kind: kind, Lexeme: lexeme, Value: lexeme, Span: Span{Start: start, End: s.position()}}
}

func (s *scanner) scanSymbolOrQuoted(start Position, r rune) (Token, error) {
	startOffset := s.offset
	switch r {
	case '\'':
		return s.scanDelimited(start, '\'', String, "string")
	case '"':
		return s.scanDelimited(start, '"', QuotedIdentifier, "quoted identifier")
	case '?':
		s.advance()
		return s.token(Parameter, startOffset, start, ""), nil
	case ',':
		s.advance()
		return s.token(Comma, startOffset, start, ""), nil
	case ';':
		s.advance()
		return s.token(Semicolon, startOffset, start, ""), nil
	case '(':
		s.advance()
		return s.token(LeftParen, startOffset, start, ""), nil
	case ')':
		s.advance()
		return s.token(RightParen, startOffset, start, ""), nil
	case '*':
		s.advance()
		return s.token(Star, startOffset, start, ""), nil
	case '+':
		s.advance()
		return s.token(Plus, startOffset, start, ""), nil
	case '-':
		s.advance()
		return s.token(Minus, startOffset, start, ""), nil
	case '=':
		s.advance()
		return s.token(Equal, startOffset, start, ""), nil
	case '!':
		s.advance()
		if s.consumeByte('=') {
			return s.token(NotEqual, startOffset, start, ""), nil
		}
		return Token{}, s.syntax(start, "expected '=' after '!'")
	case '<':
		s.advance()
		if s.consumeByte('=') {
			return s.token(LessEqual, startOffset, start, ""), nil
		}
		if s.consumeByte('>') {
			return s.token(NotEqual, startOffset, start, ""), nil
		}
		return s.token(Less, startOffset, start, ""), nil
	case '>':
		s.advance()
		if s.consumeByte('=') {
			return s.token(GreaterEqual, startOffset, start, ""), nil
		}
		return s.token(Greater, startOffset, start, ""), nil
	default:
		s.advance()
		return Token{}, s.syntax(start, fmt.Sprintf("unexpected character %q", r))
	}
}

func (s *scanner) scanDelimited(start Position, delimiter rune, kind Kind, description string) (Token, error) {
	startOffset := s.offset
	s.advance()
	var value strings.Builder
	for s.offset < len(s.source) {
		r, size := s.peek()
		if r == utf8.RuneError && size == 1 {
			return Token{}, s.syntax(s.position(), "invalid UTF-8")
		}
		s.advance()
		if r != delimiter {
			value.WriteRune(r)
			continue
		}
		if next, _ := s.peek(); s.offset < len(s.source) && next == delimiter {
			s.advance()
			value.WriteRune(delimiter)
			continue
		}
		if kind == QuotedIdentifier && value.Len() == 0 {
			return Token{}, s.syntax(start, "quoted identifier cannot be empty")
		}
		return s.token(kind, startOffset, start, value.String()), nil
	}
	return Token{}, s.syntax(start, "unterminated "+description)
}

func (s *scanner) token(kind Kind, startOffset int, start Position, value string) Token {
	return Token{Kind: kind, Lexeme: s.source[startOffset:s.offset], Value: value, Span: Span{Start: start, End: s.position()}}
}

func (s *scanner) consumeByte(want byte) bool {
	if s.offset < len(s.source) && s.source[s.offset] == want {
		s.advance()
		return true
	}
	return false
}

func (s *scanner) peek() (rune, int) {
	if s.offset >= len(s.source) {
		return 0, 0
	}
	return utf8.DecodeRuneInString(s.source[s.offset:])
}

func (s *scanner) advance() (rune, int) {
	r, size := s.peek()
	s.offset += size
	if r == '\n' {
		s.line++
		s.column = 1
	} else {
		s.column++
	}
	return r, size
}

func (s *scanner) position() Position {
	return Position{Offset: s.offset, Line: s.line, Column: s.column}
}

func (s *scanner) syntax(start Position, message string) *Error {
	return &Error{Span: Span{Start: start, End: s.position()}, Message: message}
}

func isIdentifierStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentifierContinue(r rune) bool {
	return isIdentifierStart(r) || unicode.IsDigit(r)
}

func asciiLower(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}
