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
	INSERT
	INTO
	VALUES
	UPDATE
	SET

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
	"INSERT": INSERT, "INTO": INTO, "VALUES": VALUES, "UPDATE": UPDATE, "SET": SET,
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
	Where   Expression
}

type InsertIntoStatement struct {
	Columns []string
	Table   string
	Values  [][]Token
}

type Assignment struct {
	Column string
	Value  Token
}

type UpdateStatement struct {
	Table string
	Set   []Assignment
	Where Expression
}

type Expression interface{}

type BinaryExpression struct {
	Left     Expression
	Right    Expression
	Operator TokenType // Must either be "AND" or "OR"
}

type ComparisonExpression struct {
	Left     Token
	Right    Token
	Operator TokenType // Must be in ["=", "!=", "<>", ">=", ">", "<=", "<"]
}
