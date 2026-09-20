package parser

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func parse(t *testing.T, sql string) (SqlStatement, error) {
	t.Helper()
	tokens := NewLexer(sql).Lexing()
	return NewParser(tokens).Parse()
}

var operatorSymbols = map[TokenType]string{
	EQ: "=", NEQ: "!=", LT: "<", LTE: "<=", GT: ">", GTE: ">=", AND: "AND", OR: "OR",
}

// whereString renders a WHERE expression tree as a canonical string so tests
// can assert on its shape without hard-coding token Position values.
func whereString(e Expression) string {
	switch v := e.(type) {
	case nil:
		return "<nil>"
	case *ComparisonExpression:
		return fmt.Sprintf("%s %s %s", v.Left.Value, operatorSymbols[v.Operator], v.Right.Value)
	case *BinaryExpression:
		return fmt.Sprintf("(%s %s %s)", whereString(v.Left), operatorSymbols[v.Operator], whereString(v.Right))
	default:
		return fmt.Sprintf("unexpected expression type %T", e)
	}
}

func TestParse_SelectWildcard(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{
		Columns: []string{"*"},
		Table:   "users",
	}, stmt)
}

func TestParse_SelectSingleColumn(t *testing.T) {
	stmt, err := parse(t, "SELECT id FROM users")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{
		Columns: []string{"id"},
		Table:   "users",
	}, stmt)
}

func TestParse_SelectMultipleColumns(t *testing.T) {
	stmt, err := parse(t, "SELECT id, name, age FROM users")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{
		Columns: []string{"id", "name", "age"},
		Table:   "users",
	}, stmt)
}

func TestParse_MissingFrom(t *testing.T) {
	_, err := parse(t, "SELECT id")

	assert.EqualError(t, err, "expected FROM, got EOF")
}

func TestParse_TrailingCommaInColumnList(t *testing.T) {
	_, err := parse(t, "SELECT id, FROM users")

	assert.EqualError(t, err, "expected column name, got FROM")
}

func TestParse_NonIdentColumn(t *testing.T) {
	_, err := parse(t, "SELECT 1 FROM users")

	assert.EqualError(t, err, "expected column name, got 1")
}

func TestParse_NonIdentTable(t *testing.T) {
	_, err := parse(t, "SELECT * FROM 123")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_UnknownCommand(t *testing.T) {
	_, err := parse(t, "UPDATE users SET x")

	assert.EqualError(t, err, "command not implemented, got UPDATE")
}

func TestParse_WhereSingleComparison(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18")

	assert.NoError(t, err)
	ss := stmt.(SelectStatement)
	assert.Equal(t, "age > 18", whereString(ss.Where))
}

func TestParse_WhereComparisonOperators(t *testing.T) {
	tests := []struct {
		sql      string
		expected string
	}{
		{"SELECT * FROM users WHERE age = 18", "age = 18"},
		{"SELECT * FROM users WHERE age != 18", "age != 18"},
		{"SELECT * FROM users WHERE age < 18", "age < 18"},
		{"SELECT * FROM users WHERE age <= 18", "age <= 18"},
		{"SELECT * FROM users WHERE age > 18", "age > 18"},
		{"SELECT * FROM users WHERE age >= 18", "age >= 18"},
		{"SELECT * FROM users WHERE name = 'bob'", "name = bob"},
	}

	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			stmt, err := parse(t, tt.sql)
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, whereString(stmt.(SelectStatement).Where))
		})
	}
}

func TestParse_WhereAndChain(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18 AND name = 'bob'")

	assert.NoError(t, err)
	assert.Equal(t, "(age > 18 AND name = bob)", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereMultipleAndChain(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE a = 1 AND b = 2 AND c = 3")

	assert.NoError(t, err)
	assert.Equal(t, "(a = 1 AND (b = 2 AND c = 3))", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereOrChain(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18 OR age < 5")

	assert.NoError(t, err)
	assert.Equal(t, "(age > 18 OR age < 5)", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereAndBindsTighterThanOr(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18 AND name = 'bob' OR flag = 1")

	assert.NoError(t, err)
	assert.Equal(t, "((age > 18 AND name = bob) OR flag = 1)", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereMissingOperator(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age 18")

	assert.EqualError(t, err, "expected comparison operator, got 18")
}

func TestParse_WhereMissingRightOperand(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age >")

	assert.EqualError(t, err, "expected identifier, number or string, got EOF")
}

func TestParse_WhereDanglingAnd(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age > 18 AND")

	assert.EqualError(t, err, "expected identifier, number or string, got EOF")
}

func TestParse_WhereDanglingOr(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age > 18 OR")

	assert.EqualError(t, err, "expected identifier, number or string, got EOF")
}

func TestParse_WhereLeadingOperator(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE > 18")

	assert.EqualError(t, err, "expected identifier, number or string, got >")
}
