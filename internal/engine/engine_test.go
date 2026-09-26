package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

const testDatabase = "test"

func TestEngine_CreateDatabase(t *testing.T) {
	e := NewEngine(NewCatalog())

	resp, err := e.Execute(parser.CreateDatabaseStatement{Database: "testdb"}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_CreateDatabase_AlreadyExists(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase("testdb"))
	e := NewEngine(catalog)

	_, err := e.Execute(parser.CreateDatabaseStatement{Database: "testdb"}, testDatabase)

	assert.EqualError(t, err, `database "testdb" already exists`)
}

func TestEngine_DropDatabase(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase("testdb"))
	e := NewEngine(catalog)

	resp, err := e.Execute(parser.DropDatabaseStatement{Database: "testdb"}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_DropDatabase_DoesNotExist(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.DropDatabaseStatement{Database: "testdb"}, testDatabase)

	assert.EqualError(t, err, `database "testdb" does not exist`)
}

func TestEngine_ShowDatabases(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase("zebra"))
	require.NoError(t, catalog.CreateDatabase("apple"))
	e := NewEngine(catalog)

	resp, err := e.Execute(parser.ShowDatabasesStatement{}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, []string{"Database"}, resp.Columns)
	assert.Equal(t, [][]string{{"apple"}, {"zebra"}}, resp.Rows)
}

func TestEngine_ShowDatabases_Empty(t *testing.T) {
	e := NewEngine(NewCatalog())

	resp, err := e.Execute(parser.ShowDatabasesStatement{}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, []string{"Database"}, resp.Columns)
	assert.Empty(t, resp.Rows)
}

func TestEngine_UnsupportedStatement(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.SelectStatement{Columns: []string{"*"}, Table: "users"}, testDatabase)

	assert.EqualError(t, err, "statement not supported, got parser.SelectStatement")
}

func TestEngine_CreateTable(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)

	columns := []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}
	resp, err := e.Execute(parser.CreateTableStatement{Table: "users", Columns: columns}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_CreateTable_NoDatabaseSelected(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.CreateTableStatement{Table: "users"}, "")

	assert.EqualError(t, err, "database name is required")
}

func TestEngine_CreateTable_DatabaseDoesNotExist(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.CreateTableStatement{Table: "users"}, testDatabase)

	assert.EqualError(t, err, `database "test" does not exist`)
}

func TestEngine_CreateTable_AlreadyExists(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)

	_, err := e.Execute(parser.CreateTableStatement{Table: "users"}, testDatabase)
	require.NoError(t, err)

	_, err = e.Execute(parser.CreateTableStatement{Table: "users"}, testDatabase)

	assert.EqualError(t, err, `table "users" already exists`)
}

func TestEngine_CreateTable_IfNotExists(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)

	_, err := e.Execute(parser.CreateTableStatement{Table: "users"}, testDatabase)
	require.NoError(t, err)

	resp, err := e.Execute(parser.CreateTableStatement{Table: "users", IfNotExists: true}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_InsertInto(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)

	columns := []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
	}
	_, err := e.Execute(parser.CreateTableStatement{Table: "users", Columns: columns}, testDatabase)
	require.NoError(t, err)

	resp, err := e.Execute(parser.InsertIntoStatement{
		Table:  "users",
		Values: [][]parser.Token{{{Type: parser.NUMBER, Value: "1"}, {Type: parser.STRING, Value: "bob"}}},
	}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_InsertInto_NoDatabaseSelected(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.InsertIntoStatement{Table: "users"}, "")

	assert.EqualError(t, err, "database name is required")
}

func TestEngine_InsertInto_DatabaseDoesNotExist(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.InsertIntoStatement{Table: "users"}, testDatabase)

	assert.EqualError(t, err, `database "test" does not exist`)
}

func TestEngine_InsertInto_TableDoesNotExist(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)

	_, err := e.Execute(parser.InsertIntoStatement{Table: "users"}, testDatabase)

	assert.EqualError(t, err, `table "users" does not exist`)
}

func newTestTable(t *testing.T, e Engine) {
	t.Helper()
	columns := []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}
	_, err := e.Execute(parser.CreateTableStatement{Table: "users", Columns: columns}, testDatabase)
	require.NoError(t, err)
}

func TestEngine_AlterTable_AddColumn(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)
	newTestTable(t, e)

	resp, err := e.Execute(parser.AlterTableStatement{
		Table:  "users",
		Action: parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}},
	}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_AlterTable_DropColumn(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)
	_, err := e.Execute(parser.CreateTableStatement{Table: "users", Columns: []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "age", DataType: parser.IntDataType{}},
	}}, testDatabase)
	require.NoError(t, err)

	resp, err := e.Execute(parser.AlterTableStatement{
		Table:  "users",
		Action: parser.DropColumnAction{Column: "age"},
	}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_AlterTable_RenameColumn(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)
	newTestTable(t, e)

	resp, err := e.Execute(parser.AlterTableStatement{
		Table:  "users",
		Action: parser.RenameColumnAction{OldName: "id", NewName: "user_id"},
	}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)
}

func TestEngine_AlterTable_RenameTable(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)
	newTestTable(t, e)

	resp, err := e.Execute(parser.AlterTableStatement{
		Table:  "users",
		Action: parser.RenameTableAction{NewName: "people"},
	}, testDatabase)

	require.NoError(t, err)
	assert.Equal(t, Response{}, resp)

	db, err := catalog.GetDatabase(testDatabase)
	require.NoError(t, err)
	_, exists := db.GetTable("people")
	assert.True(t, exists)
}

func TestEngine_AlterTable_NoDatabaseSelected(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.AlterTableStatement{Table: "users", Action: parser.RenameTableAction{NewName: "people"}}, "")

	assert.EqualError(t, err, "database name is required")
}

func TestEngine_AlterTable_DatabaseDoesNotExist(t *testing.T) {
	e := NewEngine(NewCatalog())

	_, err := e.Execute(parser.AlterTableStatement{
		Table:  "users",
		Action: parser.RenameTableAction{NewName: "people"},
	}, testDatabase)

	assert.EqualError(t, err, `database "test" does not exist`)
}

func TestEngine_AlterTable_TableDoesNotExist(t *testing.T) {
	catalog := NewCatalog()
	require.NoError(t, catalog.CreateDatabase(testDatabase))
	e := NewEngine(catalog)

	_, err := e.Execute(parser.AlterTableStatement{
		Table:  "users",
		Action: parser.DropColumnAction{Column: "id"},
	}, testDatabase)

	assert.EqualError(t, err, `table "users" does not exist`)
}
