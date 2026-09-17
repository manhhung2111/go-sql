package parser

type TokenType int

const (
	// Special
	EOF TokenType = iota
	ILLEGAL

	// Literals
	IDENT // column names, table names, aliases
	STRING
	NUMBER

	// Reserved words
	SELECT
	FROM
	WHERE
	AND
	OR
	NOT

	// Operators
	EQ
	NEQ
	LT
	LTE
	GT
	GTE

	// Punctuation
	COMMA
	SEMICOLON
	LPAREN
	RPAREN
	ASTERISK
)

var tokenTypeMapping = map[string]TokenType{
	";": SEMICOLON, ",": COMMA, "(": LPAREN, ")": RPAREN, "*": ASTERISK,
	"=": EQ, "!=": NEQ, "<>": NEQ, "<": LT, "<=": LTE, ">": GT, ">=": GTE,
	"SELECT": SELECT, "FROM": FROM, "WHERE": WHERE, "AND": AND, "OR": OR, "NOT": NOT,
}

type Token struct {
	Type     TokenType
	Value    string
	Position int
}

type SqlStatement interface{}

type SelectStatement struct {
	Columns []string
	Table   string
}
