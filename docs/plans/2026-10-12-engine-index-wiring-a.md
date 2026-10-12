# Engine index wiring, part A: lifecycle, uniqueness and maintenance. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give each table lazily built in-memory B+ tree indexes on its PRIMARY KEY and UNIQUE columns, keep them in step with every write, and use them for uniqueness checks, rejecting over-long keys, without changing any other statement's result.

**Architecture:** `SqlTable` gains a `tableIndexes` (a map from key column to `*index.Tree`, built from one heap scan on first use, discarded on any error that might leave it disagreeing with the file). Writes update the trees only after the heap write and `Sync` succeed. `checkUniqueness` and `Update`'s conflict check look keys up in the trees; a column the tree cannot hold keeps its scan check. The equality access path for `SELECT`/`UPDATE`/`DELETE` is part B, a separate PR.

**Tech Stack:** Go 1.26.4, `internal/index` (key codec and B+ tree), `internal/storage` (`RowID`, `File.Get`), testify.

**Spec:** `docs/specs/2026-10-12-engine-index-wiring-design.md`

## Global Constraints

- Go 1.26.4; `internal/engine` must never import `proto/sqlpb`; run `go test -race ./...` (required when touching `internal/engine`).
- Lock order: `Catalog.mu` → `Database.mu` → `Table.mu` → `tableIndexes.mu` → `catalogStore.mu`. `tableIndexes.mu` is a leaf held only while building or discarding; trees are mutated only under the table's write lock.
- The heap file is the only source of truth. Any write error (an `Insert`, `Delete` or `Sync` that fails) discards the indexes. A statement that fails validation before writing leaves them alone.
- A `RowID` is meaningful only within one file: indexes record the `fileID` they were built from and are never used for another.
- NULL is never indexed. A column whose values the tree cannot hold (a duplicate key, or a key over `maxKeyLen`) gets a nil tree and keeps its scan-based check.
- A PRIMARY KEY/UNIQUE string longer than `index.MaxKeyLenFor(index.DefaultNodeBytes)` (4084) is rejected with `key too long for column %q: %d bytes exceeds the maximum of %d`.
- Existing error messages and their order (duplicate keys, coercion errors) do not change.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify; every test is seen failing before the code that makes it pass. Run `gofmt -w internal/` after each edit; the snippets are gofmt-clean but deleting a stub leaves a stray blank line.

## Review Focus

- A key seen twice at build time (an interrupted UPDATE leaves two rows with one key) or over the limit must degrade only that column, not fail the build or lose the other column's tree. Pinned in Task 2 and Task 4.
- An `UPDATE` re-points every unique column of every updated row, including unchanged ones, and removes old entries before adding new ones so a row never collides with its own old entry. Pinned in Task 3 and by the differential test in Task 5.
- After `ADD`/`DROP COLUMN` the old trees must not be trusted, because old `RowID`s point at other rows in the new file. Pinned in Task 2 (the `fileID` guard) and Task 3.
- `UPDATE ... SET unique = <its own current value>` is not a conflict, and two matched rows still cannot receive one unique value. Pinned in Task 4.
- Behaviour with indexes equals behaviour without them, including errors and their order, for a long random mix of statements. Pinned in Task 5.

---

### Task 1: `index.MaxKeyLenFor`

**Files:**
- Modify: `internal/index/btree.go`
- Test: `internal/index/btree_test.go`

**Interfaces:**
- Consumes: the existing `nodeHeaderSize`, `minEntriesPerNode`, `keyLenSize`, `rowIDSize` and `New` in `btree.go`.
- Produces: `func MaxKeyLenFor(nodeBytes int) int`, the formula `New` already uses, so the engine can check a key's length without building a tree.

- [ ] **Step 1: Add a stub**

Append to `internal/index/btree.go`:

```go
// MaxKeyLenFor is a stub until the implementation step.
func MaxKeyLenFor(nodeBytes int) int { return 0 }
```

- [ ] **Step 2: Write the failing test**

Append to `internal/index/btree_test.go`:

```go
func TestMaxKeyLenFor(t *testing.T) {
	assert.Equal(t, 4084, MaxKeyLenFor(DefaultNodeBytes))
	assert.Equal(t, 12, MaxKeyLenFor(testNodeBytes))
	assert.Equal(t, 1, MaxKeyLenFor(52))
	assert.Equal(t, 0, MaxKeyLenFor(51))

	for _, nodeBytes := range []int{52, 96, 1000, DefaultNodeBytes} {
		tree, err := New(nodeBytes)
		require.NoError(t, err)
		assert.Equal(t, tree.MaxKeyLen(), MaxKeyLenFor(nodeBytes), "nodeBytes %d", nodeBytes)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/index/ -run MaxKeyLenFor 2>&1 | head -12`
Expected: FAIL: `TestMaxKeyLenFor` reports `4084` expected but `0` actual.

- [ ] **Step 4: Implement it**

In `internal/index/btree.go`, delete the stub and add this directly above the `// New returns an empty tree` comment:

```go
// MaxKeyLenFor is the longest key a tree with nodes of nodeBytes bytes
// accepts. It is below 1 when nodeBytes is too small for New.
func MaxKeyLenFor(nodeBytes int) int {
	return (nodeBytes-nodeHeaderSize)/minEntriesPerNode - (keyLenSize + rowIDSize)
}
```

In `New`, replace the line `maxKey := (nodeBytes-nodeHeaderSize)/minEntriesPerNode - (keyLenSize + rowIDSize)` with `maxKey := MaxKeyLenFor(nodeBytes)`.

- [ ] **Step 5: Run the package**

Run: `gofmt -w internal/index/ && go test -race ./internal/index/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/index/btree.go internal/index/btree_test.go
git commit -m "feat(index): expose MaxKeyLenFor" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Table indexes: lazy build, degrade and discard

**Files:**
- Create: `internal/engine/tableindex.go`
- Modify: `internal/engine/table.go` (two fields), `internal/engine/testhelpers_test.go`
- Test: `internal/engine/tableindex_test.go` (new)

**Interfaces:**
- Consumes: `index.New`, `index.EncodeKey`, `index.DefaultNodeBytes`, `index.MaxKeyLenFor` (Task 1); `SqlTable.file`, `fileID`, `Columns`; `DecodeRow`.
- Produces: `type tableIndexes`; `SqlTable.idx` and `SqlTable.noIndexes`; `maxKeyLen`; `(*SqlTable).keyColumns() []int`, `keyValues(row []any) []any`, `indexes() (map[int]*index.Tree, error)`, `builtIndexes() map[int]*index.Tree`, `dropIndexes()`, `buildIndexes()`; `checkKeyLength(col string, value any) error`. Test helpers `countingFile`, `countOn`, `keyedTable`, `insertUser`, `writeRaw`, `assertIndexesMatchHeap` and the column constants `colID`, `colEmail`, `colName`.

- [ ] **Step 1: Write the failing tests**

In `internal/engine/testhelpers_test.go`, add `"iter"` to the imports (after `"errors"`) and `"sync/atomic"` (after `"sort"`), and append:

```go
// countingFile wraps a storage.File and counts the scans and gets made on it.
type countingFile struct {
	storage.File
	scans atomic.Int64
	gets  atomic.Int64
}

func (f *countingFile) Scan() iter.Seq2[storage.Row, error] {
	f.scans.Add(1)
	return f.File.Scan()
}

func (f *countingFile) Get(id storage.RowID) ([]byte, error) {
	f.gets.Add(1)
	return f.File.Get(id)
}

// countOn replaces the table's file with a counting wrapper around it.
func countOn(table *SqlTable) *countingFile {
	cf := &countingFile{File: table.file}
	table.file = cf
	return cf
}
```

Create `internal/engine/tableindex_test.go`:

```go
package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

const (
	colID    = 0
	colEmail = 1
	colName  = 2
)

// keyedTable has a PRIMARY KEY (id), a UNIQUE column (email) and a plain one.
func keyedTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "email", DataType: parser.VarCharDataType{Size: 100}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
	})
	require.NoError(t, err)
	return table
}

func insertUser(t *testing.T, table *SqlTable, id int, email, name string) {
	t.Helper()
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok(fmt.Sprint(id)), strTok(email), strTok(name)),
	}))
}

// writeRaw stores row in the table's file without any of the statement-level
// checks, the way rows written by an older version or an interrupted UPDATE
// got there.
func writeRaw(t *testing.T, table *SqlTable, row []any) storage.RowID {
	t.Helper()
	b, err := EncodeRow(table.Columns, row)
	require.NoError(t, err)
	id, err := table.file.Insert(b)
	require.NoError(t, err)
	require.NoError(t, table.file.Sync())
	return id
}

func entries(tree *index.Tree) []string {
	out := []string{}
	for key, id := range tree.Range(nil, nil) {
		out = append(out, fmt.Sprintf("%x=%d/%d", key, id.Page, id.Slot))
	}
	return out
}

// assertIndexesMatchHeap fails unless the table's built indexes hold exactly
// what a fresh build from the heap would.
func assertIndexesMatchHeap(t *testing.T, table *SqlTable) {
	t.Helper()
	got := table.builtIndexes()
	require.NotNil(t, got, "the indexes must be built")
	want, err := table.buildIndexes()
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for col, wantTree := range want {
		gotTree, ok := got[col]
		require.True(t, ok, "column %d has no entry", col)
		if wantTree == nil {
			assert.Nil(t, gotTree, "column %d cannot be indexed", col)
			continue
		}
		require.NotNil(t, gotTree, "column %d lost its tree", col)
		assert.Equal(t, entries(wantTree), entries(gotTree), "column %d", col)
	}
}

func TestTableIndexes_BuildEveryKeyColumn(t *testing.T) {
	table := keyedTable(t)
	idA := writeRaw(t, table, []any{int64(1), "a@x.com", "A"})
	writeRaw(t, table, []any{int64(2), nil, "B"}) // a NULL is never indexed
	idC := writeRaw(t, table, []any{int64(3), "c@x.com", "C"})

	trees, err := table.indexes()

	require.NoError(t, err)
	require.Len(t, trees, 2, "one tree per PRIMARY KEY/UNIQUE column, none for name")
	assert.Equal(t, 3, trees[colID].Len())
	assert.Equal(t, 2, trees[colEmail].Len())
	key, err := index.EncodeKey("a@x.com")
	require.NoError(t, err)
	got, found := trees[colEmail].Get(key)
	assert.True(t, found)
	assert.Equal(t, idA, got)
	key, err = index.EncodeKey(int64(3))
	require.NoError(t, err)
	got, found = trees[colID].Get(key)
	assert.True(t, found)
	assert.Equal(t, idC, got)
}

func TestTableIndexes_BuiltOnceAndKeptUntilDropped(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "a@x.com", "A"})
	cf := countOn(table)
	assert.Nil(t, table.builtIndexes(), "nothing is built before first use")

	_, err := table.indexes()
	require.NoError(t, err)
	_, err = table.indexes()
	require.NoError(t, err)
	assert.EqualValues(t, 1, cf.scans.Load(), "the second call reuses the trees")
	assert.NotNil(t, table.builtIndexes())

	table.dropIndexes()
	assert.Nil(t, table.builtIndexes())
	_, err = table.indexes()
	require.NoError(t, err)
	assert.EqualValues(t, 2, cf.scans.Load(), "a dropped index is rebuilt from the heap")
}

func TestTableIndexes_RebuiltWhenTheFileChanges(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "a@x.com", "A"})
	cf := countOn(table)
	_, err := table.indexes()
	require.NoError(t, err)

	table.fileID++ // what adopting a rewritten file does
	_, err = table.indexes()

	require.NoError(t, err)
	assert.EqualValues(t, 2, cf.scans.Load(), "trees built from another file are never trusted")
}

func TestTableIndexes_DisabledMeansNoTrees(t *testing.T) {
	table := keyedTable(t)
	table.noIndexes = true
	cf := countOn(table)

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.Empty(t, trees)
	assert.Nil(t, table.builtIndexes())
	assert.EqualValues(t, 0, cf.scans.Load())
}

func TestTableIndexes_NoKeyColumnsNeedNoScan(t *testing.T) {
	table := usersTable(t) // no PRIMARY KEY, no UNIQUE
	cf := countOn(table)

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.NotNil(t, trees)
	assert.Empty(t, trees)
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.NotNil(t, table.builtIndexes(), "an empty set of trees counts as built")
}

func TestTableIndexes_ADuplicateKeyDegradesOnlyThatColumn(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "same@x.com", "A"})
	writeRaw(t, table, []any{int64(2), "same@x.com", "B"}) // what an interrupted UPDATE can leave

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.Nil(t, trees[colEmail], "email keeps its scan path")
	require.NotNil(t, trees[colID])
	assert.Equal(t, 2, trees[colID].Len())
}

func TestTableIndexes_AKeyOverTheLimitDegradesOnlyThatColumn(t *testing.T) {
	table, err := newTempTable(t, "docs", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "code", DataType: parser.TextDataType{}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
	})
	require.NoError(t, err)
	writeRaw(t, table, []any{int64(1), strings.Repeat("x", maxKeyLen+1)}) // written before such keys were rejected
	writeRaw(t, table, []any{int64(2), "short"})

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.Nil(t, trees[1], "code keeps its scan path")
	require.NotNil(t, trees[0])
	assert.Equal(t, 2, trees[0].Len())
}

func TestTableIndexes_AClosedTableCannotBuild(t *testing.T) {
	table := keyedTable(t)
	require.NoError(t, table.Close())

	trees, err := table.indexes()

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Nil(t, trees)
	assert.Nil(t, table.builtIndexes(), "a failed build caches nothing")
}

// Select holds only the read lock, so several readers can ask for the indexes
// at once; they must build them exactly once. Meaningful under -race.
func TestTableIndexes_ConcurrentReadersBuildOnce(t *testing.T) {
	table := keyedTable(t)
	for i := 0; i < 200; i++ {
		writeRaw(t, table, []any{int64(i), fmt.Sprintf("u%d@x.com", i), "n"})
	}
	cf := countOn(table)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			table.mu.RLock()
			defer table.mu.RUnlock()
			trees, err := table.indexes()
			if assert.NoError(t, err) {
				assert.Equal(t, 200, trees[colID].Len())
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, cf.scans.Load())
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go vet ./internal/engine/ 2>&1 | head`
Expected: FAIL to compile: `table.noIndexes undefined`, `table.indexes undefined`, `maxKeyLen undefined` and the like. The feature is missing.

- [ ] **Step 3: Implement the lifecycle**

In `internal/engine/table.go`, replace the end of the `SqlTable` struct

```go
	store    *catalogStore
	database string
}
```

with:

```go
	store    *catalogStore
	database string

	// idx caches indexes of the file; see tableIndexes. noIndexes turns them
	// off so tests can compare behaviour with and without.
	idx       tableIndexes
	noIndexes bool
}
```

Create `internal/engine/tableindex.go`:

```go
package engine

import (
	"errors"
	"fmt"
	"sync"

	"manhhung2111/go-sql/internal/index"
)

// maxKeyLen is the longest PRIMARY KEY/UNIQUE string the indexes can hold.
var maxKeyLen = index.MaxKeyLenFor(index.DefaultNodeBytes)

// tableIndexes holds the in-memory indexes of one table: a cache of its heap
// file, built on first use and discarded whenever it might disagree with the
// file. Callers hold the table's mu; mu here only serialises building and
// discarding, and is a leaf lock (Table.mu, then tableIndexes.mu).
type tableIndexes struct {
	mu sync.Mutex
	// fileID is the file the trees were built from. A RowID means something
	// only within one file, so trees built from another file are never used.
	fileID int64
	// trees maps a PRIMARY KEY/UNIQUE column's schema index to its tree. A nil
	// map means "not built"; a nil tree means the column cannot be indexed
	// (see buildIndexes) and its checks scan the file instead.
	trees map[int]*index.Tree
}

// keyColumns returns the schema indexes of the PRIMARY KEY and UNIQUE columns.
func (t *SqlTable) keyColumns() []int {
	cols := make([]int, 0)
	for i, col := range t.Columns {
		if col.IsPrimaryKey() || col.IsUnique() {
			cols = append(cols, i)
		}
	}
	return cols
}

// keyValues returns row's values at the key columns, in keyColumns order.
func (t *SqlTable) keyValues(row []any) []any {
	cols := t.keyColumns()
	values := make([]any, len(cols))
	for i, col := range cols {
		values[i] = row[col]
	}
	return values
}

// checkKeyLength rejects a string too long to be indexed. It applies to the
// values of PRIMARY KEY and UNIQUE columns only; callers check that.
func checkKeyLength(col string, value any) error {
	if s, ok := value.(string); ok && len(s) > maxKeyLen {
		return fmt.Errorf("key too long for column %q: %d bytes exceeds the maximum of %d", col, len(s), maxKeyLen)
	}
	return nil
}

// indexes returns the table's trees, building them from one scan of the file
// if they are missing or were built from another file. Callers hold t.mu, for
// reading or writing. With noIndexes set it returns no trees, so every column
// takes its scan path.
func (t *SqlTable) indexes() (map[int]*index.Tree, error) {
	if t.noIndexes {
		return map[int]*index.Tree{}, nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.trees != nil && t.idx.fileID == t.fileID {
		return t.idx.trees, nil
	}
	if t.file == nil {
		return nil, fmt.Errorf("table %q is closed", t.Name)
	}
	trees, err := t.buildIndexes()
	if err != nil {
		return nil, err
	}
	t.idx.trees, t.idx.fileID = trees, t.fileID
	return trees, nil
}

// builtIndexes returns the trees if they are built for the current file, and
// nil otherwise. It never builds.
func (t *SqlTable) builtIndexes() map[int]*index.Tree {
	if t.noIndexes {
		return nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.trees != nil && t.idx.fileID == t.fileID {
		return t.idx.trees
	}
	return nil
}

// dropIndexes discards the trees; the next indexes call rebuilds them from the
// heap. Writers call it after any failure that may have left the file and the
// trees disagreeing.
func (t *SqlTable) dropIndexes() {
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	t.idx.trees = nil
}

// buildIndexes scans the file once and indexes every non-NULL PRIMARY
// KEY/UNIQUE value. A column whose values the tree cannot hold gets a nil tree
// and keeps using its scan path: a key seen twice (an interrupted UPDATE can
// leave two rows with one key) or longer than maxKeyLen (written before such
// values were rejected). Any other error aborts the build.
func (t *SqlTable) buildIndexes() (map[int]*index.Tree, error) {
	trees := make(map[int]*index.Tree)
	for _, col := range t.keyColumns() {
		tree, err := index.New(index.DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		trees[col] = tree
	}
	if len(trees) == 0 {
		return trees, nil
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return nil, err
		}
		for col, tree := range trees {
			if tree == nil || row[col] == nil {
				continue
			}
			key, err := index.EncodeKey(row[col])
			if err != nil {
				return nil, err
			}
			if err := tree.Insert(key, fileRow.ID); err != nil {
				if !errors.Is(err, index.ErrDuplicateKey) && !errors.Is(err, index.ErrKeyTooLong) {
					return nil, err
				}
				trees[col] = nil
			}
		}
	}
	return trees, nil
}
```

- [ ] **Step 4: Run them to verify they pass**

Run: `gofmt -w internal/ && go test -race -count=1 ./internal/engine/ -run TableIndexes -v 2>&1 | grep -E '^(--- |FAIL|ok|PASS)'`
Expected: PASS for the ten `TestTableIndexes_*` tests, including the concurrent build.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/tableindex.go internal/engine/table.go internal/engine/testhelpers_test.go internal/engine/tableindex_test.go
git commit -m "feat(engine): build a table's key indexes lazily from its heap file" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Keep the indexes in step with every write

**Files:**
- Modify: `internal/engine/tableindex.go`, `internal/engine/table.go`
- Test: `internal/engine/tableindex_test.go`

**Interfaces:**
- Consumes: Task 2's `builtIndexes`, `dropIndexes`, `keyColumns`, `keyValues`, `buildIndexes`, `assertIndexesMatchHeap`.
- Produces: `(*SqlTable).indexInserted(ids []storage.RowID, rows [][]any)`, `indexDeleted(keys [][]any)`, `indexMoved(oldKeys, rows [][]any, ids []storage.RowID)`, `removeKeys`; `appendToFile`, `deleteFromFile`, `Update` and `closeLocked` that call them and discard the indexes on any write error. Nothing reads the trees yet (Task 4), so no statement changes behaviour in this task.

- [ ] **Step 1: Write the failing tests**

Append to `internal/engine/tableindex_test.go`:

```go
// builtKeyedTable is a keyedTable holding five rows whose indexes are built.
func builtKeyedTable(t *testing.T) *SqlTable {
	t.Helper()
	table := keyedTable(t)
	for i := 1; i <= 5; i++ {
		writeRaw(t, table, []any{int64(i), fmt.Sprintf("u%d@x.com", i), fmt.Sprintf("n%d", i)})
	}
	_, err := table.indexes()
	require.NoError(t, err)
	return table
}

func TestTableIndexes_InsertAddsTheNewRows(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("6"), strTok("u6@x.com"), strTok("n6")),
		tokRow(numTok("7"), strTok("u7@x.com"), strTok("n7")),
	}))
	require.NoError(t, table.InsertValues([]string{"id"}, [][]parser.Token{tokRow(numTok("8"))})) // email stays NULL

	assertIndexesMatchHeap(t, table)
	assert.Equal(t, 8, table.builtIndexes()[colID].Len())
	assert.Equal(t, 7, table.builtIndexes()[colEmail].Len())
}

func TestTableIndexes_DeleteRemovesTheRows(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.GT, numTok("3"))))
	assertIndexesMatchHeap(t, table)
	assert.Equal(t, 3, table.builtIndexes()[colID].Len())

	require.NoError(t, table.Delete(nil))
	assertIndexesMatchHeap(t, table)
	assert.Equal(t, 0, table.builtIndexes()[colID].Len())
}

func TestTableIndexes_DeleteDoesNotBuildThem(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "a@x.com", "A"})

	require.NoError(t, table.Delete(nil))

	assert.Nil(t, table.builtIndexes(), "only statements that need an index build one")
}

func TestTableIndexes_UpdateRepointsEveryUpdatedRow(t *testing.T) {
	t.Run("a key column changes", func(t *testing.T) {
		table := builtKeyedTable(t)

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("new@x.com")}},
			cmp(identTok("id"), parser.EQ, numTok("2"))))

		assertIndexesMatchHeap(t, table)
		key, err := index.EncodeKey("new@x.com")
		require.NoError(t, err)
		_, found := table.builtIndexes()[colEmail].Get(key)
		assert.True(t, found)
		key, err = index.EncodeKey("u2@x.com")
		require.NoError(t, err)
		_, found = table.builtIndexes()[colEmail].Get(key)
		assert.False(t, found, "the old value is gone")
	})

	t.Run("only a plain column changes, on many rows", func(t *testing.T) {
		table := builtKeyedTable(t)

		require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("same")}}, nil))

		assertIndexesMatchHeap(t, table) // every row moved to a new RowID
		assert.Equal(t, 5, table.builtIndexes()[colID].Len())
	})

	t.Run("no row matches", func(t *testing.T) {
		table := builtKeyedTable(t)

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "name", Value: strTok("x")}},
			cmp(identTok("id"), parser.EQ, numTok("99"))))

		assertIndexesMatchHeap(t, table)
	})
}

func TestTableIndexes_AWriteThatFailsDiscardsThemAndTheyRebuildCorrectly(t *testing.T) {
	tests := []struct {
		name string
		arm  func(ff *failingFile)
		run  func(table *SqlTable) error
	}{
		{"insert fails", func(ff *failingFile) { ff.failInserts = true }, func(table *SqlTable) error {
			return table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("z@x.com"), strTok("z"))})
		}},
		{"sync fails after an insert", func(ff *failingFile) { ff.failSyncs = true }, func(table *SqlTable) error {
			return table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("z@x.com"), strTok("z"))})
		}},
		{"delete fails", func(ff *failingFile) { ff.failDeletes = 1 }, func(table *SqlTable) error {
			return table.Delete(cmp(identTok("id"), parser.GT, numTok("1")))
		}},
		{"sync fails after a delete", func(ff *failingFile) { ff.failSyncs = true }, func(table *SqlTable) error {
			return table.Delete(nil)
		}},
		{"update's insert fails", func(ff *failingFile) { ff.failInserts = true }, func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, nil)
		}},
		{"update's delete fails", func(ff *failingFile) { ff.failDeletes = 1 }, func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, nil)
		}},
		{"update's sync fails", func(ff *failingFile) { ff.failSyncs = true }, func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := builtKeyedTable(t)
			ff := &failingFile{File: table.file}
			table.file = ff
			tc.arm(ff)

			err := tc.run(table)

			require.ErrorIs(t, err, errInjected)
			assert.Nil(t, table.builtIndexes(), "the indexes may disagree with the file, so they are gone")

			*ff = failingFile{File: ff.File} // the disk behaves again
			_, err = table.indexes()
			require.NoError(t, err)
			assertIndexesMatchHeap(t, table)
		})
	}
}

func TestTableIndexes_AStatementThatFailsBeforeWritingKeepsThem(t *testing.T) {
	table := builtKeyedTable(t)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(strTok("not a number"), strTok("z@x.com"), strTok("z"))})
	require.Error(t, err)

	assert.NotNil(t, table.builtIndexes())
	assertIndexesMatchHeap(t, table)
}

func TestTableIndexes_AddAndDropColumnDiscardThem(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}}))
	assert.Nil(t, table.builtIndexes(), "an ALTER rewrites the file, so every RowID is new")
	_, err := table.indexes()
	require.NoError(t, err)
	assertIndexesMatchHeap(t, table)

	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))
	assert.Nil(t, table.builtIndexes())
	_, err = table.indexes()
	require.NoError(t, err)
	assertIndexesMatchHeap(t, table)
}

func TestTableIndexes_RenamingAColumnKeepsThem(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "nickname"}))

	assert.NotNil(t, table.builtIndexes(), "no row moved and no column index changed")
	assertIndexesMatchHeap(t, table)
}

func TestTableIndexes_CloseAndDropDiscardThem(t *testing.T) {
	closed := builtKeyedTable(t)
	require.NoError(t, closed.Close())
	assert.Nil(t, closed.builtIndexes())

	dropped := builtKeyedTable(t)
	require.NoError(t, dropped.Drop())
	assert.Nil(t, dropped.builtIndexes())
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/engine/ -run TableIndexes 2>&1 | grep -E '^(\s*--- FAIL|FAIL|ok)' | head -20`
Expected: FAIL. `InsertAddsTheNewRows`, `DeleteRemovesTheRows`, `UpdateRepointsEveryUpdatedRow`, `AWriteThatFailsDiscardsThem...` and `CloseAndDropDiscardThem` fail because nothing updates or discards the trees yet. The Task 2 tests still pass, and so do `DeleteDoesNotBuildThem`, `AStatementThatFailsBeforeWritingKeepsThem`, `RenamingAColumnKeepsThem` and `AddAndDropColumnDiscardThem` (the last because the `fileID` guard already refuses trees from another file; the explicit drop frees their memory).

- [ ] **Step 3: Implement the maintenance**

In `internal/engine/tableindex.go`, add `"manhhung2111/go-sql/internal/storage"` to the imports (after the `index` import) and append:

```go
// indexInserted records rows just written to the file at ids. A built index
// that cannot take an entry is discarded, never left half updated.
func (t *SqlTable) indexInserted(ids []storage.RowID, rows [][]any) {
	trees := t.builtIndexes()
	if trees == nil {
		return
	}
	for col, tree := range trees {
		if tree == nil {
			continue
		}
		for i, row := range rows {
			if row[col] == nil {
				continue
			}
			key, err := index.EncodeKey(row[col])
			if err == nil {
				err = tree.Insert(key, ids[i])
			}
			if err != nil {
				t.dropIndexes()
				return
			}
		}
	}
}

// indexDeleted removes rows just tombstoned. keys[i] holds row i's values at
// the key columns, in keyColumns order. An entry that is missing means the
// index was wrong, so it is discarded.
func (t *SqlTable) indexDeleted(keys [][]any) {
	trees := t.builtIndexes()
	if trees == nil {
		return
	}
	if !t.removeKeys(trees, keys) {
		t.dropIndexes()
	}
}

// indexMoved records an UPDATE: every updated row got a new RowID, so its old
// entries (oldKeys, as in indexDeleted) are removed first and the new values
// are added at the new ids. Unchanged keys are re-pointed too.
func (t *SqlTable) indexMoved(oldKeys [][]any, rows [][]any, ids []storage.RowID) {
	trees := t.builtIndexes()
	if trees == nil {
		return
	}
	if !t.removeKeys(trees, oldKeys) {
		t.dropIndexes()
		return
	}
	for col, tree := range trees {
		if tree == nil {
			continue
		}
		for i, row := range rows {
			if row[col] == nil {
				continue
			}
			key, err := index.EncodeKey(row[col])
			if err == nil {
				err = tree.Insert(key, ids[i])
			}
			if err != nil {
				t.dropIndexes()
				return
			}
		}
	}
}

// removeKeys deletes each row's entries from trees and reports whether every
// entry was there.
func (t *SqlTable) removeKeys(trees map[int]*index.Tree, keys [][]any) bool {
	for j, col := range t.keyColumns() {
		tree := trees[col]
		if tree == nil {
			continue
		}
		for _, values := range keys {
			if values[j] == nil {
				continue
			}
			key, err := index.EncodeKey(values[j])
			if err != nil || !tree.Delete(key) {
				return false
			}
		}
	}
	return true
}
```

In `internal/engine/table.go`, replace each of these whole functions, including their doc comments, with the version below.

`appendToFile`:

```go
// appendToFile writes rows to the table's file with a single fsync and then
// records them in the table's indexes. Every row is encoded and size-checked
// before the first write, so a bad batch writes nothing; only an I/O error can
// leave it partly written, and the engine has no WAL to undo that. Such an
// error discards the indexes, so they are rebuilt from whatever the file now
// holds. Callers hold t.mu.
func (t *SqlTable) appendToFile(rows [][]any) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	encoded, err := encodeRows(t.Columns, rows)
	if err != nil {
		return err
	}

	ids := make([]storage.RowID, 0, len(encoded))
	for _, b := range encoded {
		id, err := t.file.Insert(b)
		if err != nil {
			t.dropIndexes()
			return err
		}
		ids = append(ids, id)
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexInserted(ids, rows)
	return nil
}
```

`deleteFromFile`:

```go
// deleteFromFile tombstones every live row of the table's file that matches
// where, then fsyncs once, then removes the rows from the table's indexes.
// The matching rows are collected before the first tombstone is written, so
// an evaluation error touches nothing, and a row is never tombstoned while
// the scan is still reading its page; only an I/O error can leave the delete
// partly applied, and the engine has no WAL to undo that. Such an error
// discards the indexes. Callers hold t.mu.
func (t *SqlTable) deleteFromFile(where parser.Expression, columnIndex map[string]int) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	// Only the key values of the matching rows are kept, and only if there is
	// an index to update.
	trackKeys := t.builtIndexes() != nil
	var matched []storage.RowID
	var keys [][]any
	for row, err := range t.file.Scan() {
		if err != nil {
			return err
		}
		decoded, err := DecodeRow(t.Columns, row.Bytes)
		if err != nil {
			return err
		}
		isMatch, err := evalWhere(where, decoded, columnIndex, t.Columns)
		if err != nil {
			return err
		}
		if isMatch {
			matched = append(matched, row.ID)
			if trackKeys {
				keys = append(keys, t.keyValues(decoded))
			}
		}
	}

	for _, id := range matched {
		if err := t.file.Delete(id); err != nil {
			t.dropIndexes()
			return err
		}
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexDeleted(keys)
	return nil
}
```

`Update`:

```go
// Update applies assignments to every row matching where (a nil where
// matches every row, same as Delete), enforcing PRIMARY KEY/UNIQUE on any
// assigned column. NOT NULL is a non-issue here: the parser only accepts a
// STRING or NUMBER literal on the right of SET, so an assignment can never
// produce NULL. The table's file is scanned once: it finds the matching rows
// and, for the uniqueness checks, whether any row that is not being updated
// already holds an assigned value. Every new version is encoded and
// size-checked before anything is written, and the new versions are inserted
// before the old ones are tombstoned, so a failure partway through (a
// coercion error, a uniqueness violation, a row too large for a page) leaves
// the table untouched, and a crash between the two writes can duplicate a row
// but never lose one.
// An I/O error during the writes discards the indexes; otherwise every updated
// row is re-pointed at its new RowID.
func (t *SqlTable) Update(assignments []parser.Assignment, where parser.Expression) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	resolved := make([]resolvedAssignment, len(assignments))
	for i, a := range assignments {
		idx, ok := columnIndex[a.Column]
		if !ok {
			return fmt.Errorf("unknown column %q", a.Column)
		}
		coerced, err := coerceValue(t.Columns[idx].DataType, a.Value)
		if err != nil {
			return fmt.Errorf("column %q: %v", t.Columns[idx].Name, err)
		}
		resolved[i] = resolvedAssignment{index: idx, value: coerced}
	}

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	// The PRIMARY KEY/UNIQUE columns being assigned, in assignment order. A
	// SET column = literal is static, so every matched row gets the same value.
	uniqueColumns := make([]int, 0)
	assigned := make(map[int]any, len(resolved))
	for _, a := range resolved {
		assigned[a.index] = a.value
		col := t.Columns[a.index]
		if col.IsPrimaryKey() || col.IsUnique() {
			uniqueColumns = append(uniqueColumns, a.index)
		}
	}

	// If the indexes are built, remember each matched row's old key values so
	// they can be removed after the write.
	trackKeys := t.builtIndexes() != nil

	var oldIDs []storage.RowID
	var oldKeys [][]any
	var updated [][]any
	conflicts := make(map[int]bool, len(uniqueColumns))
	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return err
		}
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return err
		}
		if !matched {
			// A column with an index is checked after the scan; one without
			// is checked here. A matched row's own old value is deliberately
			// excluded, so re-assigning a column back to its current value
			// never self-conflicts.
			for _, idx := range uniqueColumns {
				if row[idx] != nil && row[idx] == assigned[idx] {
					conflicts[idx] = true
				}
			}
			continue
		}
		oldIDs = append(oldIDs, fileRow.ID)
		if trackKeys {
			oldKeys = append(oldKeys, t.keyValues(row))
		}
		for _, a := range resolved {
			row[a.index] = a.value
		}
		updated = append(updated, row)
	}

	if len(updated) > 0 {
		for _, idx := range uniqueColumns {
			if conflicts[idx] {
				return fmt.Errorf("duplicate entry %v for column %q", assigned[idx], t.Columns[idx].Name)
			}
		}
		// Two matched rows would both receive the assigned value; the old
		// row-by-row check hit that on the second row, on the first unique
		// column in assignment order.
		if len(updated) > 1 && len(uniqueColumns) > 0 {
			idx := uniqueColumns[0]
			return fmt.Errorf("duplicate entry %v for column %q", assigned[idx], t.Columns[idx].Name)
		}
	}

	encoded, err := encodeRows(t.Columns, updated)
	if err != nil {
		return err
	}
	newIDs := make([]storage.RowID, 0, len(encoded))
	for _, b := range encoded {
		id, err := t.file.Insert(b)
		if err != nil {
			t.dropIndexes()
			return err
		}
		newIDs = append(newIDs, id)
	}
	for _, id := range oldIDs {
		if err := t.file.Delete(id); err != nil {
			t.dropIndexes()
			return err
		}
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexMoved(oldKeys, updated, newIDs)
	return nil
}
```

`closeLocked`:

```go
func (t *SqlTable) closeLocked() error {
	t.dropIndexes()
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}
```

- [ ] **Step 4: Run the whole engine suite**

Run: `gofmt -w internal/ && go vet ./internal/... && go test -race -count=1 ./internal/engine/`
Expected: PASS: the new tests and every existing engine test, since no statement reads the trees yet.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/tableindex.go internal/engine/table.go internal/engine/tableindex_test.go
git commit -m "feat(engine): keep a table's indexes in step with its writes" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Uniqueness checks through the indexes, and the key length limit

**Files:**
- Modify: `internal/engine/table.go`
- Test: `internal/engine/wiring_test.go` (new)

**Interfaces:**
- Consumes: Task 2's `indexes`, `checkKeyLength`, `maxKeyLen`; Task 3's maintenance; the helpers `keyedTable`, `builtKeyedTable`, `writeRaw`, `countOn`, `assertIndexesMatchHeap`.
- Produces: `checkUniqueness` and `Update` that use the trees (and still scan for a column with no tree); the "key too long" error from `INSERT`, `UPDATE` and `ADD COLUMN`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/wiring_test.go`:

```go
package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// seededKeyedTable is a keyedTable with five rows whose indexes are built,
// watched by a counting file whose counters start at zero.
func seededKeyedTable(t *testing.T) (*SqlTable, *countingFile) {
	t.Helper()
	table := builtKeyedTable(t)
	cf := countOn(table)
	return table, cf
}

func TestUniqueness_UsesTheIndexInsteadOfScanning(t *testing.T) {
	table, cf := seededKeyedTable(t)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("fresh@x.com"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry 3 for column "id"`)

	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("u4@x.com"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry u4@x.com for column "email"`)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("u9@x.com"), strTok("n"))}))

	assert.EqualValues(t, 0, cf.scans.Load(), "checking keys must not read the table")
	assertIndexesMatchHeap(t, table)
}

func TestUniqueness_TheFirstStatementBuildsTheIndexWithOneScan(t *testing.T) {
	table := keyedTable(t)
	cf := countOn(table)
	insertUser(t, table, 1, "a@x.com", "A")
	cf.scans.Store(0)

	insertUser(t, table, 2, "b@x.com", "B")
	insertUser(t, table, 3, "c@x.com", "C")

	assert.EqualValues(t, 0, cf.scans.Load())
	assert.NotNil(t, table.builtIndexes())
}

func TestUniqueness_ABatchChecksItsOwnRowsToo(t *testing.T) {
	table, _ := seededKeyedTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("10"), strTok("x@x.com"), strTok("n")),
		tokRow(numTok("11"), strTok("x@x.com"), strTok("n")),
	})

	assert.EqualError(t, err, `duplicate entry x@x.com for column "email"`)
	assert.Len(t, fileRows(t, table), 5, "a rejected batch writes nothing")
}

func TestUniqueness_AKeyIsFreeAgainAfterItsRowIsDeleted(t *testing.T) {
	table, _ := seededKeyedTable(t)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("2"))))

	insertUser(t, table, 2, "u2@x.com", "again")
	assertIndexesMatchHeap(t, table)
}

func TestUniqueness_UpdateMovesAKeyAndFreesTheOldOne(t *testing.T) {
	table, _ := seededKeyedTable(t)

	require.NoError(t, table.Update(
		[]parser.Assignment{{Column: "email", Value: strTok("moved@x.com")}},
		cmp(identTok("id"), parser.EQ, numTok("2"))))

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("moved@x.com"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry moved@x.com for column "email"`)
	insertUser(t, table, 9, "u2@x.com", "takes the old email")
	assertIndexesMatchHeap(t, table)
}

func TestUniqueness_UpdateConflictsAreFoundThroughTheIndex(t *testing.T) {
	t.Run("another row holds the value", func(t *testing.T) {
		table, _ := seededKeyedTable(t)

		err := table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("u4@x.com")}},
			cmp(identTok("id"), parser.EQ, numTok("2")))

		assert.EqualError(t, err, `duplicate entry u4@x.com for column "email"`)
		assertIndexesMatchHeap(t, table)
		assert.Len(t, fileRows(t, table), 5)
	})

	t.Run("re-assigning a row's own value is not a conflict", func(t *testing.T) {
		table, _ := seededKeyedTable(t)

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("u2@x.com")}},
			cmp(identTok("id"), parser.EQ, numTok("2"))))

		assertIndexesMatchHeap(t, table)
	})

	t.Run("two matched rows cannot share one value", func(t *testing.T) {
		table, _ := seededKeyedTable(t)

		err := table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("one@x.com")}},
			cmp(identTok("id"), parser.LTE, numTok("2")))

		assert.EqualError(t, err, `duplicate entry one@x.com for column "email"`)
		assertIndexesMatchHeap(t, table)
	})
}

func TestUniqueness_ADegradedColumnStillChecksByScanning(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "same@x.com", "A"})
	writeRaw(t, table, []any{int64(2), "same@x.com", "B"}) // an interrupted UPDATE left two rows with one email
	cf := countOn(table)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("same@x.com"), strTok("C"))})
	assert.EqualError(t, err, `duplicate entry same@x.com for column "email"`)
	assert.GreaterOrEqual(t, cf.scans.Load(), int64(1), "email has no tree, so it is checked by scanning")

	insertUser(t, table, 3, "fresh@x.com", "C")
	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("other@x.com"), strTok("D"))})
	assert.EqualError(t, err, `duplicate entry 3 for column "id"`, "id still has its tree")
	assert.Nil(t, table.builtIndexes()[colEmail])
	assert.NotNil(t, table.builtIndexes()[colID])
}

func codesTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "docs", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "code", DataType: parser.TextDataType{}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
		{Name: "note", DataType: parser.TextDataType{}},
	})
	require.NoError(t, err)
	return table
}

func TestLongKeys_AreRejectedAtTheLimit(t *testing.T) {
	atLimit := strings.Repeat("k", maxKeyLen)
	overLimit := strings.Repeat("k", maxKeyLen+1)
	wantErr := fmt.Sprintf(`key too long for column "code": %d bytes exceeds the maximum of %d`, maxKeyLen+1, maxKeyLen)

	t.Run("INSERT", func(t *testing.T) {
		table := codesTable(t)

		err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok(overLimit), strTok("n"))})
		assert.EqualError(t, err, wantErr)
		assertTableRows(t, table)

		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok(atLimit), strTok("n"))}))
		assertTableRows(t, table, []any{int64(1), atLimit, "n"})
	})

	t.Run("INSERT of a long value in a column that is not a key is fine", func(t *testing.T) {
		table := codesTable(t)

		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok(overLimit))}))
	})

	t.Run("a long value is reported where a duplicate would be", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))

		err := table.InsertValues(nil, [][]parser.Token{
			tokRow(numTok("1"), strTok(overLimit), strTok("n")), // duplicate id comes first in column order
		})

		assert.EqualError(t, err, `duplicate entry 1 for column "id"`)
	})

	t.Run("UPDATE", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))

		err := table.Update([]parser.Assignment{{Column: "code", Value: strTok(overLimit)}}, nil)

		assert.EqualError(t, err, wantErr)
		assertTableRows(t, table, []any{int64(1), "c", "n"})
		require.NoError(t, table.Update([]parser.Assignment{{Column: "code", Value: strTok(atLimit)}}, nil))
	})

	t.Run("ADD COLUMN with a long default", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))
		column := func(def string) parser.ColumnDefinition {
			return parser.ColumnDefinition{Name: "slug", DataType: parser.TextDataType{},
				Constraints: []parser.Constraint{parser.UniqueConstraint{}, parser.DefaultConstraint{DefaultValue: strTok(def)}}}
		}

		err := table.AlterColumns(parser.AddColumnAction{Column: column(overLimit)})
		assert.EqualError(t, err, strings.Replace(wantErr, `"code"`, `"slug"`, 1))
		assert.Len(t, table.Columns, 3, "the schema is unchanged")

		require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: column(atLimit)}))
	})
}

func TestLongKeys_RowsWrittenBeforeTheRuleKeepWorking(t *testing.T) {
	table := codesTable(t)
	long := strings.Repeat("k", maxKeyLen+10)
	writeRaw(t, table, []any{int64(1), long, "legacy"})

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("short"), strTok("n"))}))
	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("short"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry short for column "code"`, "code is checked by scanning")
	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("other"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry 1 for column "id"`)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("1"))))
	assertTableRows(t, table, []any{int64(2), "short", "n"})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/engine/ -run 'Uniqueness|LongKeys' 2>&1 | grep -E '^(\s*--- FAIL|FAIL|ok)' | head -20`
Expected: FAIL. `UsesTheIndexInsteadOfScanning` and `TheFirstStatementBuildsTheIndexWithOneScan` count scans where the tests expect none, `ADegradedColumnStillChecksByScanning` finds no built indexes, and the `LongKeys_AreRejectedAtTheLimit` subtests for INSERT, UPDATE and ADD COLUMN get no `key too long` error. The other uniqueness tests and `LongKeys_RowsWrittenBeforeTheRuleKeepWorking` already pass, because the scan gives the same answers.

- [ ] **Step 3: Implement**

In `internal/engine/table.go`, add `"manhhung2111/go-sql/internal/index"` to the imports (before the `parser` import), and replace these two whole functions, including doc comments.

`checkUniqueness`:

```go
// checkUniqueness reports the first PRIMARY KEY/UNIQUE violation among
// newRows, checked in row order against the rows already in the table and
// against the earlier rows of the batch, and rejects a string too long to be
// indexed at the point where a duplicate would be reported. A column with an
// index is checked by looking each value up; a column without one (indexes
// are off, or it could not be indexed) streams the file once against the
// statement's own key values, so memory is proportional to the statement.
// NULLs never conflict.
func (t *SqlTable) checkUniqueness(newRows [][]any, uniqueColumns []int) error {
	candidates := make(map[int]map[any]bool, len(uniqueColumns))
	anyCandidate := false
	for _, idx := range uniqueColumns {
		set := make(map[any]bool, len(newRows))
		for _, row := range newRows {
			if v := row[idx]; v != nil {
				set[v] = true
				anyCandidate = true
			}
		}
		candidates[idx] = set
	}
	if !anyCandidate {
		return nil
	}

	trees, err := t.indexes()
	if err != nil {
		return err
	}
	scanned := make(map[int]map[any]bool)
	for _, idx := range uniqueColumns {
		if trees[idx] == nil {
			scanned[idx] = candidates[idx]
		}
	}
	seen := make(map[int]map[any]bool, len(uniqueColumns))
	if len(scanned) > 0 {
		if seen, err = t.existingKeys(scanned); err != nil {
			return err
		}
	}
	for _, idx := range uniqueColumns {
		if seen[idx] == nil {
			seen[idx] = make(map[any]bool)
		}
	}

	for _, row := range newRows {
		for _, idx := range uniqueColumns {
			value := row[idx]
			if value == nil {
				continue // multiple NULLs are allowed in a UNIQUE column
			}
			if err := checkKeyLength(t.Columns[idx].Name, value); err != nil {
				return err
			}
			duplicate := seen[idx][value]
			if tree := trees[idx]; tree != nil && !duplicate {
				key, err := index.EncodeKey(value)
				if err != nil {
					return err
				}
				_, duplicate = tree.Get(key)
			}
			if duplicate {
				return fmt.Errorf("duplicate entry %v for column %q", value, t.Columns[idx].Name)
			}
			seen[idx][value] = true
		}
	}
	return nil
}
```

`Update`:

```go
// Update applies assignments to every row matching where (a nil where
// matches every row, same as Delete), enforcing PRIMARY KEY/UNIQUE on any
// assigned column. NOT NULL is a non-issue here: the parser only accepts a
// STRING or NUMBER literal on the right of SET, so an assignment can never
// produce NULL. The table's file is scanned once to find the matching rows. A
// unique column is in conflict when a row that is not being updated already
// holds the assigned value: an indexed column looks the value up, and a column
// without an index notices it during the scan. Every new version is encoded
// and size-checked before anything is written, and the new versions are
// inserted before the old ones are tombstoned, so a failure partway through (a
// coercion error, a uniqueness violation, a row too large for a page) leaves
// the table untouched, and a crash between the two writes can duplicate a row
// but never lose one. An I/O error during the writes discards the indexes;
// otherwise every updated row is re-pointed at its new RowID.
func (t *SqlTable) Update(assignments []parser.Assignment, where parser.Expression) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	resolved := make([]resolvedAssignment, len(assignments))
	for i, a := range assignments {
		idx, ok := columnIndex[a.Column]
		if !ok {
			return fmt.Errorf("unknown column %q", a.Column)
		}
		coerced, err := coerceValue(t.Columns[idx].DataType, a.Value)
		if err != nil {
			return fmt.Errorf("column %q: %v", t.Columns[idx].Name, err)
		}
		resolved[i] = resolvedAssignment{index: idx, value: coerced}
	}

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	// The PRIMARY KEY/UNIQUE columns being assigned, in assignment order. A
	// SET column = literal is static, so every matched row gets the same value.
	uniqueColumns := make([]int, 0)
	assigned := make(map[int]any, len(resolved))
	for _, a := range resolved {
		assigned[a.index] = a.value
		col := t.Columns[a.index]
		if col.IsPrimaryKey() || col.IsUnique() {
			if err := checkKeyLength(col.Name, a.value); err != nil {
				return err
			}
			uniqueColumns = append(uniqueColumns, a.index)
		}
	}

	var trees map[int]*index.Tree
	if len(uniqueColumns) > 0 {
		var err error
		if trees, err = t.indexes(); err != nil {
			return err
		}
	}
	// If the indexes are built, remember each matched row's old key values so
	// they can be removed after the write.
	trackKeys := t.builtIndexes() != nil

	var oldIDs []storage.RowID
	var oldKeys [][]any
	var updated [][]any
	conflicts := make(map[int]bool, len(uniqueColumns))
	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return err
		}
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return err
		}
		if !matched {
			// A column with an index is checked after the scan; one without
			// is checked here. A matched row's own old value is deliberately
			// excluded, so re-assigning a column back to its current value
			// never self-conflicts.
			for _, idx := range uniqueColumns {
				if trees[idx] == nil && row[idx] != nil && row[idx] == assigned[idx] {
					conflicts[idx] = true
				}
			}
			continue
		}
		oldIDs = append(oldIDs, fileRow.ID)
		if trackKeys {
			oldKeys = append(oldKeys, t.keyValues(row))
		}
		for _, a := range resolved {
			row[a.index] = a.value
		}
		updated = append(updated, row)
	}

	if len(updated) > 0 {
		for _, idx := range uniqueColumns {
			if tree := trees[idx]; tree != nil && assigned[idx] != nil {
				key, err := index.EncodeKey(assigned[idx])
				if err != nil {
					return err
				}
				if holder, found := tree.Get(key); found && !slices.Contains(oldIDs, holder) {
					conflicts[idx] = true
				}
			}
			if conflicts[idx] {
				return fmt.Errorf("duplicate entry %v for column %q", assigned[idx], t.Columns[idx].Name)
			}
		}
		// Two matched rows would both receive the assigned value; the old
		// row-by-row check hit that on the second row, on the first unique
		// column in assignment order.
		if len(updated) > 1 && len(uniqueColumns) > 0 {
			idx := uniqueColumns[0]
			return fmt.Errorf("duplicate entry %v for column %q", assigned[idx], t.Columns[idx].Name)
		}
	}

	encoded, err := encodeRows(t.Columns, updated)
	if err != nil {
		return err
	}
	newIDs := make([]storage.RowID, 0, len(encoded))
	for _, b := range encoded {
		id, err := t.file.Insert(b)
		if err != nil {
			t.dropIndexes()
			return err
		}
		newIDs = append(newIDs, id)
	}
	for _, id := range oldIDs {
		if err := t.file.Delete(id); err != nil {
			t.dropIndexes()
			return err
		}
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexMoved(oldKeys, updated, newIDs)
	return nil
}
```

In `addColumn`, replace

```go
	// One value backfilled onto two or more rows would hold the same key twice.
	if (column.IsPrimaryKey() || column.IsUnique()) && filler != nil {
```

with:

```go
	if column.IsPrimaryKey() || column.IsUnique() {
		if err := checkKeyLength(column.Name, filler); err != nil {
			return err
		}
	}

	// One value backfilled onto two or more rows would hold the same key twice.
	if (column.IsPrimaryKey() || column.IsUnique()) && filler != nil {
```

- [ ] **Step 4: Run the whole engine suite**

Run: `gofmt -w internal/ && go vet ./internal/... && go test -race -count=1 ./internal/engine/`
Expected: PASS: the new tests and every existing uniqueness test (their error text and order are the regression net for this change).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/table.go internal/engine/wiring_test.go
git commit -m "feat(engine): check PRIMARY KEY and UNIQUE through the indexes" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Differential test, mutation checks, documentation and the full suite

**Files:**
- Modify (temporarily, then reverted): `internal/engine/table.go`, `internal/engine/tableindex.go`
- Modify: `CLAUDE.md`
- Test: `internal/engine/indexdiff_test.go` (new)

**Interfaces:**
- Consumes: everything above; `SqlTable.noIndexes`.
- Produces: `TestIndexedTableMatchesAnUnindexedOne`, which runs a long seeded mix of INSERT, UPDATE, DELETE and SELECT on two tables, one with `noIndexes`, and requires identical results, errors and files at every step; the `CLAUDE.md` update.

- [ ] **Step 1: Write the differential test**

Create `internal/engine/indexdiff_test.go`:

```go
package engine

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func randEmail(rng *rand.Rand) string { return fmt.Sprintf("u%d@x.com", rng.Intn(25)) }
func randName(rng *rand.Rand) string  { return fmt.Sprintf("n%d", rng.Intn(4)) }

// randomWhere draws a WHERE from a mix of shapes: none, equalities on the key
// columns (either operand order), ranges, AND/OR, and one that cannot be
// coerced. The expression is built once so both tables see the same one.
func randomWhere(rng *rand.Rand) parser.Expression {
	id := func() parser.Token { return numTok(fmt.Sprint(rng.Intn(25))) }
	switch rng.Intn(9) {
	case 0:
		return nil
	case 1:
		return cmp(identTok("id"), parser.EQ, id())
	case 2:
		return cmp(identTok("id"), parser.GTE, id())
	case 3:
		return cmp(identTok("email"), parser.EQ, strTok(randEmail(rng)))
	case 4:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.AND, cmp(identTok("name"), parser.EQ, strTok(randName(rng))))
	case 5:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.OR, cmp(identTok("id"), parser.EQ, id()))
	case 6:
		return cmp(id(), parser.EQ, identTok("id"))
	case 7:
		return cmp(identTok("name"), parser.EQ, strTok(randName(rng)))
	default:
		return cmp(identTok("id"), parser.EQ, strTok("oops"))
	}
}

// randomStatement draws one statement and returns it as a function to run on a
// table, so the very same statement can run on two tables.
func randomStatement(rng *rand.Rand) func(*SqlTable) (Response, error) {
	switch rng.Intn(10) {
	case 0, 1, 2, 3: // INSERT
		var columns []string
		rows := make([][]parser.Token, 1+rng.Intn(3))
		withEmail := rng.Intn(5) > 0
		for i := range rows {
			if withEmail {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(25))), strTok(randEmail(rng)), strTok(randName(rng)))
			} else {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(25))), strTok(randName(rng)))
			}
		}
		if !withEmail {
			columns = []string{"id", "name"}
		}
		return func(table *SqlTable) (Response, error) { return Response{}, table.InsertValues(columns, rows) }
	case 4, 5: // UPDATE
		var assignment parser.Assignment
		switch rng.Intn(4) {
		case 0:
			assignment = parser.Assignment{Column: "email", Value: strTok(randEmail(rng))}
		case 1:
			assignment = parser.Assignment{Column: "id", Value: numTok(fmt.Sprint(rng.Intn(25)))}
		default:
			assignment = parser.Assignment{Column: "name", Value: strTok(randName(rng))}
		}
		where := randomWhere(rng)
		return func(table *SqlTable) (Response, error) {
			return Response{}, table.Update([]parser.Assignment{assignment}, where)
		}
	case 6, 7: // DELETE
		where := randomWhere(rng)
		return func(table *SqlTable) (Response, error) { return Response{}, table.Delete(where) }
	default: // SELECT
		where := randomWhere(rng)
		return func(table *SqlTable) (Response, error) { return table.Select([]string{"*"}, where) }
	}
}

// With and without indexes, the same statements must give the same results,
// the same errors and the same file, whatever the mix.
func TestIndexedTableMatchesAnUnindexedOne(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			indexed, plain := keyedTable(t), keyedTable(t)
			plain.noIndexes = true

			for op := 0; op < 400; op++ {
				statement := randomStatement(rng)

				gotResp, gotErr := statement(indexed)
				wantResp, wantErr := statement(plain)

				require.Equal(t, errText(wantErr), errText(gotErr), "op %d", op)
				require.Equal(t, wantResp, gotResp, "op %d", op)
				require.Equal(t, fileRows(t, plain), fileRows(t, indexed), "op %d", op)
				if indexed.builtIndexes() != nil {
					assertIndexesMatchHeap(t, indexed)
				}
			}
			assert.NotNil(t, indexed.builtIndexes(), "the run must have used the indexes")
		})
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -race -count=1 ./internal/engine/ -run IndexedTableMatches -v 2>&1 | grep -E '^(\s*--- |FAIL|ok|PASS)'`
Expected: PASS for the three seeds. (It passes on the first run because the implementation is already complete; Step 3 is what proves the test can fail.) If it fails, the message names the operation number: fix the code, not the test, and ledger a ruling.

- [ ] **Step 3: Mutation checks**

For each row, make the one edit, run `go test -count=1 ./internal/engine/` (about 15 seconds), confirm a test fails (not a build failure), then restore with `git checkout internal/engine/`.

| # | Edit | A test that must fail |
| --- | --- | --- |
| 1 | `appendToFile`: delete the line `t.indexInserted(ids, rows)` | `TestIndexedTableMatchesAnUnindexedOne` |
| 2 | `Update`: delete the line `t.indexMoved(oldKeys, updated, newIDs)` | `TestIndexedTableMatchesAnUnindexedOne` |
| 3 | `closeLocked`: delete the line `t.dropIndexes()` | `TestTableIndexes_CloseAndDropDiscardThem` |
| 4 | `indexes`: change `t.idx.trees != nil && t.idx.fileID == t.fileID` to `t.idx.trees != nil` | `TestTableIndexes_RebuiltWhenTheFileChanges` |
| 5 | `appendToFile`: delete the `t.dropIndexes()` inside the `Sync` error branch | `TestTableIndexes_AWriteThatFailsDiscardsThem...` (sync fails after an insert) |
| 6 | `deleteFromFile`: change `trackKeys := t.builtIndexes() != nil` to `trackKeys := false` | `TestIndexedTableMatchesAnUnindexedOne` |
| 7 | `checkKeyLength`: add `&& false` to the `len(s) > maxKeyLen` condition | `TestLongKeys_AreRejectedAtTheLimit` |
| 8 | `checkUniqueness`: replace `_, duplicate = tree.Get(key)` with `_ = tree; _ = key` | `TestUniqueness_UsesTheIndexInsteadOfScanning` or the differential test |
| 9 | `Update`: change `found && !slices.Contains(oldIDs, holder)` to `found && holder == holder` | `TestUniqueness_UpdateConflictsAreFoundThroughTheIndex` or the differential test |
| 10 | `indexMoved`: remove the old keys after inserting the new ones instead of before | `TestIndexedTableMatchesAnUnindexedOne` |
| 11 | `Update`: delete the `t.dropIndexes()` inside the first (`Insert`) error branch | `TestTableIndexes_AWriteThatFailsDiscardsThem...` (the update insert fails case) |
| 12 | `buildIndexes`: change the error test so every `Insert` error aborts the build (`if true {`, keeping the imports used) | `TestTableIndexes_ADuplicateKeyDegradesOnlyThatColumn` |
| 13 | `checkUniqueness`: never add a nil-tree column to `scanned` | `TestUniqueness_ADegradedColumnStillChecksByScanning` or the differential test |
| 14 | `indexes`: ignore `noIndexes` | `TestIndexedTableMatchesAnUnindexedOne` or `TestTableIndexes_DisabledMeansNoTrees` |

Expected: all fourteen fail. A mutant that survives means a missing test: add one that fails for it, then continue.

- [ ] **Step 4: Update `CLAUDE.md`**

In the Storage paragraph, replace the sentence

> PRIMARY KEY/UNIQUE checks stream the file against a small set of the statement's own key values, so memory is proportional to the statement and time to the table until an index exists.

with:

> PRIMARY KEY/UNIQUE checks use an in-memory B+ tree per key column (`internal/index`), built from one scan of the file the first time a statement needs it and discarded whenever the file might disagree with it (an I/O error mid-write, `ADD`/`DROP COLUMN`, close), so a check costs a lookup instead of a scan. The heap file stays the only copy of the rows; an index holds just each key and its `RowID`, so a table that has been used keeps its key values in memory. A column whose values the tree cannot hold (a key over the tree's limit written before the limit existed, or two rows with one key left by an interrupted UPDATE) keeps the old scan-based check. A PRIMARY KEY/UNIQUE string longer than 4084 bytes is rejected with `key too long`.

- [ ] **Step 5: Run everything**

Run: `git status --short` (expected: only `CLAUDE.md` and the new test), then `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: every package passes and `gofmt` and `go vet` print nothing.

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md internal/engine/indexdiff_test.go
git commit -m "test(engine): compare indexed and unindexed tables statement by statement" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
