package engine

import (
	"math/rand"

	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
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

// savedEvents returns a catalog restarted over a saved snapshot of 30 events,
// with the snapshot path.
func savedEvents(t *testing.T) (Catalog, Engine, string, string) {
	t.Helper()
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	mustExec(t, e, "shop", kindQuery)
	path := tableOf(t, c, "shop", "events").snapshotPath()
	c, e = restart(t, dir, c)
	require.FileExists(t, path)
	return c, e, path, dir
}

func TestSnapshot_EveryHeapWriteRemovesItFirst(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"INSERT", "INSERT INTO events (id, kind) VALUES (100, 'k1')"},
		{"UPDATE", "UPDATE events SET kind = 'k9' WHERE id = 1"},
		{"UPDATE of every row", "UPDATE events SET id = 5"},
		{"DELETE by the indexed column", "DELETE FROM events WHERE kind = 'k3'"},
		{"DELETE by a column with no index", "DELETE FROM events WHERE id = 1"},
		{"DELETE of everything", "DELETE FROM events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, path, _ := savedEvents(t)

			mustExec(t, e, "shop", tc.sql)

			assert.NoFileExists(t, path)
		})
	}
}

func TestSnapshot_StatementsThatWriteNothingKeepIt(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"SELECT", "SELECT id FROM events WHERE kind = 'k1'"},
		{"a SELECT that scans", "SELECT id FROM events WHERE id = 3"},
		{"a DELETE that matches nothing", "DELETE FROM events WHERE id = 999"},
		{"an UPDATE that matches nothing", "UPDATE events SET kind = 'z' WHERE id = 999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, path, _ := savedEvents(t)

			mustExec(t, e, "shop", tc.sql)

			assert.FileExists(t, path)
		})
	}
}

func TestSnapshot_ARejectedInsertKeepsIt(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE users (id INT PRIMARY KEY)")
	mustExec(t, e, "shop", "INSERT INTO users (id) VALUES (1)")
	mustExec(t, e, "shop", "SELECT id FROM users WHERE id = 1")
	path := tableOf(t, c, "shop", "users").snapshotPath()
	c, e = restart(t, dir, c)

	err := execError(t, e, "shop", "INSERT INTO users (id) VALUES (1)")

	assert.ErrorContains(t, err, "duplicate")
	assert.FileExists(t, path)
	_ = c
}

func TestSnapshot_AWriteAfterACrashIsNotMissedByTheNextStart(t *testing.T) {
	_, e, _, dir := savedEvents(t)
	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k3')")
	c := newTestCatalogAt(t, dir) // crash: the first catalog is never closed
	e = NewEngine(c)

	rows := mustExec(t, e, "shop", kindQuery).Rows

	assert.Contains(t, rows, []string{"100"}, "the new row is found: the trees were rebuilt, not loaded")
	assertIndexesMatchHeap(t, tableOf(t, c, "shop", "events"))
}

// A failed unlink must fail the statement before the heap is touched, because a
// write that went ahead could leave a snapshot that no longer matches.
func TestSnapshot_ARemovalThatFailsFailsTheWriteAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fileRemains bool
		fail        func(t *testing.T)
	}{
		{"the unlink fails", true, func(t *testing.T) {
			old := removeSnapshotFile
			removeSnapshotFile = func(string) error { return errInjected }
			t.Cleanup(func() { removeSnapshotFile = old })
		}},
		{"the directory fsync fails", false, func(t *testing.T) {
			old := syncSnapshotDir
			syncSnapshotDir = func(string) error { return errInjected }
			t.Cleanup(func() { syncSnapshotDir = old })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, e, path, _ := savedEvents(t)
			table := tableOf(t, c, "shop", "events")
			rowsBefore := fileRows(t, table)
			tc.fail(t)

			for _, sql := range []string{
				"INSERT INTO events (id, kind) VALUES (100, 'k1')",
				"UPDATE events SET kind = 'z' WHERE id = 1",
				"DELETE FROM events WHERE id = 1",
			} {
				assert.ErrorContains(t, execError(t, e, "shop", sql), errInjected.Error(), sql)
			}

			assert.Equal(t, rowsBefore, fileRows(t, table), "no statement wrote anything")
			if tc.fileRemains {
				assert.FileExists(t, path)
			}
		})
	}
}

func TestSnapshot_ARemovalIsRetriedByTheNextWrite(t *testing.T) {
	c, e, path, _ := savedEvents(t)
	old := syncSnapshotDir
	t.Cleanup(func() { syncSnapshotDir = old })
	syncSnapshotDir = func(string) error { return errInjected }
	require.Error(t, execError(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k1')"))
	syncSnapshotDir = old

	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k1')")

	assert.NoFileExists(t, path)
	assert.Len(t, fileRows(t, tableOf(t, c, "shop", "events")), 31)
}

func TestSnapshot_ARemovalIsOnceNotPerStatement(t *testing.T) {
	_, e, _, _ := savedEvents(t)
	syncs := 0
	old := syncSnapshotDir
	syncSnapshotDir = func(p string) error { syncs++; return old(p) }
	t.Cleanup(func() { syncSnapshotDir = old })

	for i := 100; i < 105; i++ {
		mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO events (id, kind) VALUES (%d, 'k1')", i))
	}

	assert.Equal(t, 1, syncs)
}

// A failed load removes the file, but not durably: the first write must still
// fsync the directory.
func TestSnapshot_AFailedLoadStillGetsItsDirectorySynced(t *testing.T) {
	c, e, path, _ := savedEvents(t)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	b[len(b)-100] ^= 0xFF
	require.NoError(t, os.WriteFile(path, b, 0o644))
	mustExec(t, e, "shop", kindQuery) // the load fails, the file goes
	require.NoFileExists(t, path)
	syncs := 0
	old := syncSnapshotDir
	syncSnapshotDir = func(p string) error { syncs++; return old(p) }
	t.Cleanup(func() { syncSnapshotDir = old })

	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k1')")

	assert.Equal(t, 1, syncs)
	_ = c
}

func TestSnapshot_ANewTableNeverHasOneToRemove(t *testing.T) {
	c := newTestCatalog(t)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE t (id INT)")
	syncs := 0
	old := syncSnapshotDir
	syncSnapshotDir = func(p string) error { syncs++; return old(p) }
	t.Cleanup(func() { syncSnapshotDir = old })

	mustExec(t, e, "shop", "INSERT INTO t (id) VALUES (1)")

	assert.Equal(t, 0, syncs)
}

func TestSnapshot_SchemaAndDefinitionChangesDoNotLeaveOneBehind(t *testing.T) {
	t.Run("CREATE INDEX", func(t *testing.T) {
		_, e, path, _ := savedEvents(t)
		mustExec(t, e, "shop", "CREATE INDEX by_id ON events (id)")
		assert.NoFileExists(t, path)
	})
	t.Run("DROP INDEX", func(t *testing.T) {
		_, e, path, _ := savedEvents(t)
		mustExec(t, e, "shop", "DROP INDEX by_kind ON events")
		assert.NoFileExists(t, path)
	})
	t.Run("ADD COLUMN gives the table a new file and removes the old snapshot", func(t *testing.T) {
		c, e, path, dir := savedEvents(t)
		mustExec(t, e, "shop", "ALTER TABLE events ADD COLUMN extra INT")
		assert.NoFileExists(t, path)
		assert.NotEqual(t, path, tableOf(t, c, "shop", "events").snapshotPath())
		syncs := 0
		old := syncSnapshotDir
		syncSnapshotDir = func(p string) error { syncs++; return old(p) }
		t.Cleanup(func() { syncSnapshotDir = old })
		mustExec(t, e, "shop", "INSERT INTO events (id, kind, extra) VALUES (100, 'k1', 1)")
		assert.Equal(t, 0, syncs, "the new file has no snapshot to remove")
		_ = dir
	})
	t.Run("DROP TABLE removes the snapshot with the heap", func(t *testing.T) {
		_, e, path, dir := savedEvents(t)
		mustExec(t, e, "shop", "DROP TABLE events")
		assert.NoFileExists(t, path)
		assert.Empty(t, namesIn(t, filepath.Join(dir, "data", "shop")))
	})
}

func TestSnapshot_ARunThatEndsAfterAWriteSavesTheNewTrees(t *testing.T) {
	c, e, _, dir := savedEvents(t)
	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k3')")
	mustExec(t, e, "shop", kindQuery) // builds the trees from the new heap

	c, e = restart(t, dir, c)

	cf := countOn(tableOf(t, c, "shop", "events"))
	assert.Contains(t, mustExec(t, e, "shop", kindQuery).Rows, []string{"100"})
	assert.EqualValues(t, 0, cf.scans.Load())
}

// The same random statements run on a table with a CREATE INDEX and on one
// without, across clean restarts and crashes (a new catalog over the same
// directory without closing the old one). Answers and errors must be equal
// throughout, which fails if any restart loads a snapshot that no longer
// matches its heap.
func TestSnapshot_RestartsNeverChangeAnAnswer(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			c := newTestCatalogAt(t, dir)
			e := NewEngine(c)
			mustExec(t, e, "", "CREATE DATABASE shop")
			mustExec(t, e, "shop", "CREATE TABLE indexed (id INT, kind VARCHAR(20))")
			mustExec(t, e, "shop", "CREATE INDEX by_kind ON indexed (kind)")
			mustExec(t, e, "shop", "CREATE TABLE plain (id INT, kind VARCHAR(20))")
			kind := func() string { return fmt.Sprintf("k%d", rng.Intn(5)) }
			where := func() string {
				switch rng.Intn(5) {
				case 0:
					return ""
				case 1:
					return fmt.Sprintf(" WHERE kind = '%s'", kind())
				case 2:
					return fmt.Sprintf(" WHERE kind >= '%s'", kind())
				case 3:
					return fmt.Sprintf(" WHERE kind < '%s'", kind())
				default:
					return fmt.Sprintf(" WHERE id = %d", rng.Intn(30))
				}
			}

			for op := 0; op < 300; op++ {
				var sql string
				switch rng.Intn(10) {
				case 0, 1, 2:
					sql = fmt.Sprintf("INSERT INTO %%s (id, kind) VALUES (%d, '%s')", rng.Intn(30), kind())
				case 3:
					sql = fmt.Sprintf("UPDATE %%s SET kind = '%s'%s", kind(), where())
				case 4:
					sql = "DELETE FROM %s" + where()
				default:
					sql = "SELECT id, kind FROM %s" + where()
				}
				gotResp, gotErr := runSQL(t, e, fmt.Sprintf(sql, "indexed"))
				wantResp, wantErr := runSQL(t, e, fmt.Sprintf(sql, "plain"))
				require.Equal(t, errText(wantErr), errText(gotErr), "op %d: %s", op, sql)
				require.Equal(t, wantResp.Rows, gotResp.Rows, "op %d: %s", op, sql)

				switch rng.Intn(25) {
				case 0:
					c, e = restart(t, dir, c)
				case 1:
					c = newTestCatalogAt(t, dir) // a crash
					e = NewEngine(c)
				}
			}
		})
	}
}

func runSQL(t *testing.T, e Engine, sql string) (Response, error) {
	t.Helper()
	statement, err := parser.NewParser(parser.NewLexer()).Parse(sql)
	require.NoError(t, err, sql)
	return e.Execute(statement, "shop")
}

// Two columns can swap names, so a tree's column name does not say which column
// it indexes. Here the unlink of the old snapshot fails, and the snapshot still
// has to be rejected when an index of the same name and column name is created
// on a different column.
func TestSnapshot_ASwappedColumnNameIsNotTheSameIndex(t *testing.T) {
	c, e, path, _ := savedEvents(t)
	old := removeSnapshotFile
	removeSnapshotFile = func(string) error { return errInjected }
	t.Cleanup(func() { removeSnapshotFile = old })
	mustExec(t, e, "shop", "DROP INDEX by_kind ON events")
	mustExec(t, e, "shop", "ALTER TABLE events RENAME COLUMN kind TO tmp")
	mustExec(t, e, "shop", "ALTER TABLE events RENAME COLUMN id TO kind")
	mustExec(t, e, "shop", "ALTER TABLE events RENAME COLUMN tmp TO id")
	mustExec(t, e, "shop", "CREATE INDEX by_kind ON events (kind)") // now the first column
	removeSnapshotFile = old
	require.FileExists(t, path, "the old snapshot survived the failed removals")
	_ = c

	rows := mustExec(t, e, "shop", "SELECT id FROM events WHERE kind = 3").Rows

	assert.Equal(t, [][]string{{"k3"}}, rows)
}

func TestSnapshot_NothingIsSavedAfterAHeapWriteFailed(t *testing.T) {
	c, e, path, dir := savedEvents(t)
	table := tableOf(t, c, "shop", "events")
	ff := &failingFile{File: table.file, failSyncs: true}
	table.file = ff
	require.Error(t, execError(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k3')"))
	ff.failSyncs = false
	mustExec(t, e, "shop", kindQuery) // rebuilds the trees from a heap that may not be on disk

	_, _ = restart(t, dir, c)

	assert.NoFileExists(t, path)
}

func TestSnapshot_AStaleEntryRemovesTheSnapshotAndTheTreesAreRebuilt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		removalFails bool
	}{
		{"the snapshot is removed", false},
		{"the removal fails, so the snapshot must not be loaded again", true},
	} {
		// A range on a secondary tree reads its rows with GetMany; an equality
		// on a key reads one row with Get. Both detect a stale entry.
		t.Run("range/"+tc.name, func(t *testing.T) {
			c, e, path, _ := savedEvents(t)
			table := tableOf(t, c, "shop", "events")
			set, err := table.indexSet()
			require.NoError(t, err)
			ids := idsOfName(t, set.secondary["by_kind"], "k3")
			require.NoError(t, table.file.Delete(ids[0])) // behind the index's back
			require.NoError(t, table.file.Sync())
			failRemoval(t, tc.removalFails)

			assert.Len(t, mustExec(t, e, "shop", kindQuery).Rows, 5, "the scan's answer")

			if !tc.removalFails {
				assert.NoFileExists(t, path, "a snapshot that led to a wrong entry is not kept")
			}
			cf := countOn(table)
			assert.Len(t, mustExec(t, e, "shop", kindQuery).Rows, 5)
			assert.EqualValues(t, 1, cf.scans.Load(), "the trees were rebuilt from the heap, not reloaded")
			assertIndexesMatchHeap(t, table)
		})
		t.Run("equality/"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := newTestCatalogAt(t, dir)
			e := NewEngine(c)
			mustExec(t, e, "", "CREATE DATABASE shop")
			mustExec(t, e, "shop", "CREATE TABLE users (id INT PRIMARY KEY, name VARCHAR(20))")
			for i := 0; i < 10; i++ {
				mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO users (id, name) VALUES (%d, 'n%d')", i, i))
			}
			mustExec(t, e, "shop", "SELECT name FROM users WHERE id = 1")
			path := tableOf(t, c, "shop", "users").snapshotPath()
			c, e = restart(t, dir, c)
			table := tableOf(t, c, "shop", "users")
			set, err := table.indexSet()
			require.NoError(t, err)
			key, err := index.EncodeKey(int64(4))
			require.NoError(t, err)
			id, ok := set.unique[0].Get(key)
			require.True(t, ok)
			require.NoError(t, table.file.Delete(id))
			require.NoError(t, table.file.Sync())
			failRemoval(t, tc.removalFails)

			assert.Empty(t, mustExec(t, e, "shop", "SELECT name FROM users WHERE id = 4").Rows)

			if !tc.removalFails {
				assert.NoFileExists(t, path)
			}
			cf := countOn(table)
			assert.Empty(t, mustExec(t, e, "shop", "SELECT name FROM users WHERE id = 4").Rows)
			assert.EqualValues(t, 1, cf.scans.Load(), "rebuilt from the heap")
			assertIndexesMatchHeap(t, table)
		})
	}
}

func failRemoval(t *testing.T, fail bool) {
	t.Helper()
	if !fail {
		return
	}
	old := removeSnapshotFile
	removeSnapshotFile = func(string) error { return errInjected }
	t.Cleanup(func() { removeSnapshotFile = old })
}
