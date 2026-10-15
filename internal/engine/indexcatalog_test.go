package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// sysIndexRecords reads every live sys_indexes row as "table.name(column)".
func sysIndexRecords(t *testing.T, store *catalogStore) []string {
	t.Helper()
	var out []string
	for row, err := range store.indexes.Scan() {
		require.NoError(t, err)
		values, err := DecodeRow(sysIndexesColumns, row.Bytes)
		require.NoError(t, err)
		out = append(out, values[1].(string)+"."+values[2].(string)+"("+values[3].(string)+")")
	}
	return out
}

func TestCatalogStore_IndexRows(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.addIndex("shop", "users", "by_name", "name"))
	require.NoError(t, store.addIndex("shop", "users", "by_age", "age"))
	assert.Equal(t, []string{"users.by_name(name)", "users.by_age(age)"}, sysIndexRecords(t, store))

	require.NoError(t, store.removeIndex("shop", "users", "by_name"))
	assert.Equal(t, []string{"users.by_age(age)"}, sysIndexRecords(t, store))

	assert.Error(t, store.removeIndex("shop", "users", "by_name"), "already gone")
}

func TestCatalogStore_StagedIndexRows(t *testing.T) {
	setup := func(t *testing.T) *catalogStore {
		store := newTestStore(t)
		require.NoError(t, store.addIndex("shop", "users", "i1", "name"))
		require.NoError(t, store.addIndex("shop", "users", "i2", "age"))
		require.NoError(t, store.addIndex("shop", "other", "o1", "x"))
		return store
	}

	t.Run("a table rename: the copies are visible before the commit, the old rows after", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "users", "people", nil)
		require.NoError(t, err)
		assert.Len(t, sysIndexRecords(t, store), 5, "old and new rows coexist")

		store.commitStaged(staged)
		assert.ElementsMatch(t, []string{"other.o1(x)", "people.i1(name)", "people.i2(age)"}, sysIndexRecords(t, store))
		require.NoError(t, store.removeIndex("shop", "people", "i1"), "the map follows the new rows")
	})

	t.Run("a column rename changes only that column", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "users", "users", map[string]string{"name": "full_name"})
		require.NoError(t, err)
		store.commitStaged(staged)

		assert.ElementsMatch(t, []string{"other.o1(x)", "users.i1(full_name)", "users.i2(age)"}, sysIndexRecords(t, store))
	})

	t.Run("abort removes the copies and keeps the originals", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "users", "people", nil)
		require.NoError(t, err)
		store.abortStaged(staged)

		assert.ElementsMatch(t, []string{"other.o1(x)", "users.i1(name)", "users.i2(age)"}, sysIndexRecords(t, store))
	})

	t.Run("a table with no indexes stages nothing", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "none", "still_none", nil)

		require.NoError(t, err)
		store.commitStaged(staged)
		assert.Len(t, sysIndexRecords(t, store), 3)
	})
}

func TestCatalogStore_RemoveTableIndexesAndSweeps(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.addDatabase("shop"))
	require.NoError(t, store.addIndex("shop", "users", "i1", "name"))
	require.NoError(t, store.addIndex("shop", "other", "o1", "x"))

	require.NoError(t, store.removeTableIndexes("shop", "users"))
	assert.Equal(t, []string{"other.o1(x)"}, sysIndexRecords(t, store))

	t.Run("adding a table sweeps leftover rows under its name", func(t *testing.T) {
		require.NoError(t, store.addIndex("shop", "ghost", "g1", "id"))
		require.NoError(t, store.addTable("shop", "ghost", mustEncodeTableRow(t, "shop", "ghost", 3, idColumn)))
		assert.Equal(t, []string{"other.o1(x)"}, sysIndexRecords(t, store))
	})

	t.Run("removing the database's tables removes its index rows", func(t *testing.T) {
		require.NoError(t, store.removeDatabaseTables("shop"))
		assert.Empty(t, sysIndexRecords(t, store))
	})
}

// A crash leaves any of these hand-made states. Loading must keep exactly the
// definitions that name a surviving table and an existing column.
func TestStartup_IndexRowsAreResolvedAgainstTheTables(t *testing.T) {
	named := func(name string) parser.ColumnDefinition {
		return parser.ColumnDefinition{Name: name, DataType: parser.IntDataType{}}
	}
	setup := func(t *testing.T) *crashFixture {
		f := newCrashFixture(t)
		f.database("shop")
		require.NoError(t, f.store.files.makeDir(f.store.files.databaseDir("shop")))
		f.file("shop", 1)
		require.NoError(t, f.store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, []parser.ColumnDefinition{named("id"), named("age")})))
		return f
	}
	definitions := func(t *testing.T, c Catalog) []indexDef {
		db, err := c.GetDatabase("shop")
		require.NoError(t, err)
		table, ok := db.GetTable("users")
		require.True(t, ok)
		return table.(*SqlTable).indexDefs
	}

	t.Run("a valid row survives", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
	})
	t.Run("a row for a missing table is dropped", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "gone", "x", "age"))
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
		store := newTestStoreAt(t, f.dir) // reopen to read what is left on disk
		assert.Equal(t, []string{"users.by_age(age)"}, sysIndexRecords(t, store))
	})
	t.Run("a row naming a column the schema lacks is dropped", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "users", "stale", "old_name"))

		c := f.mustOpen()

		assert.Empty(t, definitions(t, c))
	})
	t.Run("a crash between the insert and the tombstone of a column rename keeps the new row", func(t *testing.T) {
		f := setup(t)
		// table row already says "age"; both the old ("years") and new ("age") index rows exist
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "years"))
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
	})
	t.Run("a crash before the table row of a column rename keeps the old row", func(t *testing.T) {
		f := setup(t)
		// table row still says "age"; the staged copy names a column that does not exist yet
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "years"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
	})
	t.Run("two valid rows with one name keep the first", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "users", "dup", "age"))
		require.NoError(t, f.store.addIndex("shop", "users", "dup", "id"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "dup", column: "age"}}, definitions(t, c))
	})
}
