package engine

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func storeOf(c Catalog) *catalogStore { return c.(*SqlCatalog).store }

func dataDirOf(c Catalog) string { return string(storeOf(c).files.dataDir) }

func usersColumns() []parser.ColumnDefinition {
	return []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 20}},
	}
}

func TestPersist_DDLWritesCatalogRows(t *testing.T) {
	c := newTestCatalog(t)
	store := storeOf(c)

	t.Run("create database", func(t *testing.T) {
		require.NoError(t, c.CreateDatabase("shop"))
		assert.Equal(t, []string{"shop"}, sysDatabaseNames(t, store))
	})

	db, err := c.GetDatabase("shop")
	require.NoError(t, err)

	t.Run("create table", func(t *testing.T) {
		require.NoError(t, db.CreateTable("users", usersColumns(), false))
		table, _ := db.GetTable("users")
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, tableRecord{"shop", "users", table.(*SqlTable).fileID, mustSchema(t, usersColumns())}, records[0])
	})

	t.Run("rename table keeps the file id", func(t *testing.T) {
		table, _ := db.GetTable("users")
		id := table.(*SqlTable).fileID
		require.NoError(t, db.RenameTable("users", "people"))
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, "people", records[0].name)
		assert.Equal(t, id, records[0].fileID)
	})

	t.Run("alter add and drop column write a new file id and schema", func(t *testing.T) {
		table, _ := db.GetTable("people")
		before := table.(*SqlTable).fileID
		require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}}))
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Greater(t, records[0].fileID, before)
		assert.Contains(t, records[0].schema, `"name":"age"`)

		require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "age"}))
		records = sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.NotContains(t, records[0].schema, `"name":"age"`)
	})

	t.Run("rename column rewrites the schema only", func(t *testing.T) {
		table, _ := db.GetTable("people")
		id := table.(*SqlTable).fileID
		require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "title"}))
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, id, records[0].fileID)
		assert.Contains(t, records[0].schema, `"name":"title"`)
	})

	t.Run("drop table", func(t *testing.T) {
		require.NoError(t, db.DropTable("people", false))
		assert.Empty(t, sysTableRecords(t, store))
	})

	t.Run("drop database", func(t *testing.T) {
		require.NoError(t, db.CreateTable("t", idColumn, false))
		require.NoError(t, c.DropDatabase("shop"))
		assert.Empty(t, sysDatabaseNames(t, store))
		assert.Empty(t, sysTableRecords(t, store))
	})
}

func TestPersist_FailedCatalogWriteChangesNothing(t *testing.T) {
	setup := func(t *testing.T) (Catalog, Database, *catalogStore) {
		c := newTestCatalog(t)
		db := createDB(t, c, "shop")
		require.NoError(t, db.CreateTable("users", usersColumns(), false))
		return c, db, storeOf(c)
	}

	t.Run("create table leaves no table and no file", func(t *testing.T) {
		c, db, store := setup(t)
		failSysTables(t, store).failInserts = true

		err := db.CreateTable("orders", idColumn, false)

		require.Error(t, err)
		_, exists := db.GetTable("orders")
		assert.False(t, exists)
		assert.Len(t, tableFiles(t, dataDirOf(c), "shop"), 1, "only users' file")
	})

	t.Run("a wide schema fails before any file exists", func(t *testing.T) {
		c, db, _ := setup(t)
		columns := make([]parser.ColumnDefinition, 600)
		for i := range columns {
			columns[i] = parser.ColumnDefinition{Name: fmt.Sprintf("column_%03d", i), DataType: parser.VarCharDataType{Size: 255}}
		}

		err := db.CreateTable("wide", columns, false)

		assert.ErrorContains(t, err, "too large to store")
		assert.Len(t, tableFiles(t, dataDirOf(c), "shop"), 1)
	})

	t.Run("drop table keeps the table and its rows", func(t *testing.T) {
		_, db, store := setup(t)
		table, _ := db.GetTable("users")
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{{{Type: parser.NUMBER, Value: "1"}, {Type: parser.STRING, Value: "ann"}}}))
		failSysTables(t, store).failDeletes = 1

		require.Error(t, db.DropTable("users", false))

		again, exists := db.GetTable("users")
		require.True(t, exists)
		resp, err := again.Select([]string{"*"}, nil)
		require.NoError(t, err)
		assert.Equal(t, [][]string{{"1", "ann"}}, resp.Rows)
	})

	t.Run("rename table keeps the old name", func(t *testing.T) {
		_, db, store := setup(t)
		failSysTables(t, store).failInserts = true

		require.Error(t, db.RenameTable("users", "people"))

		_, oldExists := db.GetTable("users")
		_, newExists := db.GetTable("people")
		assert.True(t, oldExists)
		assert.False(t, newExists)
	})

	t.Run("alter add column keeps the schema and discards the new file", func(t *testing.T) {
		c, db, store := setup(t)
		table, _ := db.GetTable("users")
		sqlTable := table.(*SqlTable)
		before := sqlTable.fileID
		failSysTables(t, store).failInserts = true

		err := table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}})

		require.Error(t, err)
		assert.Len(t, sqlTable.Columns, 2)
		assert.Equal(t, before, sqlTable.fileID)
		assert.Len(t, tableFiles(t, dataDirOf(c), "shop"), 1)
	})

	t.Run("rename column keeps the name and does not edit the caller's slice", func(t *testing.T) {
		_, db, store := setup(t)
		table, _ := db.GetTable("users")
		failSysTables(t, store).failInserts = true

		require.Error(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "title"}))

		assert.Equal(t, "name", table.(*SqlTable).Columns[1].Name)
	})

	t.Run("drop database that cannot commit keeps the database usable", func(t *testing.T) {
		c, _, store := setup(t)
		failSysDatabases(t, store).failDeletes = 1

		require.Error(t, c.DropDatabase("shop"))

		db, err := c.GetDatabase("shop")
		require.NoError(t, err)
		_, exists := db.GetTable("users")
		assert.True(t, exists)
	})
}

func TestPersist_DropDatabaseWithStaleTableRowsIsNotResurrected(t *testing.T) {
	c := newTestCatalog(t)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", usersColumns(), false))
	store := storeOf(c)
	failSysTables(t, store).failDeletes = 1

	err := c.DropDatabase("shop")

	assert.ErrorContains(t, err, "dropped, but")
	_, getErr := c.GetDatabase("shop")
	require.Error(t, getErr)
	require.Len(t, sysTableRecords(t, store), 1, "the stale row is still there")

	require.NoError(t, c.CreateDatabase("shop"))

	assert.Empty(t, sysTableRecords(t, store))
	fresh, err := c.GetDatabase("shop")
	require.NoError(t, err)
	_, exists := fresh.GetTable("users")
	assert.False(t, exists)
}
