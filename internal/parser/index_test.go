package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse_CreateIndex(t *testing.T) {
	tests := []struct {
		sql  string
		want CreateIndexStatement
	}{
		{"CREATE INDEX idx_name ON users (name)", CreateIndexStatement{Name: "idx_name", Table: "users", Column: "name"}},
		{"create index idx_name on users (name);", CreateIndexStatement{Name: "idx_name", Table: "users", Column: "name"}},
		{"CREATE INDEX IF NOT EXISTS i ON t (c)", CreateIndexStatement{Name: "i", Table: "t", Column: "c", IfNotExists: true}},
	}
	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			stmt, err := parse(t, tc.sql)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, stmt)
		})
	}
}

func TestParse_DropIndex(t *testing.T) {
	tests := []struct {
		sql  string
		want DropIndexStatement
	}{
		{"DROP INDEX idx_name ON users", DropIndexStatement{Name: "idx_name", Table: "users"}},
		{"drop index if exists i on t;", DropIndexStatement{Name: "i", Table: "t", IfExists: true}},
	}
	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			stmt, err := parse(t, tc.sql)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, stmt)
		})
	}
}

func TestParse_IndexErrors(t *testing.T) {
	tests := []struct {
		sql     string
		wantErr string
	}{
		{"CREATE INDEX", "expected index name, got EOF"},
		{"CREATE INDEX 1 ON t (c)", "expected index name, got 1"},
		{"CREATE INDEX i t (c)", "expected ON after index name, got t"},
		{"CREATE INDEX i ON", "expected table name, got EOF"},
		{"CREATE INDEX i ON t c", "expected ( after table name, got c"},
		{"CREATE INDEX i ON t ()", "expected column name, got )"},
		{"CREATE INDEX i ON t (a, b)", "composite indexes are not supported"},
		{"CREATE INDEX i ON t (a", "expected ) after column name, got EOF"},
		{"CREATE INDEX i ON t (a) extra", "unexpected token after statement, got extra"},
		{"CREATE INDEX IF i ON t (a)", "expected NOT after IF, got i"},
		{"CREATE INDEX IF NOT i ON t (a)", "expected EXISTS after IF NOT, got i"},
		{"CREATE UNIQUE INDEX i ON t (a)", "DATABASE, TABLE or INDEX keyword must be expected after CREATE, got UNIQUE"},
		{"DROP INDEX", "expected index name, got EOF"},
		{"DROP INDEX i", "expected ON after index name, got EOF"},
		{"DROP INDEX i ON", "expected table name, got EOF"},
		{"DROP INDEX IF i ON t", "expected EXISTS after IF, got i"},
		{"DROP INDEX i ON t extra", "unexpected token after statement, got extra"},
	}
	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			_, err := parse(t, tc.sql)

			assert.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestParse_IndexAndOnAreReservedWords(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE t (index INT)",
		"CREATE TABLE t (on INT)",
		"SELECT index FROM t",
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := parse(t, sql)

			assert.Error(t, err)
		})
	}
}
