package engine

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func TestCatalogStore_DatabaseRows(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.addDatabase("shop"))
	require.NoError(t, store.addDatabase("blog"))
	assert.Equal(t, []string{"shop", "blog"}, sysDatabaseNames(t, store))

	require.NoError(t, store.removeDatabase("shop"))
	assert.Equal(t, []string{"blog"}, sysDatabaseNames(t, store))
}

func TestCatalogStore_TableRows(t *testing.T) {
	store := newTestStore(t)
	schema := mustSchema(t, idColumn)

	require.NoError(t, store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 7, idColumn)))
	assert.Equal(t, []tableRecord{{"shop", "users", 7, schema}}, sysTableRecords(t, store))

	t.Run("replace with a new name keeps one row", func(t *testing.T) {
		require.NoError(t, store.replaceTable("shop", "users", "people", mustEncodeTableRow(t, "shop", "people", 7, idColumn)))
		assert.Equal(t, []tableRecord{{"shop", "people", 7, schema}}, sysTableRecords(t, store))
	})

	t.Run("replace with a new file id keeps one row", func(t *testing.T) {
		require.NoError(t, store.replaceTable("shop", "people", "people", mustEncodeTableRow(t, "shop", "people", 9, idColumn)))
		assert.Equal(t, []tableRecord{{"shop", "people", 9, schema}}, sysTableRecords(t, store))
	})

	t.Run("remove leaves nothing", func(t *testing.T) {
		require.NoError(t, store.removeTable("shop", "people"))
		assert.Empty(t, sysTableRecords(t, store))
	})
}

func TestCatalogStore_RemoveDatabaseTablesOnlyTouchesThatDatabase(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.addTable("shop", "a", mustEncodeTableRow(t, "shop", "a", 1, idColumn)))
	require.NoError(t, store.addTable("blog", "b", mustEncodeTableRow(t, "blog", "b", 2, idColumn)))

	require.NoError(t, store.removeDatabaseTables("shop"))

	records := sysTableRecords(t, store)
	require.Len(t, records, 1)
	assert.Equal(t, "blog", records[0].database)
}

func TestCatalogStore_FailedWritesChangeNothing(t *testing.T) {
	t.Run("a failed insert leaves no row and no index entry", func(t *testing.T) {
		store := newTestStore(t)
		ff := failSysTables(t, store)
		ff.failInserts = true

		err := store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn))

		require.Error(t, err)
		assert.Empty(t, sysTableRecords(t, store))
		assert.Empty(t, store.tableRows)
	})

	t.Run("a failed sync removes the row it inserted", func(t *testing.T) {
		store := newTestStore(t)
		ff := failSysTables(t, store)
		ff.failSyncs = true

		err := store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn))

		require.Error(t, err)
		ff.failSyncs = false
		assert.Empty(t, sysTableRecords(t, store))
	})

	t.Run("replace: a failed tombstone removes the new row and keeps the old", func(t *testing.T) {
		store := newTestStore(t)
		require.NoError(t, store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn)))
		ff := failSysTables(t, store)
		ff.failDeletes = 1 // the old row's tombstone fails; the clean-up of the new row works

		err := store.replaceTable("shop", "users", "people", mustEncodeTableRow(t, "shop", "people", 1, idColumn))

		require.Error(t, err)
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, "users", records[0].name)
		require.NoError(t, store.removeTable("shop", "users"), "the index still points at the old row")
	})
}

func TestCatalogStore_AddDatabaseSweepsStaleTableRows(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.addDatabase("shop"))
	require.NoError(t, store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn)))
	require.NoError(t, store.removeDatabase("shop"))
	ff := failSysTables(t, store)
	ff.failDeletes = 1
	require.Error(t, store.removeDatabaseTables("shop"), "the table row's tombstone fails")
	require.Len(t, sysTableRecords(t, store), 1, "the stale row is still on disk")

	require.NoError(t, store.addDatabase("shop"))

	assert.Empty(t, sysTableRecords(t, store), "a new database of that name must not inherit the old tables")
}

func TestCatalogStore_WideSchemaIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	columns := make([]parser.ColumnDefinition, 600)
	for i := range columns {
		columns[i] = parser.ColumnDefinition{Name: fmt.Sprintf("column_%03d", i), DataType: parser.VarCharDataType{Size: 255}}
	}

	_, err := encodeTableRow("shop", "wide", 1, columns)

	assert.ErrorContains(t, err, `schema of table "wide" is too large to store`)
}

func TestCatalogStore_ReopeningKeepsTheRows(t *testing.T) {
	dir := t.TempDir()
	first := newTestStoreAt(t, dir)
	require.NoError(t, first.addDatabase("shop"))
	require.NoError(t, first.Close())
	require.NoError(t, first.Close(), "Close is idempotent")

	second := newTestStoreAt(t, dir)

	assert.Equal(t, []string{"shop"}, sysDatabaseNames(t, second))
}
