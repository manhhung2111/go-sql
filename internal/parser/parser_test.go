package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse_UnknownCommand(t *testing.T) {
	_, err := parse(t, "MERGE users SET x")

	assert.EqualError(t, err, "command not implemented, got MERGE")
}

func TestParse_TrailingSemicolonAllowed(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users;")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{Columns: []string{"*"}, Table: "users"}, stmt)
}

func TestParse_TrailingGarbageAfterStatement(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users EXTRA")

	assert.EqualError(t, err, "unexpected token after statement, got EXTRA")
}

func TestParse_TrailingStatementAfterSemicolonRejected(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users; SELECT 1")

	assert.EqualError(t, err, "unexpected token after statement, got SELECT")
}
