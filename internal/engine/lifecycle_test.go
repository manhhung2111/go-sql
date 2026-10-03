package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

var idColumn = []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}

func createDB(t *testing.T, c Catalog, name string) Database {
	t.Helper()
	require.NoError(t, c.CreateDatabase(name))
	db, err := c.GetDatabase(name)
	require.NoError(t, err)
	return db
}

func TestLifecycle_CreateDatabaseMakesItsDirectory(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)

	createDB(t, c, "shop")

	info, err := os.Stat(filepath.Join(dir, "data", "shop"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestLifecycle_CreateDatabaseFailureLeavesNoPhantomDatabase(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	// "data" is a regular file, so no database directory can be made under it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), nil, 0o644))

	err := c.CreateDatabase("shop")

	require.Error(t, err)
	assert.Empty(t, c.ListDatabases())
}

func TestLifecycle_CreateDatabaseRejectsPathLikeNames(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)

	for _, name := range []string{"../escape", "a/b", ".."} {
		assert.ErrorContains(t, c.CreateDatabase(name), "invalid database name", name)
	}

	assert.Empty(t, c.ListDatabases())
	_, err := os.Stat(filepath.Join(dir, "escape"))
	assert.True(t, os.IsNotExist(err), "nothing is created outside <data_dir>/data")
}

func TestLifecycle_CreateTableMakesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")

	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
	info, err := os.Stat(filepath.Join(dir, "data", "shop", "1.tbl"))
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestLifecycle_CreateTableWithInvalidSchemaLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")

	err := db.CreateTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "id", DataType: parser.IntDataType{}},
	}, false)

	require.Error(t, err)
	assert.Empty(t, tableFiles(t, dir, "shop"))
	_, exists := db.GetTable("users")
	assert.False(t, exists)
}

func TestLifecycle_CreateTableThatAlreadyExistsMakesNoSecondFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Error(t, db.CreateTable("users", idColumn, false))
	assert.NoError(t, db.CreateTable("users", idColumn, true), "IF NOT EXISTS")

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_DropTableRemovesOnlyItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))  // 1.tbl
	require.NoError(t, db.CreateTable("orders", idColumn, false)) // 2.tbl

	require.NoError(t, db.DropTable("users", false))

	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"))
	assert.NoError(t, db.DropTable("users", true), "IF EXISTS on a missing table is a no-op")
	assert.EqualError(t, db.DropTable("users", false), `table "users" does not exist`)
	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_RecreatingADroppedTableGetsANewFileID(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false)) // 1.tbl
	require.NoError(t, db.DropTable("users", false))

	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_RenameTableKeepsItsFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))

	require.NoError(t, db.RenameTable("users", "customers"))

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
	_, exists := db.GetTable("customers")
	assert.True(t, exists)
}

func TestLifecycle_DropDatabaseRemovesItsFilesAndDirectory(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))
	require.NoError(t, db.CreateTable("orders", idColumn, false))

	require.NoError(t, c.DropDatabase("shop"))

	_, err := os.Stat(filepath.Join(dir, "data", "shop"))
	assert.True(t, os.IsNotExist(err))
	assert.Empty(t, c.ListDatabases())
}

// Two database names that differ only by case (or Unicode form) share one
// directory on a case-insensitive filesystem, so dropping one must not delete
// anything it did not create.
func TestLifecycle_DropDatabaseLeavesForeignFilesAlone(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))
	foreign := filepath.Join(dir, "data", "shop", "foreign.dat")
	require.NoError(t, os.WriteFile(foreign, []byte("not ours"), 0o644))

	require.NoError(t, c.DropDatabase("shop"))

	assert.Equal(t, []string{"foreign.dat"}, tableFiles(t, dir, "shop"), "its own table file is gone, the foreign file and directory remain")
	got, err := os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, []byte("not ours"), got)
}

func TestLifecycle_RestartContinuesFileIDs(t *testing.T) {
	dir := t.TempDir()
	first := newTestCatalogAt(t, dir)
	db := createDB(t, first, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))  // 1.tbl
	require.NoError(t, db.CreateTable("orders", idColumn, false)) // 2.tbl
	require.NoError(t, first.Close())

	// The new catalog loads "shop" and its tables; a table created now must
	// not reuse or overwrite an id already on disk.
	second := newTestCatalogAt(t, dir)
	db2, err := second.GetDatabase("shop")
	require.NoError(t, err)
	require.NoError(t, db2.CreateTable("invoices", idColumn, false))

	assert.Equal(t, []string{"1.tbl", "2.tbl", "3.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_ConcurrentCreateTableGetsDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assert.NoError(t, db.CreateTable(fmt.Sprintf("t%d", i), idColumn, false))
		}(i)
	}
	wg.Wait()

	assert.Len(t, tableFiles(t, dir, "shop"), n)
}

func TestLifecycle_CloseIsIdempotentAndReleasesTheFile(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))
	table, _ := db.GetTable("users")

	require.NoError(t, c.Close())
	require.NoError(t, c.Close())

	assert.Nil(t, table.(*SqlTable).file)
}

func TestTable_DropRemovesItsFile(t *testing.T) {
	table, err := newTempTable(t, "users", idColumn)
	require.NoError(t, err)

	require.NoError(t, table.Drop())
	require.NoError(t, table.Drop(), "dropping twice is harmless")

	_, statErr := os.Stat(table.path)
	assert.True(t, os.IsNotExist(statErr))
}

// Two database names that differ only by case share one directory on a
// case-insensitive filesystem, so dropping one can remove a directory the other
// still uses. Removing the directory by hand reproduces that state portably.
func TestLifecycle_CreateTableRecreatesAMissingDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "data", "shop")))

	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
}

// A handle fetched before DROP DATABASE must not be able to create tables in a
// database that no longer exists: the table would be unreachable, and its file
// descriptor and file would leak.
func TestLifecycle_CreateTableOnADroppedDatabaseFails(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, c.DropDatabase("shop"))

	err := db.CreateTable("users", idColumn, false)

	assert.EqualError(t, err, `database "shop" does not exist`)
	_, statErr := os.Stat(filepath.Join(dir, "data", "shop"))
	assert.True(t, os.IsNotExist(statErr), "nothing is created for a dropped database")
}

// The end-to-end form of the case-twin hazard. On a case-insensitive
// filesystem "shop" and "SHOP" share one directory, so this really collides;
// on a case-sensitive one they are separate and it passes trivially.
func TestLifecycle_DroppingACaseTwinDoesNotBreakTheSurvivor(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	createDB(t, c, "shop")
	survivor := createDB(t, c, "SHOP")
	require.NoError(t, c.DropDatabase("shop"))

	require.NoError(t, survivor.CreateTable("users", idColumn, false))

	_, exists := survivor.GetTable("users")
	assert.True(t, exists)
}

// After an ALTER the table lives in a new file, so dropping it must remove
// that file, not the path the table started with.
func TestLifecycle_AlterThenDropRemovesTheCurrentFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false)) // 1.tbl
	table, _ := db.GetTable("users")

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}},
	}))

	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"), "1.tbl was replaced by a fresh id, never reused")

	require.NoError(t, db.DropTable("users", false))

	assert.Empty(t, tableFiles(t, dir, "shop"))
}
