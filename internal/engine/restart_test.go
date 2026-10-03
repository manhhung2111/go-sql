package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const usersDDL = "CREATE TABLE users (id INT PRIMARY KEY, name VARCHAR(20) NOT NULL DEFAULT 'anon', active BOOLEAN)"

func TestRestart_KeepsDatabasesSchemasAndRows(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "", "CREATE DATABASE empty")
	mustExec(t, e, "shop", usersDDL)
	mustExec(t, e, "shop", "INSERT INTO users (id, name, active) VALUES (1, 'ann', 1), (2, 'bob', 0)")
	mustExec(t, e, "shop", "INSERT INTO users (id) VALUES (3)")

	c, e = restart(t, dir, c)

	assert.Equal(t, []string{"empty", "shop"}, c.ListDatabases())
	resp := mustExec(t, e, "shop", "SELECT * FROM users")
	assert.Equal(t, []string{"id", "name", "active"}, resp.Columns)
	assert.Equal(t, [][]string{{"1", "ann", "true"}, {"2", "bob", "false"}, {"3", "anon", "NULL"}}, resp.Rows)

	t.Run("constraints survive: the primary key still rejects a duplicate", func(t *testing.T) {
		err := execError(t, e, "shop", "INSERT INTO users (id) VALUES (1)")
		assert.ErrorContains(t, err, "duplicate")
	})
	t.Run("NOT NULL with a default survives", func(t *testing.T) {
		mustExec(t, e, "shop", "INSERT INTO users (id) VALUES (4)")
		resp := mustExec(t, e, "shop", "SELECT name FROM users WHERE id = 4")
		assert.Equal(t, [][]string{{"anon"}}, resp.Rows)
	})
}

func TestRestart_KeepsDeletesAndUpdates(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", usersDDL)
	mustExec(t, e, "shop", "INSERT INTO users (id, name) VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	mustExec(t, e, "shop", "DELETE FROM users WHERE id = 2")
	mustExec(t, e, "shop", "UPDATE users SET name = 'z' WHERE id = 3")

	_, e = restart(t, dir, c)

	resp := mustExec(t, e, "shop", "SELECT id, name FROM users")
	assert.ElementsMatch(t, [][]string{{"1", "a"}, {"3", "z"}}, resp.Rows)
}

func TestRestart_AfterDDL(t *testing.T) {
	tests := map[string]struct {
		ddl    []string
		verify func(t *testing.T, e Engine)
	}{
		"drop table": {
			ddl: []string{"DROP TABLE users"},
			verify: func(t *testing.T, e Engine) {
				assert.ErrorContains(t, execError(t, e, "shop", "SELECT * FROM users"), `table "users" does not exist`)
			},
		},
		"rename table": {
			ddl: []string{"ALTER TABLE users RENAME TO people"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT id FROM people")
				assert.Equal(t, [][]string{{"1"}}, resp.Rows)
				assert.Error(t, execError(t, e, "shop", "SELECT * FROM users"))
			},
		},
		"add column": {
			ddl: []string{"ALTER TABLE users ADD COLUMN age INT DEFAULT 7"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT id, age FROM users")
				assert.Equal(t, [][]string{{"1", "7"}}, resp.Rows)
			},
		},
		"drop column": {
			ddl: []string{"ALTER TABLE users DROP COLUMN active"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT * FROM users")
				assert.Equal(t, []string{"id", "name"}, resp.Columns)
			},
		},
		"rename column": {
			ddl: []string{"ALTER TABLE users RENAME COLUMN name TO title"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT title FROM users")
				assert.Equal(t, [][]string{{"x"}}, resp.Rows)
			},
		},
		"drop database": {
			ddl: []string{"DROP DATABASE shop"},
			verify: func(t *testing.T, e Engine) {
				assert.Equal(t, [][]string{}, mustExec(t, e, "", "SHOW DATABASES").Rows)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := newTestCatalogAt(t, dir)
			e := NewEngine(c)
			mustExec(t, e, "", "CREATE DATABASE shop")
			mustExec(t, e, "shop", usersDDL)
			mustExec(t, e, "shop", "INSERT INTO users (id, name, active) VALUES (1, 'x', 1)")
			for _, sql := range tt.ddl {
				mustExec(t, e, "shop", sql)
			}

			_, e = restart(t, dir, c)

			tt.verify(t, e)
		})
	}
}

func TestRestart_IsStableAndNeverReusesFileIDs(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", usersDDL)
	db, _ := c.GetDatabase("shop")
	table, _ := db.GetTable("users")
	first := table.(*SqlTable).fileID

	c, e = restart(t, dir, c)
	c, e = restart(t, dir, c)

	mustExec(t, e, "shop", "CREATE TABLE orders (id INT)")
	db, _ = c.GetDatabase("shop")
	users, _ := db.GetTable("users")
	orders, _ := db.GetTable("orders")
	assert.Equal(t, first, users.(*SqlTable).fileID, "a table keeps its file id across restarts")
	assert.Greater(t, orders.(*SqlTable).fileID, first)
	require.NotNil(t, c)
}
