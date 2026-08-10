package lexer_test

import (
	"testing"

	"pebbledb/sql/lexer"
)

func TestTokenizeSQLQuotingCommentsAndOperators(t *testing.T) {
	input := "-- heading\nSELECT \"Odd\"\"Name\", 'Reuben''s', age >= 21 /* adult */ FROM users WHERE active != false;"
	tokens, err := lexer.Tokenize(input)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := []struct {
		kind    lexer.Kind
		literal string
		quoted  bool
	}{
		{lexer.Identifier, "select", false},
		{lexer.Identifier, "Odd\"Name", true},
		{lexer.Comma, ",", false},
		{lexer.String, "Reuben's", false},
		{lexer.Comma, ",", false},
		{lexer.Identifier, "age", false},
		{lexer.GreaterEqual, ">=", false},
		{lexer.Number, "21", false},
		{lexer.Identifier, "from", false},
		{lexer.Identifier, "users", false},
		{lexer.Identifier, "where", false},
		{lexer.Identifier, "active", false},
		{lexer.NotEqual, "!=", false},
		{lexer.Identifier, "false", false},
		{lexer.Semicolon, ";", false},
		{lexer.EOF, "", false},
	}
	if len(tokens) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(tokens), len(want), tokens)
	}
	for index := range want {
		if tokens[index].Kind != want[index].kind || tokens[index].Literal != want[index].literal || tokens[index].Quoted != want[index].quoted {
			t.Fatalf("token %d = %+v, want %+v", index, tokens[index], want[index])
		}
	}
	if tokens[0].Position.Line != 2 || tokens[0].Position.Column != 1 {
		t.Fatalf("SELECT position = %+v", tokens[0].Position)
	}
}

func TestLexerReportsMalformedInput(t *testing.T) {
	for _, input := range []string{"SELECT 'unterminated", "/* unterminated", "SELECT 1.", string([]byte{0xff})} {
		if _, err := lexer.Tokenize(input); err == nil {
			t.Fatalf("expected %q to fail", input)
		}
	}
}
