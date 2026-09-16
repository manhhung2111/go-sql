package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLexing(t *testing.T) {
	sqlLexer := NewLexer("select * from users")
	assert.Equal(t, []Token{
		{Type: SELECT, Value: "select", Position: 0},
		{Type: ASTERISK, Value: "*", Position: 7},
		{Type: FROM, Value: "from", Position: 9},
		{Type: IDENT, Value: "users", Position: 14},
		{Type: EOF, Value: "EOF", Position: 19},
	}, sqlLexer.Lexing())
}

func TestLexing_MultipleWhiteSpaces(t *testing.T) {
	sqlLexer := NewLexer("select     * from     users")
	assert.Equal(t, []Token{
		{Type: SELECT, Value: "select", Position: 0},
		{Type: ASTERISK, Value: "*", Position: 11},
		{Type: FROM, Value: "from", Position: 13},
		{Type: IDENT, Value: "users", Position: 22},
		{Type: EOF, Value: "EOF", Position: 27},
	}, sqlLexer.Lexing())
}

func TestLexing_TrailingWhitespace(t *testing.T) {
	sqlLexer := NewLexer("select * from users   ")
	assert.Equal(t, []Token{
		{Type: SELECT, Value: "select", Position: 0},
		{Type: ASTERISK, Value: "*", Position: 7},
		{Type: FROM, Value: "from", Position: 9},
		{Type: IDENT, Value: "users", Position: 14},
		{Type: EOF, Value: "EOF", Position: 22},
	}, sqlLexer.Lexing())
}

func TestLexing_EmptyStatement(t *testing.T) {
	sqlLexer := NewLexer("")
	assert.Equal(t, []Token{
		{Type: EOF, Value: "EOF", Position: 0},
	}, sqlLexer.Lexing())
}

func TestLexing_BlankStatement(t *testing.T) {
	sqlLexer := NewLexer("    ")
	assert.Equal(t, []Token{
		{Type: EOF, Value: "EOF", Position: 4},
	}, sqlLexer.Lexing())
}

func TestLexing_KeywordsAreCaseInsensitive(t *testing.T) {
	sqlLexer := NewLexer("SELECT * FROM users WHERE id = 1 AND name != 'a' OR NOT flag")
	tokens := sqlLexer.Lexing()

	expectedTypes := []TokenType{
		SELECT, ASTERISK, FROM, IDENT, WHERE, IDENT, EQ, NUMBER,
		AND, IDENT, NEQ, STRING, OR, NOT, IDENT, EOF,
	}
	assert.Len(t, tokens, len(expectedTypes))
	for i, expected := range expectedTypes {
		assert.Equal(t, expected, tokens[i].Type, "token %d (%q)", i, tokens[i].Value)
	}
}

func TestLexing_Punctuation(t *testing.T) {
	sqlLexer := NewLexer("( ) , ; = != <> < <= > >=")
	tokens := sqlLexer.Lexing()

	expectedTypes := []TokenType{
		LPAREN, RPAREN, COMMA, SEMICOLON, EQ, NEQ, NEQ, LT, LTE, GT, GTE, EOF,
	}
	assert.Len(t, tokens, len(expectedTypes))
	for i, expected := range expectedTypes {
		assert.Equal(t, expected, tokens[i].Type, "token %d (%q)", i, tokens[i].Value)
	}
}

func TestLexing_UnknownTokenIsIdent(t *testing.T) {
	sqlLexer := NewLexer("some_unrecognized_token")
	assert.Equal(t, []Token{
		{Type: IDENT, Value: "some_unrecognized_token", Position: 0},
		{Type: EOF, Value: "EOF", Position: 23},
	}, sqlLexer.Lexing())
}

func TestLexing_CommaSeparatedFieldsAndComparison(t *testing.T) {
	sqlLexer := NewLexer("SELECT id, name FROM users WHERE age > 18")
	assert.Equal(t, []Token{
		{Type: SELECT, Value: "SELECT", Position: 0},
		{Type: IDENT, Value: "id", Position: 7},
		{Type: COMMA, Value: ",", Position: 9},
		{Type: IDENT, Value: "name", Position: 11},
		{Type: FROM, Value: "FROM", Position: 16},
		{Type: IDENT, Value: "users", Position: 21},
		{Type: WHERE, Value: "WHERE", Position: 27},
		{Type: IDENT, Value: "age", Position: 33},
		{Type: GT, Value: ">", Position: 37},
		{Type: NUMBER, Value: "18", Position: 39},
		{Type: EOF, Value: "EOF", Position: 41},
	}, sqlLexer.Lexing())
}

func TestLexing_TrailingSemicolonNoSpace(t *testing.T) {
	sqlLexer := NewLexer("SELECT * FROM users;")
	assert.Equal(t, []Token{
		{Type: SELECT, Value: "SELECT", Position: 0},
		{Type: ASTERISK, Value: "*", Position: 7},
		{Type: FROM, Value: "FROM", Position: 9},
		{Type: IDENT, Value: "users", Position: 14},
		{Type: SEMICOLON, Value: ";", Position: 19},
		{Type: EOF, Value: "EOF", Position: 20},
	}, sqlLexer.Lexing())
}

func TestLexing_NumberLiteral(t *testing.T) {
	sqlLexer := NewLexer("age >= 18.5")
	assert.Equal(t, []Token{
		{Type: IDENT, Value: "age", Position: 0},
		{Type: GTE, Value: ">=", Position: 4},
		{Type: NUMBER, Value: "18.5", Position: 7},
		{Type: EOF, Value: "EOF", Position: 11},
	}, sqlLexer.Lexing())
}

func TestLexing_StringLiteral(t *testing.T) {
	sqlLexer := NewLexer(`name = 'bob' AND note = "hi"`)
	assert.Equal(t, []Token{
		{Type: IDENT, Value: "name", Position: 0},
		{Type: EQ, Value: "=", Position: 5},
		{Type: STRING, Value: "bob", Position: 7},
		{Type: AND, Value: "AND", Position: 13},
		{Type: IDENT, Value: "note", Position: 17},
		{Type: EQ, Value: "=", Position: 22},
		{Type: STRING, Value: "hi", Position: 24},
		{Type: EOF, Value: "EOF", Position: 28},
	}, sqlLexer.Lexing())
}

func TestLexing_UnterminatedStringWithContent(t *testing.T) {
	sqlLexer := NewLexer("name = 'bob")
	assert.Equal(t, []Token{
		{Type: IDENT, Value: "name", Position: 0},
		{Type: EQ, Value: "=", Position: 5},
		{Type: ILLEGAL, Value: "'bob", Position: 7},
		{Type: EOF, Value: "EOF", Position: 11},
	}, sqlLexer.Lexing())
}

func TestLexing_UnterminatedStringNoContent(t *testing.T) {
	sqlLexer := NewLexer("name = '")
	assert.Equal(t, []Token{
		{Type: IDENT, Value: "name", Position: 0},
		{Type: EQ, Value: "=", Position: 5},
		{Type: ILLEGAL, Value: "'", Position: 7},
		{Type: EOF, Value: "EOF", Position: 8},
	}, sqlLexer.Lexing())
}

func TestLexing_IsReRunnable(t *testing.T) {
	sqlLexer := NewLexer("select * from users")
	first := sqlLexer.Lexing()
	second := sqlLexer.Lexing()

	assert.Equal(t, first, second)
}
