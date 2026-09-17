package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func parse(t *testing.T, sql string) (SqlStatement, error) {
	t.Helper()
	tokens := NewLexer(sql).Lexing()
	return NewParser(tokens).Parse()
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
