package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

// crashFixture builds a data directory by hand, the way a crash would leave
// it: catalog rows and files written independently of each other.
type crashFixture struct {
	t     *testing.T
	dir   string
	store *catalogStore
}

func newCrashFixture(t *testing.T) *crashFixture {
	dir := t.TempDir()
	return &crashFixture{t: t, dir: dir, store: newTestStoreAt(t, dir)}
}

func (f *crashFixture) database(name string) {
	require.NoError(f.t, f.store.files.makeDir(f.store.files.databaseDir(name)))
	require.NoError(f.t, f.store.addDatabase(name))
}

// row writes a sys_tables row without creating its file.
func (f *crashFixture) row(db, name string, id int64) {
	require.NoError(f.t, f.store.addTable(db, name, mustEncodeTableRow(f.t, db, name, id, idColumn)))
}

// file creates an empty table file with the given id.
func (f *crashFixture) file(db string, id int64) string {
	require.NoError(f.t, f.store.files.makeDir(f.store.files.databaseDir(db)))
	path := f.store.files.tablePath(db, id)
	file, err := storage.CreateFile(path)
	require.NoError(f.t, err)
	require.NoError(f.t, file.Close())
	return path
}

func (f *crashFixture) open() (Catalog, error) {
	require.NoError(f.t, f.store.Close())
	return NewCatalog(DataDir(f.dir))
}

func (f *crashFixture) mustOpen() Catalog {
	c, err := f.open()
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func hasTable(t *testing.T, c Catalog, db, name string) bool {
	t.Helper()
	d, err := c.GetDatabase(db)
	require.NoError(t, err)
	_, ok := d.GetTable(name)
	return ok
}

func TestStartup_RemovesAnOrphanFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	live := f.file("shop", 1)
	orphan := f.file("shop", 2) // created, but the catalog row was never written

	f.mustOpen()

	assert.True(t, exists(t, live))
	assert.False(t, exists(t, orphan))
}

func TestStartup_RemovesAnOrphanDirectoryAndItsFiles(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	stray := f.file("ghost", 5)
	require.NoError(t, os.MkdirAll(filepath.Join(f.dir, "data", "empty_ghost"), 0o755))

	f.mustOpen()

	assert.False(t, exists(t, stray))
	assert.False(t, exists(t, filepath.Dir(stray)))
	assert.False(t, exists(t, filepath.Join(f.dir, "data", "empty_ghost")))
}

func TestStartup_NeverRemovesWhatItDidNotCreate(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	note := filepath.Join(f.dir, "data", "shop", "notes.txt")
	require.NoError(t, os.WriteFile(note, []byte("mine"), 0o644))
	stray := filepath.Join(f.dir, "data", "README")
	require.NoError(t, os.WriteFile(stray, []byte("mine"), 0o644))

	f.mustOpen()

	assert.True(t, exists(t, note), "a file that is not <id>.tbl is left alone")
	assert.True(t, exists(t, stray), "files directly under data/ are left alone")
}

func TestStartup_KeepsAnEmptyDatabaseDirectory(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")

	c := f.mustOpen()

	assert.Equal(t, []string{"shop"}, c.ListDatabases())
	assert.True(t, exists(t, filepath.Join(f.dir, "data", "shop")))
}

func TestStartup_DropsATableRowWhoseDatabaseIsGone(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("gone", "users", 3)
	orphan := f.file("gone", 3)

	c := f.mustOpen()

	assert.Equal(t, []string{"shop"}, c.ListDatabases())
	assert.False(t, exists(t, orphan))
	// the row is tombstoned for good: a database of that name starts empty
	require.NoError(t, c.CreateDatabase("gone"))
	assert.False(t, hasTable(t, c, "gone", "users"))
}

func TestStartup_DuplicateRowsAfterAnAlterKeepTheLaterFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	f.row("shop", "users", 2) // the ALTER's new row; the old row was never tombstoned
	old := f.file("shop", 1)
	fresh := f.file("shop", 2)

	c := f.mustOpen()

	assert.True(t, hasTable(t, c, "shop", "users"))
	db, _ := c.GetDatabase("shop")
	table, _ := db.GetTable("users")
	assert.Equal(t, int64(2), table.(*SqlTable).fileID)
	assert.False(t, exists(t, old))
	assert.True(t, exists(t, fresh))
}

func TestStartup_DuplicateRowsAfterARenameKeepTheNewNameAndTheSharedFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	f.row("shop", "people", 1) // the rename's new row; the old row was never tombstoned
	shared := f.file("shop", 1)

	c := f.mustOpen()

	assert.False(t, hasTable(t, c, "shop", "users"))
	assert.True(t, hasTable(t, c, "shop", "people"))
	assert.True(t, exists(t, shared), "the surviving table still uses this file")
}

func TestStartup_RenameThenAlterChainKeepsSharedFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "a", 1)
	f.row("shop", "b", 1) // rename a -> b, old row lost
	f.row("shop", "a", 2) // a new table named a
	f1 := f.file("shop", 1)
	f2 := f.file("shop", 2)

	c := f.mustOpen()

	assert.True(t, hasTable(t, c, "shop", "a"))
	assert.True(t, hasTable(t, c, "shop", "b"))
	assert.True(t, exists(t, f1), "b still uses f1")
	assert.True(t, exists(t, f2))
	db, _ := c.GetDatabase("shop")
	a, _ := db.GetTable("a")
	b, _ := db.GetTable("b")
	assert.Equal(t, int64(2), a.(*SqlTable).fileID)
	assert.Equal(t, int64(1), b.(*SqlTable).fileID)
}

func TestStartup_LiveRowWithAMissingFileFailsAndClosesWhatItOpened(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	f.row("shop", "orders", 2)
	f.file("shop", 1) // orders' file is missing

	c, err := f.open()

	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorContains(t, err, `table "orders"`)
	assert.ErrorContains(t, err, "missing or damaged")
}

func TestStartup_TornSystemFileFailsStartup(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	require.NoError(t, f.store.Close())
	path := filepath.Join(f.dir, "sys", sysDatabasesFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.Write([]byte("torn"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = NewCatalog(DataDir(f.dir))

	assert.Error(t, err)
}

// The allocator scans ids before cleanup removes the orphan, so the process
// that removed file 40 never hands the id out again.
func TestStartup_DoesNotHandOutTheIDOfAnOrphanItJustRemoved(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	orphan := f.file("shop", 40) // orphan with the highest id

	c := f.mustOpen()
	require.False(t, exists(t, orphan))

	db, _ := c.GetDatabase("shop")
	require.NoError(t, db.CreateTable("t", idColumn, false))
	table, _ := db.GetTable("t")
	assert.Greater(t, table.(*SqlTable).fileID, int64(40))
}

func TestStartup_CaseTwinDatabasesKeepTheirFiles(t *testing.T) {
	probe := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(probe, "a"), 0o755))
	if _, err := os.Stat(filepath.Join(probe, "A")); err != nil {
		t.Skip("filesystem is case-sensitive; Shop and shop do not share a directory here")
	}

	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE Shop")
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "Shop", "CREATE TABLE a (id INT)")
	mustExec(t, e, "Shop", "INSERT INTO a (id) VALUES (1)")
	mustExec(t, e, "shop", "CREATE TABLE b (id INT)")
	mustExec(t, e, "shop", "INSERT INTO b (id) VALUES (2)")

	_, e = restart(t, dir, c)

	assert.Equal(t, [][]string{{"1"}}, mustExec(t, e, "Shop", "SELECT id FROM a").Rows)
	assert.Equal(t, [][]string{{"2"}}, mustExec(t, e, "shop", "SELECT id FROM b").Rows)
}

func TestRestart_DroppedDatabaseWithStaleRowsIsNotResurrected(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE users (id INT)")
	failSysTables(t, storeOf(c)).failDeletes = 1
	require.Error(t, execError(t, e, "", "DROP DATABASE shop"))
	mustExec(t, e, "", "CREATE DATABASE shop")

	_, e = restart(t, dir, c)

	assert.ErrorContains(t, execError(t, e, "shop", "SELECT * FROM users"), `table "users" does not exist`)
}
