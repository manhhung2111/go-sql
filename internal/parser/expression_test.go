package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
