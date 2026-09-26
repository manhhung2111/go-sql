package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func TestEngine_CreateDatabase(t *testing.T) {
	e := NewEngine(NewCatalog())

	resp, err := e.Execute(parser.CreateDatabaseStatement{Database: "testdb"})

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_CreateDatabase_AlreadyExists(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase("testdb"))
	e := NewEngine(catalog)

	_, err := e.Execute(parser.CreateDatabaseStatement{Database: "testdb"})

	assert.EqualError(t, err, `database "testdb" already exists`)
}

func TestEngine_DropDatabase(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase("testdb"))
	e := NewEngine(catalog)

	resp, err := e.Execute(parser.DropDatabaseStatement{Database: "testdb"})

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_DropDatabase_DoesNotExist(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.DropDatabaseStatement{Database: "testdb"})

	assert.EqualError(t, err, `database "testdb" does not exist`)
}

func TestEngine_ShowDatabases(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase("zebra"))
	require.NoError(t, catalog.CreateDatabase("apple"))
	e := NewEngine(catalog)

	resp, err := e.Execute(parser.ShowDatabasesStatement{})

	require.NoError(t, err)
	assert.Equal(t, []string{"Database"}, resp.Columns)
	assert.Equal(t, [][]string{{"apple"}, {"zebra"}}, resp.Rows)
}

func TestEngine_ShowDatabases_Empty(t *testing.T) {
	e := NewEngine(NewCatalog())

	resp, err := e.Execute(parser.ShowDatabasesStatement{})

	require.NoError(t, err)
	assert.Equal(t, []string{"Database"}, resp.Columns)
	assert.Empty(t, resp.Rows)
}

func TestEngine_UnsupportedStatement(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.SelectStatement{Columns: []string{"*"}, Table: "users"})

	assert.EqualError(t, err, "statement not supported, got parser.SelectStatement")
}
