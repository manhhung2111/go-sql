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

const peopleDDL = "CREATE TABLE people (id INT PRIMARY KEY, name VARCHAR(20), age INT)"

func newIndexEngine(t *testing.T) (string, Catalog, Engine) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", peopleDDL)
	return dir, c, e
}

func defsOf(t *testing.T, c Catalog, table string) []indexDef {
	t.Helper()
	db, err := c.GetDatabase("shop")
	require.NoError(t, err)
	tbl, ok := db.GetTable(table)
	require.True(t, ok)
	return tbl.(*SqlTable).indexDefs
}

func TestCreateIndex_StatementAndErrors(t *testing.T) {
	_, c, e := newIndexEngine(t)

	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))

	t.Run("a repeated name is an error unless IF NOT EXISTS", func(t *testing.T) {
		assert.ErrorContains(t, execError(t, e, "shop", "CREATE INDEX by_name ON people (age)"),
			`index "by_name" already exists on table "people"`)
		mustExec(t, e, "shop", "CREATE INDEX IF NOT EXISTS by_name ON people (age)")
		assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	})
	t.Run("an unknown column or table", func(t *testing.T) {
		assert.ErrorContains(t, execError(t, e, "shop", "CREATE INDEX x ON people (nope)"), `unknown column "nope"`)
		assert.ErrorContains(t, execError(t, e, "shop", "CREATE INDEX x ON nope (id)"), `table "nope" does not exist`)
	})
	t.Run("a key column and several indexes on one column are fine", func(t *testing.T) {
		mustExec(t, e, "shop", "CREATE INDEX by_id ON people (id)")
		mustExec(t, e, "shop", "CREATE INDEX by_name2 ON people (name)")
		assert.Len(t, defsOf(t, c, "people"), 3)
	})
	t.Run("drop", func(t *testing.T) {
		mustExec(t, e, "shop", "DROP INDEX by_name2 ON people")
		assert.Len(t, defsOf(t, c, "people"), 2)
		assert.ErrorContains(t, execError(t, e, "shop", "DROP INDEX by_name2 ON people"),
			`index "by_name2" does not exist on table "people"`)
		mustExec(t, e, "shop", "DROP INDEX IF EXISTS by_name2 ON people")
	})
}

func TestCreateIndex_SurvivesARestart(t *testing.T) {
	dir, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	mustExec(t, e, "shop", "CREATE INDEX by_age ON people (age)")
	mustExec(t, e, "shop", "DROP INDEX by_age ON people")

	c, e = restart(t, dir, c)

	assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	_ = e
}

func TestCreateIndex_FollowsRenamesAndDrops(t *testing.T) {
	t.Run("RENAME TO", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "shop", "ALTER TABLE people RENAME TO humans")
		c, _ = restart(t, dir, c)

		assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "humans"))
	})
	t.Run("RENAME COLUMN", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "shop", "ALTER TABLE people RENAME COLUMN name TO full_name")
		assert.Equal(t, []indexDef{{"by_name", "full_name"}}, defsOf(t, c, "people"))
		c, _ = restart(t, dir, c)

		assert.Equal(t, []indexDef{{"by_name", "full_name"}}, defsOf(t, c, "people"))
	})
	t.Run("DROP COLUMN of an indexed column is refused, of another is not", func(t *testing.T) {
		_, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		err := execError(t, e, "shop", "ALTER TABLE people DROP COLUMN name")
		assert.ErrorContains(t, err, `cannot drop column "name": index "by_name" uses it`)
		mustExec(t, e, "shop", "ALTER TABLE people DROP COLUMN age")

		assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	})
	t.Run("DROP TABLE then CREATE TABLE of the same name starts with no indexes, also after a restart", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "shop", "DROP TABLE people")
		mustExec(t, e, "shop", peopleDDL)
		assert.Empty(t, defsOf(t, c, "people"))
		c, _ = restart(t, dir, c)

		assert.Empty(t, defsOf(t, c, "people"))
	})
	t.Run("DROP DATABASE removes the rows", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "", "DROP DATABASE shop")
		mustExec(t, e, "", "CREATE DATABASE shop")
		mustExec(t, e, "shop", peopleDDL)
		c, _ = restart(t, dir, c)

		assert.Empty(t, defsOf(t, c, "people"))
	})
}

func TestCreateIndex_OnADroppedTableFailsWithoutTouchingTheCatalog(t *testing.T) {
	_, c, e := newIndexEngine(t)
	db, err := c.GetDatabase("shop")
	require.NoError(t, err)
	table, _ := db.GetTable("people")
	mustExec(t, e, "shop", "DROP TABLE people")

	err = table.CreateIndex("late", "name", false)

	assert.ErrorContains(t, err, "is closed")
}

// Crash states for RENAME TO, written by hand: the staged rows exist under the
// new name while the table row still has the old one, and the reverse.
func TestStartup_ARenameThatCrashedKeepsTheDefinitions(t *testing.T) {
	named := func(name string) parser.ColumnDefinition {
		return parser.ColumnDefinition{Name: name, DataType: parser.IntDataType{}}
	}
	for _, tc := range []struct {
		name, tableRow string
	}{
		{"before the table row was replaced", "users"},
		{"after the table row was replaced, before the old rows were tombstoned", "people"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrashFixture(t)
			f.database("shop")
			f.file("shop", 1)
			require.NoError(t, f.store.addTable("shop", tc.tableRow, mustEncodeTableRow(t, "shop", tc.tableRow, 1, []parser.ColumnDefinition{named("id")})))
			require.NoError(t, f.store.addIndex("shop", "users", "i", "id"))
			require.NoError(t, f.store.addIndex("shop", "people", "i", "id"))

			c := f.mustOpen()

			db, err := c.GetDatabase("shop")
			require.NoError(t, err)
			table, ok := db.GetTable(tc.tableRow)
			require.True(t, ok)
			assert.Equal(t, []indexDef{{"i", "id"}}, table.(*SqlTable).indexDefs)
			store := newTestStoreAt(t, f.dir)
			assert.Len(t, sysIndexRecords(t, store), 1, "the other copy is gone")
		})
	}
}

func TestCreateIndex_AFailedCatalogWriteLeavesNoDefinition(t *testing.T) {
	_, c, e := newIndexEngine(t)
	storeOf(c).indexes = &failingFile{File: storeOf(c).indexes, failInserts: true}

	err := execError(t, e, "shop", "CREATE INDEX by_name ON people (name)")

	assert.ErrorIs(t, err, errInjected)
	assert.Empty(t, defsOf(t, c, "people"), "the definition is added only after its row is durable")
}

func TestCreateIndex_AFailedIndexCopyLeavesTheTableUnrenamed(t *testing.T) {
	dir, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	ff := &failingFile{File: storeOf(c).indexes}
	storeOf(c).indexes = ff
	ff.failInserts = true

	err := execError(t, e, "shop", "ALTER TABLE people RENAME TO humans")

	assert.ErrorIs(t, err, errInjected)
	ff.failInserts = false
	c, e = restart(t, dir, c)
	assert.True(t, hasTable(t, c, "shop", "people"), "the table row is replaced only after the index rows are copied")
	assert.False(t, hasTable(t, c, "shop", "humans"))
	assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	_ = e
}

func TestDropTable_RemovesItsIndexRows(t *testing.T) {
	_, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

	mustExec(t, e, "shop", "DROP TABLE people")

	assert.Empty(t, sysIndexRecords(t, storeOf(c)))
}

// A tombstone that fails leaves a row in sys_indexes that nothing in memory
// points at. Once a table of that name has the column, startup cannot tell it
// from a live row, so the store retries it before the next index operation.
func TestCatalogStore_AFailedTombstoneIsRetriedBeforeTheNextIndexOperation(t *testing.T) {
	setup := func(t *testing.T) (*catalogStore, *failingFile, *stagedIndexRows) {
		store := newTestStore(t)
		require.NoError(t, store.addIndex("shop", "users", "i1", "name"))
		ff := &failingFile{File: store.indexes}
		store.indexes = ff
		staged, err := store.stageIndexRows("shop", "users", "people", nil)
		require.NoError(t, err)
		return store, ff, staged
	}

	t.Run("a commit whose tombstone of the original fails", func(t *testing.T) {
		store, ff, staged := setup(t)
		ff.failDeletes = 1

		store.commitStaged(staged)
		assert.Len(t, sysIndexRecords(t, store), 2, "the original is still on disk")
		require.NoError(t, store.addIndex("shop", "people", "i2", "age"))

		assert.ElementsMatch(t, []string{"people.i1(name)", "people.i2(age)"}, sysIndexRecords(t, store))
	})
	t.Run("an abort whose tombstone of the copy fails", func(t *testing.T) {
		store, ff, staged := setup(t)
		ff.failDeletes = 1

		store.abortStaged(staged)
		assert.Len(t, sysIndexRecords(t, store), 2, "the copy is still on disk")
		require.NoError(t, store.addIndex("shop", "users", "i2", "age"))

		assert.ElementsMatch(t, []string{"users.i1(name)", "users.i2(age)"}, sysIndexRecords(t, store))
	})
	t.Run("an operation is refused while the retry still fails", func(t *testing.T) {
		store, ff, staged := setup(t)
		ff.failDeletes = 2

		store.commitStaged(staged)

		assert.ErrorIs(t, store.addIndex("shop", "people", "i2", "age"), errInjected)
		assert.NoError(t, store.addIndex("shop", "people", "i2", "age"), "and goes through once it succeeds")
	})
}

func TestCreateIndex_ALeakedRowDoesNotComeBackAfterADropAndARestart(t *testing.T) {
	dir, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	storeOf(c).indexes = &failingFile{File: storeOf(c).indexes, failDeletes: 1}
	mustExec(t, e, "shop", "ALTER TABLE people RENAME COLUMN age TO years") // copies by_name too; the original's tombstone fails

	mustExec(t, e, "shop", "DROP INDEX by_name ON people")
	assert.Empty(t, defsOf(t, c, "people"))
	c, _ = restart(t, dir, c)

	assert.Empty(t, defsOf(t, c, "people"), "the dropped index is not resurrected")
}

func TestCreateIndex_ALeakedRowDoesNotAttachToANewTableOfTheSameName(t *testing.T) {
	dir, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	storeOf(c).indexes = &failingFile{File: storeOf(c).indexes, failDeletes: 1}
	mustExec(t, e, "shop", "ALTER TABLE people RENAME TO humans")

	mustExec(t, e, "shop", "CREATE TABLE people (id INT PRIMARY KEY, name VARCHAR(20))")
	c, _ = restart(t, dir, c)

	assert.Empty(t, defsOf(t, c, "people"))
	assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "humans"))
}

// The rows left under a name must be swept when another table is renamed to it,
// also when the renamed table has no indexes of its own.
func TestCreateIndex_RenamingOntoANameSweepsLeftoverRows(t *testing.T) {
	dir, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE TABLE other (id INT PRIMARY KEY, name VARCHAR(20))")
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	storeOf(c).indexes = &failingFile{File: storeOf(c).indexes, failDeletes: 1}
	mustExec(t, e, "shop", "DROP TABLE people") // the index row's tombstone fails and stays in the map
	storeOf(c).indexes = storeOf(c).indexes.(*failingFile).File

	mustExec(t, e, "shop", "ALTER TABLE other RENAME TO people")
	c, _ = restart(t, dir, c)

	assert.Empty(t, defsOf(t, c, "people"))
}
