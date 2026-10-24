package lexer_test

import (
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/sql/lexer"
)

func TestTokenizeStatementAndLocations(t *testing.T) {
	tokens, err := lexer.Tokenize("SELECT name\nFROM Users WHERE id <= 2;")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []lexer.Kind{
		lexer.Select, lexer.Identifier, lexer.From, lexer.Identifier, lexer.Where,
		lexer.Identifier, lexer.LessEqual, lexer.Integer, lexer.Semicolon, lexer.EOF,
	}
	if len(tokens) != len(wantKinds) {
		t.Fatalf("got %d tokens, want %d: %#v", len(tokens), len(wantKinds), tokens)
	}
	for index, want := range wantKinds {
		if tokens[index].Kind != want {
			t.Errorf("token %d kind = %s, want %s", index, tokens[index].Kind, want)
		}
	}
	if got := tokens[2].Span.Start; got.Line != 2 || got.Column != 1 || got.Offset != 12 {
		t.Fatalf("FROM position = %+v, want offset 12, line 2, column 1", got)
	}
}

func TestTokenizeDecodesDelimitedValues(t *testing.T) {
	tokens, err := lexer.Tokenize(`INSERT INTO "Odd""Name" VALUES ('Ada''s');`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tokens[2].Value, `Odd"Name`; got != want {
		t.Fatalf("quoted identifier = %q, want %q", got, want)
	}
	if got, want := tokens[5].Value, "Ada's"; got != want {
		t.Fatalf("string value = %q, want %q", got, want)
	}
}

func TestTokenizeKeywordsAreASCIIInsensitive(t *testing.T) {
	tokens, err := lexer.Tokenize("SeLeCt SELECTED AS OF TRANSACTION")
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Kind != lexer.Select || tokens[1].Kind != lexer.Identifier || tokens[2].Kind != lexer.As || tokens[3].Kind != lexer.Of || tokens[4].Kind != lexer.Transaction {
		t.Fatalf("token kinds = %s, %s, %s, %s, %s", tokens[0].Kind, tokens[1].Kind, tokens[2].Kind, tokens[3].Kind, tokens[4].Kind)
	}
}

func TestTokenizeReturnsStructuredErrors(t *testing.T) {
	for _, source := range []string{"'unterminated", "!", string([]byte{0xff})} {
		_, err := lexer.Tokenize(source)
		if !errors.Is(err, dberrors.ErrSyntax) {
			t.Fatalf("Tokenize(%q) error = %v, want syntax category", source, err)
		}
		var lexicalError *lexer.Error
		if !errors.As(err, &lexicalError) {
			t.Fatalf("Tokenize(%q) error %T has no lexical context", source, err)
		}
	}
}

func TestTokenizeSkipsComments(t *testing.T) {
	tokens, err := lexer.Tokenize("-- comment\nSELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Kind != lexer.Select || tokens[0].Span.Start.Line != 2 {
		t.Fatalf("first token = %+v, want SELECT on line 2", tokens[0])
	}
}

func FuzzTokenize(f *testing.F) {
	for _, seed := range []string{"", "SELECT 1", "'Ada''s'", "\xff", "-- comment\n?"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		_, _ = lexer.Tokenize(source)
	})
}
