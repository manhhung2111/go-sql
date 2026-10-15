package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// names lists the files of a database directory, sorted.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func touch(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
}

func TestFileIDOf(t *testing.T) {
	tests := []struct {
		name string
		id   int64
		ok   bool
	}{
		{"12.tbl", 12, true},
		{"12.idx", 12, true},
		{"12.idx.tmp", 12, true},
		{"x.idx", 0, false},
		{"12.tbl.idx", 0, false},
		{"12.txt", 0, false},
		{".idx", 0, false},
		{"-3.idx", 0, false},
	}
	for _, tc := range tests {
		id, ok := fileIDOf(tc.name)
		assert.Equal(t, tc.ok, ok, tc.name)
		assert.Equal(t, tc.id, id, tc.name)
	}
}

func TestAllocator_NeverReusesTheIdOfALeftoverSnapshot(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "data", "shop", "3.tbl"))
	touch(t, filepath.Join(dir, "data", "shop", "9.idx"))

	files, err := newFileAllocator(DataDir(dir))

	require.NoError(t, err)
	assert.EqualValues(t, 10, files.nextFileID())
}

func TestRemoveOrphans_HandlesSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	shop := filepath.Join(dir, "data", "shop")
	for _, name := range []string{"5.tbl", "5.idx", "5.idx.tmp", "7.tbl", "7.idx", "8.idx", "8.idx.tmp", "x.idx", "9.idx.bak"} {
		touch(t, filepath.Join(shop, name))
	}
	files := &fileAllocator{dataDir: DataDir(dir)}

	removeOrphans(files, map[string]bool{"shop": true}, map[int64]bool{5: true})

	assert.Equal(t, []string{"5.idx", "5.idx.tmp", "5.tbl", "9.idx.bak", "x.idx"}, namesIn(t, shop),
		"a referenced table keeps its files (the .tmp of a crashed save is cleaned by the next save); unreferenced ids go, and so do names that are not <number> plus a known suffix")
}
func tableOf(t *testing.T, c Catalog, database, name string) *SqlTable {
	t.Helper()
	db, err := c.GetDatabase(database)
	require.NoError(t, err)
	table, ok := db.GetTable(name)
	require.True(t, ok)
	return table.(*SqlTable)
}

// eventsEngine is a catalog over dir with shop.events(id INT, kind VARCHAR(20))
// and CREATE INDEX by_kind, holding n rows with kinds k0..k4.
func eventsEngine(t *testing.T, dir string, n int) (Catalog, Engine) {
	t.Helper()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE events (id INT, kind VARCHAR(20))")
	mustExec(t, e, "shop", "CREATE INDEX by_kind ON events (kind)")
	for i := 0; i < n; i++ {
		mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO events (id, kind) VALUES (%d, 'k%d')", i, i%5))
	}
	return c, e
}

const kindQuery = "SELECT id FROM events WHERE kind = 'k3'"

func TestSnapshot_ARestartLoadsTheTreesWithoutScanning(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	want := mustExec(t, e, "shop", kindQuery).Rows // builds the trees
	require.NotEmpty(t, want)
	path := tableOf(t, c, "shop", "events").snapshotPath()

	c, e = restart(t, dir, c)

	assert.FileExists(t, path, "a clean shutdown saved the trees")
	table := tableOf(t, c, "shop", "events")
	cf := countOn(table)
	assert.Equal(t, want, mustExec(t, e, "shop", kindQuery).Rows)
	assert.EqualValues(t, 0, cf.scans.Load(), "the heap was not read to build the trees")
	assert.EqualValues(t, 1, cf.getManyCalls.Load(), "and the lookup used them")
	assertIndexesMatchHeap(t, table)
}

func TestSnapshot_UniqueColumnsAreLoadedToo(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE users (id INT PRIMARY KEY, email VARCHAR(40) UNIQUE)")
	for i := 0; i < 20; i++ {
		mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO users (id, email) VALUES (%d, 'u%d@x.com')", i, i))
	}
	mustExec(t, e, "shop", "SELECT id FROM users WHERE id = 3")

	c, e = restart(t, dir, c)

	cf := countOn(tableOf(t, c, "shop", "users"))
	assert.Equal(t, [][]string{{"7"}}, mustExec(t, e, "shop", "SELECT id FROM users WHERE email = 'u7@x.com'").Rows)
	assert.EqualValues(t, 0, cf.scans.Load())
	err := execError(t, e, "shop", "INSERT INTO users (id, email) VALUES (3, 'new@x.com')")
	assert.ErrorContains(t, err, "duplicate", "the loaded tree still enforces the key")
}

func TestSnapshot_ARestartThatUsesNothingKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 10)
	mustExec(t, e, "shop", kindQuery)
	path := tableOf(t, c, "shop", "events").snapshotPath()
	c, _ = restart(t, dir, c)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, _ = restart(t, dir, c) // the table was never used in the second run

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "nothing was built, so nothing was saved over it")
}

func TestSnapshot_ADamagedFileFallsBackToTheScanAndIsRemoved(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	want := mustExec(t, e, "shop", kindQuery).Rows
	path := tableOf(t, c, "shop", "events").snapshotPath()
	c, e = restart(t, dir, c)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	b[len(b)-100] ^= 0xFF
	require.NoError(t, os.WriteFile(path, b, 0o644))
	cf := countOn(tableOf(t, c, "shop", "events"))

	assert.Equal(t, want, mustExec(t, e, "shop", kindQuery).Rows)

	assert.EqualValues(t, 1, cf.scans.Load(), "the trees were rebuilt")
	assert.NoFileExists(t, path)
}

func TestSnapshot_ARenamedColumnIsNotTrustedAfterACrash(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	mustExec(t, e, "shop", kindQuery)
	c, e = restart(t, dir, c) // saved with the column called kind
	mustExec(t, e, "shop", "ALTER TABLE events RENAME COLUMN kind TO label")
	c = newTestCatalogAt(t, dir) // the old catalog is not closed: a crash
	e = NewEngine(c)
	cf := countOn(tableOf(t, c, "shop", "events"))

	rows := mustExec(t, e, "shop", "SELECT id FROM events WHERE label = 'k3'").Rows

	assert.Len(t, rows, 6)
	assert.EqualValues(t, 1, cf.scans.Load(), "the saved tree set no longer matches, so it was rebuilt")
}
