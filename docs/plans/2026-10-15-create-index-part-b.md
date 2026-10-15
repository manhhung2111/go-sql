# CREATE INDEX part B (build, maintain and use the secondary trees) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A column with a `CREATE INDEX` definition gets an in-memory tree, built with the others, kept in step on every write, and used by `SELECT`, `UPDATE` and `DELETE` for `=` and range `WHERE`s, under the page-count rule.

**Architecture:** `tableIndexes` holds a `treeSet{unique, secondary}`; the secondary tree for an index is keyed by `index.EncodeSecondaryKey(value, rowID)`, so duplicates are legal and one value's rows sit together in file order. Maintenance extends the existing `indexInserted/Deleted/Moved` (a delete or move needs the old `RowID`, so those take ids). The planner turns an `=` or ordering comparison on an indexed non-key column into a `rangePlan` over the secondary tree, which goes through the existing `rangeRows` and its page rule.

**Tech Stack:** Go 1.26, testify, `internal/index`, `internal/engine`.

**Spec:** `docs/specs/2026-10-15-create-index-design.md` ("Long values", "Index state and maintenance", "Planner"). This plan builds on part A (PR #43: syntax, definitions in `sys_indexes`, `SqlTable.indexDefs`, `index.EncodeSecondaryKey`/`SecondaryEqualRange`) and on the page-count rule (PR #41); it is written against the branch `feat/engine-create-index-a`.

## Global Constraints

- A secondary entry's key is `EncodeSecondaryKey(value, rowID)`; NULLs are not indexed; a degraded secondary tree is `nil` and its column scans.
- An entry must not exceed the tree's `MaxKeyLen`: `INSERT` and `UPDATE` reject a value whose secondary key would be too long with `key too long for index %q: %d bytes exceeds the maximum of %d` (the size is `index.SecondaryKeyLen`, the encoded value part plus 8), writing nothing. A value already in the file degrades only that index at build time.
- `ADD COLUMN` needs no such check: a new column cannot have an index yet.
- Writes update the trees only after the heap write and `Sync` succeed; any write error drops all trees (existing rule).
- A lookup is used only for a `WHERE` that cannot fail at run time, re-evaluates the whole `WHERE` on each fetched row, returns rows in file order, and follows the page-count rule (`rangeCostFactor`, default 2). Precedence: a PRIMARY KEY/UNIQUE equality, then a range on a unique tree, then a range or equality on a secondary tree. A column that is itself a PRIMARY KEY/UNIQUE column uses its unique tree, never a secondary one.
- Lock order is unchanged. Engine non-test code must not import stdlib `cmp`. Commits: Conventional Commits ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. `gofmt -w` touched files; `go test -race ./...` before the last commit of each task.

## Review Focus

- An UPDATE that leaves an indexed value unchanged still moves the row to a new `RowID`: its secondary entry must be re-pointed, or a later lookup returns a tombstone.
- A secondary entry must never be left behind by DELETE, UPDATE or a failed write; `assertIndexesMatchHeap` must compare secondary trees too.
- `col > v` and `col <= v` on a secondary index must not include or exclude a neighbouring value's rows (off-by-one in the bounds).
- An equality on an indexed column whose matches are spread over most pages must scan.
- `CREATE INDEX` and `DROP INDEX` must discard any built trees so the next statement sees the new set.
- A row whose indexed value is NULL must be found by no secondary lookup, matching the scan (a comparison with NULL is never true).

## File Structure

- `internal/index/secondarykey.go`: `SecondaryKeyLen`.
- `internal/engine/tableindex.go`: `treeSet`, build, accessors, maintenance, `maxKeyLen` use; `indexdef.go`: `secondaryColumns`, `secondaryFor`, `checkSecondaryLengths`.
- `internal/engine/table.go`: `InsertValues`, `deleteFromFile`, `Update` call sites.
- `internal/engine/lookup.go`: `rangePlan.secondary`, planning, `lookupRows`.
- Tests: `internal/engine/secondaryindex_test.go` (new), `tableindex_test.go` (helper), `internal/index/secondarykey_test.go`.

---

### Task 1: Build, maintain and bound the secondary trees

**Files:**
- Modify: `internal/index/secondarykey.go`, `internal/engine/tableindex.go`, `internal/engine/indexdef.go`, `internal/engine/table.go` (the `InsertValues` check, `deleteFromFile`, `Update`), `internal/engine/lookup.go` (the one `trees, err := t.indexes()` call stays as is in this task)
- Test: `internal/index/secondarykey_test.go`, `internal/engine/secondaryindex_test.go` (create), `internal/engine/tableindex_test.go` (`assertIndexesMatchHeap`)

**Interfaces:**
- Consumes: part A's `indexDef{name, column}`, `SqlTable.indexDefs`, `index.EncodeSecondaryKey`, `index.SecondaryEqualRange`.
- Produces: `index.SecondaryKeyLen(v any) (int, error)`; `type treeSet struct{ unique map[int]*index.Tree; secondary map[string]*index.Tree }`; `(*SqlTable).indexSet() (*treeSet, error)`; `(*SqlTable).builtSet() *treeSet`; `buildIndexes() (*treeSet, error)`; `(*SqlTable).secondaryColumns() []int` (schema index per definition, -1 if missing); `(*SqlTable).trackedColumns() []int`; `indexDeleted(keys [][]any, ids []storage.RowID)`; `indexMoved(oldKeys [][]any, oldIDs []storage.RowID, rows [][]any, newIDs []storage.RowID)`; `(*SqlTable).checkSecondaryLengths(rows [][]any) error`.

- [ ] **Step 1: Failing tests for the key length helper**

Append to `internal/index/secondarykey_test.go`:

```go
func TestSecondaryKeyLen(t *testing.T) {
	tests := []struct {
		v    any
		want int
	}{
		{int64(7), 16},
		{true, 9},
		{"", 10},
		{"a", 11},
		{"a\x00", 13}, // the zero byte is escaped to two
	}
	for _, tc := range tests {
		got, err := SecondaryKeyLen(tc.v)

		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
		key, err := EncodeSecondaryKey(tc.v, storage.RowID{})
		require.NoError(t, err)
		assert.Equal(t, len(key), got, "it is the length of the key")
	}
	_, err := SecondaryKeyLen(nil)
	assert.Error(t, err)
}
```

Add a stub to `internal/index/secondarykey.go` (`func SecondaryKeyLen(v any) (int, error) { return 0, nil }`), run `go test ./internal/index/ -run SecondaryKeyLen 2>&1 | tail`. Expected: FAIL on assertions.

- [ ] **Step 2: Implement the helper**

Replace the stub in `internal/index/secondarykey.go`:

```go
// SecondaryKeyLen returns the length of the secondary key for v: the value part
// plus the row id. A string with zero bytes is longer than its Go length.
func SecondaryKeyLen(v any) (int, error) {
	part, err := valuePart(v)
	if err != nil {
		return 0, err
	}
	return len(part) + secondaryRowIDSize, nil
}
```

Run: `go test -race ./internal/index/` — Expected: PASS.

- [ ] **Step 3: Failing engine tests**

Create `internal/engine/secondaryindex_test.go`:

```go
package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

const colName = 2

// namedIndex is a keyedTable (id PRIMARY KEY, email UNIQUE, name) with a
// CREATE INDEX on name.
func namedIndex(t *testing.T) *SqlTable {
	t.Helper()
	table := keyedTable(t)
	require.NoError(t, table.CreateIndex("by_name", "name", false))
	return table
}

func insertNamed(t *testing.T, table *SqlTable, id int, name string) {
	t.Helper()
	insertUser(t, table, id, fmt.Sprintf("u%d@x.com", id), name)
}

// idsOfName returns the row ids a secondary tree holds for value, in tree order.
func idsOfName(t *testing.T, tree *index.Tree, value any) []storage.RowID {
	t.Helper()
	lo, hi, err := index.SecondaryEqualRange(value)
	require.NoError(t, err)
	var ids []storage.RowID
	for _, id := range tree.Range(lo, hi) {
		ids = append(ids, id)
	}
	return ids
}

func TestSecondaryIndex_IsBuiltWithTheOthersAndHoldsDuplicates(t *testing.T) {
	table := namedIndex(t)
	a := writeRaw(t, table, []any{int64(1), "a@x.com", "n1"})
	writeRaw(t, table, []any{int64(2), "b@x.com", nil}) // NULL is never indexed
	b := writeRaw(t, table, []any{int64(3), "c@x.com", "n1"})
	c := writeRaw(t, table, []any{int64(4), "d@x.com", "n2"})

	set, err := table.indexSet()

	require.NoError(t, err)
	require.Len(t, set.unique, 2)
	require.Contains(t, set.secondary, "by_name")
	tree := set.secondary["by_name"]
	require.NotNil(t, tree)
	assert.Equal(t, 3, tree.Len(), "one entry per non-NULL value")
	assert.Equal(t, []storage.RowID{a, b}, idsOfName(t, tree, "n1"), "duplicates, in file order")
	assert.Equal(t, []storage.RowID{c}, idsOfName(t, tree, "n2"))
	assert.Empty(t, idsOfName(t, tree, "n3"))
}

func TestSecondaryIndex_WithNoUniqueColumnsStillBuilds(t *testing.T) {
	table, err := newTempTable(t, "events", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "kind", DataType: parser.VarCharDataType{Size: 20}},
	})
	require.NoError(t, err)
	require.NoError(t, table.CreateIndex("by_kind", "kind", false))
	writeRaw(t, table, []any{int64(1), "x"})
	writeRaw(t, table, []any{int64(2), "x"})

	set, err := table.indexSet()

	require.NoError(t, err)
	assert.Empty(t, set.unique)
	assert.Equal(t, 2, set.secondary["by_kind"].Len())
}

func TestSecondaryIndex_StaysInStepWithEveryWrite(t *testing.T) {
	table := namedIndex(t)
	for i := 1; i <= 6; i++ {
		insertNamed(t, table, i, fmt.Sprintf("n%d", i%3))
	}
	_, err := table.indexSet()
	require.NoError(t, err)

	steps := []struct {
		name string
		run  func() error
	}{
		{"insert", func() error { return table.InsertValues(nil, [][]parser.Token{tokRow(numTok("7"), strTok("u7@x.com"), strTok("n1"))}) }},
		{"insert with NULL name", func() error {
			return table.InsertValues([]string{"id", "email"}, [][]parser.Token{tokRow(numTok("8"), strTok("u8@x.com"))})
		}},
		{"update the indexed value", func() error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("moved")}}, eqCmp("id", numTok("2")))
		}},
		{"update another column: the row still moves", func() error {
			return table.Update([]parser.Assignment{{Column: "email", Value: strTok("new@x.com")}}, eqCmp("id", numTok("3")))
		}},
		{"update several rows", func() error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("all")}}, cmp(identTok("id"), parser.GTE, numTok("5")))
		}},
		{"delete one", func() error { return table.Delete(eqCmp("id", numTok("1"))) }},
		{"delete by the indexed value", func() error { return table.Delete(eqCmp("name", strTok("all"))) }},
		{"delete everything", func() error { return table.Delete(nil) }},
	}
	for _, step := range steps {
		require.NoError(t, step.run(), step.name)
		assertIndexesMatchHeap(t, table)
	}
}

func TestSecondaryIndex_AFailedWriteDiscardsItAndItRebuilds(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*failingFile)
		run  func(*SqlTable) error
	}{
		{"an insert", func(f *failingFile) { f.failInserts = true }, func(tb *SqlTable) error {
			return tb.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("z@x.com"), strTok("n1"))})
		}},
		{"a sync", func(f *failingFile) { f.failSyncs = true }, func(tb *SqlTable) error {
			return tb.Update([]parser.Assignment{{Column: "name", Value: strTok("q")}}, eqCmp("id", numTok("1")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := namedIndex(t)
			insertNamed(t, table, 1, "n1")
			_, err := table.indexSet()
			require.NoError(t, err)
			ff := &failingFile{File: table.file}
			table.file = ff
			tc.fail(ff)

			assert.Error(t, tc.run(table))

			assert.Nil(t, table.builtSet(), "the trees were discarded")
			ff.failInserts, ff.failSyncs = false, false
			_, err = table.indexSet()
			require.NoError(t, err)
			assertIndexesMatchHeap(t, table)
		})
	}
}

func TestSecondaryIndex_CreateAndDropIndexDiscardTheTrees(t *testing.T) {
	table := keyedTable(t)
	insertNamed(t, table, 1, "n1")
	_, err := table.indexSet()
	require.NoError(t, err)

	require.NoError(t, table.CreateIndex("by_name", "name", false))
	assert.Nil(t, table.builtSet(), "a new index needs a build")
	set, err := table.indexSet()
	require.NoError(t, err)
	assert.Contains(t, set.secondary, "by_name")

	require.NoError(t, table.DropIndex("by_name", false))
	assert.Nil(t, table.builtSet(), "a dropped index must not linger")
	set, err = table.indexSet()
	require.NoError(t, err)
	assert.NotContains(t, set.secondary, "by_name")
}

func TestSecondaryIndex_FollowsSchemaChanges(t *testing.T) {
	table := namedIndex(t)
	insertNamed(t, table, 1, "n1")
	insertNamed(t, table, 2, "n1")
	_, err := table.indexSet()
	require.NoError(t, err)

	t.Run("renaming the indexed column keeps the trees", func(t *testing.T) {
		require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "label"}))

		assert.NotNil(t, table.builtSet())
		assertIndexesMatchHeap(t, table)
	})
	t.Run("adding and dropping another column rebuilds with the right column", func(t *testing.T) {
		require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "extra", DataType: parser.IntDataType{}}}))
		set, err := table.indexSet()
		require.NoError(t, err)
		assert.Equal(t, 2, set.secondary["by_name"].Len())
		require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "email"}))
		assertIndexesMatchHeap(t, table)
	})
}

// notes is a table with a TEXT column, which (unlike the VARCHAR(50) name of
// keyedTable) can hold a value longer than an index entry may be, with a
// CREATE INDEX on it.
func notesTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "notes", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "note", DataType: parser.TextDataType{}},
	})
	require.NoError(t, err)
	require.NoError(t, table.CreateIndex("by_note", "note", false))
	return table
}

func TestSecondaryIndex_LongValues(t *testing.T) {
	long := strings.Repeat("x", maxKeyLen) // the value part alone leaves no room for the row id

	t.Run("INSERT and UPDATE reject them and write nothing", func(t *testing.T) {
		table := notesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("short"))}))

		err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok(long))})
		assert.ErrorContains(t, err, `key too long for index "by_note"`)
		err = table.Update([]parser.Assignment{{Column: "note", Value: strTok(long)}}, eqCmp("id", numTok("1")))
		assert.ErrorContains(t, err, `key too long for index "by_note"`)

		assert.Len(t, fileRows(t, table), 1)
		resp, err := table.Select([]string{"note"}, nil)
		require.NoError(t, err)
		assert.Equal(t, [][]string{{"short"}}, resp.Rows)
	})
	t.Run("a value that just fits is accepted", func(t *testing.T) {
		table := notesTable(t)
		fits := strings.Repeat("x", maxKeyLen-10) // value part n+2, plus 8 for the row id: exactly maxKeyLen
		over := strings.Repeat("x", maxKeyLen-9)  // one byte too many

		assert.ErrorContains(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok(over))}), "key too long")
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok(fits))}))

		set, err := table.indexSet()
		require.NoError(t, err)
		assert.Equal(t, 1, set.secondary["by_note"].Len())
	})
	t.Run("an UPDATE that matches nothing is not checked", func(t *testing.T) {
		table := notesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("short"))}))

		err := table.Update([]parser.Assignment{{Column: "note", Value: strTok(long)}}, eqCmp("id", numTok("99")))

		assert.NoError(t, err)
	})
	t.Run("a value already in the file degrades only that index", func(t *testing.T) {
		table := notesTable(t)
		writeRaw(t, table, []any{int64(1), long})
		writeRaw(t, table, []any{int64(2), "short"})

		set, err := table.indexSet()

		require.NoError(t, err)
		assert.Nil(t, set.secondary["by_note"], "the index cannot hold the value")
		assert.NotNil(t, set.unique[0], "the unique tree is unaffected")
		assertIndexesMatchHeap(t, table)
	})
}

(`eqCmp`, `numTok`, `insertUser`, `writeRaw`, `fileRows`, `maxKeyLen` already exist in the package. A string of n bytes has a secondary key of n+10 bytes, so `maxKeyLen-10` fits exactly and `maxKeyLen-9` does not.)

- [ ] **Step 4: Update `assertIndexesMatchHeap` and add compile-only stubs, then watch the tests fail**

In `internal/engine/tableindex_test.go`, replace the body of `assertIndexesMatchHeap` with:

```go
func assertIndexesMatchHeap(t *testing.T, table *SqlTable) {
	t.Helper()
	got := table.builtSet()
	require.NotNil(t, got, "the indexes must be built")
	want, err := table.buildIndexes()
	require.NoError(t, err)
	require.Len(t, got.unique, len(want.unique))
	for col, wantTree := range want.unique {
		gotTree, ok := got.unique[col]
		require.True(t, ok, "column %d has no entry", col)
		if wantTree == nil {
			assert.Nil(t, gotTree, "column %d cannot be indexed", col)
			continue
		}
		require.NotNil(t, gotTree, "column %d lost its tree", col)
		assert.Equal(t, entries(wantTree), entries(gotTree), "column %d", col)
	}
	require.Len(t, got.secondary, len(want.secondary))
	for name, wantTree := range want.secondary {
		gotTree, ok := got.secondary[name]
		require.True(t, ok, "index %q has no entry", name)
		if wantTree == nil {
			assert.Nil(t, gotTree, "index %q cannot hold the table's values", name)
			continue
		}
		require.NotNil(t, gotTree, "index %q lost its tree", name)
		assert.Equal(t, entries(wantTree), entries(gotTree), "index %q", name)
	}
}
```

Make the package compile with stubs in `internal/engine/tableindex.go` (the other tests that call `assertIndexesMatchHeap` fail until Step 5, which is expected): `type treeSet struct{ unique map[int]*index.Tree; secondary map[string]*index.Tree }`, `func (t *SqlTable) indexSet() (*treeSet, error) { return &treeSet{}, nil }`, `func (t *SqlTable) builtSet() *treeSet { return nil }`, and change `buildIndexes` to return `(*treeSet, error)` by wrapping: rename the current function to `buildUnique() (map[int]*index.Tree, error)`, and `buildIndexes() (*treeSet, error) { u, err := t.buildUnique(); return &treeSet{unique: u, secondary: map[string]*index.Tree{}}, err }`. Add `func (t *SqlTable) checkSecondaryLengths(rows [][]any) error { return nil }` and the variable `maxKeyLen` is already defined.

Run: `go test ./internal/engine/ -run 'TestSecondaryIndex_' 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)'`
Expected: FAIL on assertions or nil-set panics, not build errors.

- [ ] **Step 5: Implement the tree set, build and accessors**

In `internal/engine/tableindex.go` (replacing the stubs of Step 4):

```go
// treeSet is the trees of one table: unique maps a PRIMARY KEY/UNIQUE column's
// schema index to its tree, secondary maps a CREATE INDEX name to its tree. A
// nil tree means the column or index cannot hold the file's values (see
// buildIndexes) and statements on it scan.
type treeSet struct {
	unique    map[int]*index.Tree
	secondary map[string]*index.Tree
}
```

Change `tableIndexes.trees map[int]*index.Tree` to `set *treeSet` (nil means not built; update its comment). Rewrite:

```go
// indexSet returns the table's trees, building them from one scan of the file
// if they are missing or were built from another file. Callers hold t.mu, for
// reading or writing. With noIndexes set it returns no trees, so every column
// takes its scan path.
func (t *SqlTable) indexSet() (*treeSet, error) {
	if t.noIndexes {
		return &treeSet{unique: map[int]*index.Tree{}, secondary: map[string]*index.Tree{}}, nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.set != nil && t.idx.fileID == t.fileID {
		return t.idx.set, nil
	}
	if t.file == nil {
		return nil, fmt.Errorf("table %q is closed", t.Name)
	}
	set, err := t.buildIndexes()
	if err != nil {
		return nil, err
	}
	t.idx.set, t.idx.fileID = set, t.fileID
	return set, nil
}

// indexes returns just the PRIMARY KEY/UNIQUE trees.
func (t *SqlTable) indexes() (map[int]*index.Tree, error) {
	set, err := t.indexSet()
	if err != nil {
		return nil, err
	}
	return set.unique, nil
}

// builtSet returns the trees if they are built for the current file, and nil
// otherwise. It never builds.
func (t *SqlTable) builtSet() *treeSet {
	if t.noIndexes {
		return nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.set != nil && t.idx.fileID == t.fileID {
		return t.idx.set
	}
	return nil
}

// builtIndexes returns the PRIMARY KEY/UNIQUE trees if built, and nil otherwise.
func (t *SqlTable) builtIndexes() map[int]*index.Tree {
	if set := t.builtSet(); set != nil {
		return set.unique
	}
	return nil
}
```

`dropIndexes` sets `t.idx.set = nil`. Replace `buildIndexes`:

```go
// secondaryColumns returns the schema index of each CREATE INDEX definition's
// column, in the order of t.indexDefs; -1 for a column that is not there.
func (t *SqlTable) secondaryColumns() []int {
	cols := make([]int, len(t.indexDefs))
	for i, def := range t.indexDefs {
		cols[i] = slices.IndexFunc(t.Columns, func(c parser.ColumnDefinition) bool { return c.Name == def.column })
	}
	return cols
}

// trackedColumns are the columns whose values a write must remember to update
// the trees: the PRIMARY KEY/UNIQUE columns, then the indexed columns.
func (t *SqlTable) trackedColumns() []int {
	return append(t.keyColumns(), t.secondaryColumns()...)
}

// buildIndexes scans the file once and indexes every non-NULL PRIMARY
// KEY/UNIQUE value and every non-NULL value of an indexed column. A unique
// column whose values the tree cannot hold gets a nil tree: a key seen twice (an
// interrupted UPDATE can leave two rows with one key) or longer than maxKeyLen.
// A secondary index gets a nil tree only for a value too long to index.
// Duplicates are legal there. Any other error aborts the build.
func (t *SqlTable) buildIndexes() (*treeSet, error) {
	set := &treeSet{unique: map[int]*index.Tree{}, secondary: map[string]*index.Tree{}}
	for _, col := range t.keyColumns() {
		tree, err := index.New(index.DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		set.unique[col] = tree
	}
	secondaryCols := t.secondaryColumns()
	for i, def := range t.indexDefs {
		if secondaryCols[i] < 0 {
			set.secondary[def.name] = nil
			continue
		}
		tree, err := index.New(index.DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		set.secondary[def.name] = tree
	}
	if len(set.unique) == 0 && len(set.secondary) == 0 {
		return set, nil
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return nil, err
		}
		for col, tree := range set.unique {
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
				set.unique[col] = nil
			}
		}
		for i, def := range t.indexDefs {
			tree, col := set.secondary[def.name], secondaryCols[i]
			if tree == nil || col < 0 || row[col] == nil {
				continue
			}
			key, err := index.EncodeSecondaryKey(row[col], fileRow.ID)
			if err != nil {
				return nil, err
			}
			if err := tree.Insert(key, fileRow.ID); err != nil {
				if !errors.Is(err, index.ErrDuplicateKey) && !errors.Is(err, index.ErrKeyTooLong) {
					return nil, err
				}
				set.secondary[def.name] = nil
			}
		}
	}
	return set, nil
}
```

(add the `slices` and `parser` imports; delete the `buildUnique` wrapper from Step 4.) Run the build-only tests: `go test ./internal/engine/ -run 'IsBuiltWithTheOthers|WithNoUniqueColumns|CreateAndDropIndexDiscard|LongValues/a_value_already' 2>&1 | grep -E '^(--- FAIL|ok|FAIL)'`. Expected: those pass; the maintenance and write-rejection tests still fail.

- [ ] **Step 6: Implement maintenance**

In `tableindex.go`, rewrite the three maintenance functions and `removeKeys` (they now read `builtSet()`):

```go
// indexInserted records rows just written to the file at ids. A built index
// that cannot take an entry is discarded, never left half updated.
func (t *SqlTable) indexInserted(ids []storage.RowID, rows [][]any) {
	set := t.builtSet()
	if set == nil {
		return
	}
	if !t.addEntries(set, rows, ids) {
		t.dropIndexes()
	}
}

// addEntries adds each row's entries to every tree and reports whether they all
// went in.
func (t *SqlTable) addEntries(set *treeSet, rows [][]any, ids []storage.RowID) bool {
	for col, tree := range set.unique {
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
				return false
			}
		}
	}
	for i, col := range t.secondaryColumns() {
		tree := set.secondary[t.indexDefs[i].name]
		if tree == nil || col < 0 {
			continue
		}
		for r, row := range rows {
			if row[col] == nil {
				continue
			}
			key, err := index.EncodeSecondaryKey(row[col], ids[r])
			if err == nil {
				err = tree.Insert(key, ids[r])
			}
			if err != nil {
				return false
			}
		}
	}
	return true
}

// indexDeleted removes rows just tombstoned. keys[i] holds row i's values at
// trackedColumns, ids[i] is its old RowID. An entry that is missing means the
// index was wrong, so it is discarded.
func (t *SqlTable) indexDeleted(keys [][]any, ids []storage.RowID) {
	set := t.builtSet()
	if set == nil {
		return
	}
	if !t.removeKeys(set, keys, ids) {
		t.dropIndexes()
	}
}

// indexMoved records an UPDATE: every updated row got a new RowID, so its old
// entries are removed first and the new values are added at the new ids.
// Unchanged values are re-pointed too, for a secondary entry ends in the RowID.
func (t *SqlTable) indexMoved(oldKeys [][]any, oldIDs []storage.RowID, rows [][]any, newIDs []storage.RowID) {
	set := t.builtSet()
	if set == nil {
		return
	}
	if !t.removeKeys(set, oldKeys, oldIDs) || !t.addEntries(set, rows, newIDs) {
		t.dropIndexes()
	}
}

// removeKeys deletes each row's entries from the trees and reports whether every
// entry was there.
func (t *SqlTable) removeKeys(set *treeSet, keys [][]any, ids []storage.RowID) bool {
	unique := t.keyColumns()
	for j, col := range unique {
		tree := set.unique[col]
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
	for i, col := range t.secondaryColumns() {
		tree := set.secondary[t.indexDefs[i].name]
		if tree == nil || col < 0 {
			continue
		}
		for r, values := range keys {
			value := values[len(unique)+i]
			if value == nil {
				continue
			}
			key, err := index.EncodeSecondaryKey(value, ids[r])
			if err != nil || !tree.Delete(key) {
				return false
			}
		}
	}
	return true
}
```

In `internal/engine/table.go`:
- `deleteFromFile`: `keyCols := t.keyColumns()` becomes `keyCols := t.trackedColumns()`; the last line `t.indexDeleted(keys)` becomes `t.indexDeleted(keys, matched)`.
- `Update`: `keyCols := t.keyColumns()` becomes `keyCols := t.trackedColumns()`; the last line becomes `t.indexMoved(oldKeys, oldIDs, updated, newIDs)`.

Add to `internal/engine/indexdef.go` (imports `fmt`, `index`):

```go
// checkSecondaryLengths rejects a value whose secondary key would not fit the
// tree: the value part and the row id must fit in maxKeyLen. It applies to
// indexed columns only.
func (t *SqlTable) checkSecondaryLengths(rows [][]any) error {
	for i, col := range t.secondaryColumns() {
		if col < 0 {
			continue
		}
		for _, row := range rows {
			if row[col] == nil {
				continue
			}
			if err := t.checkSecondaryValue(t.indexDefs[i].name, row[col]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *SqlTable) checkSecondaryValue(indexName string, value any) error {
	n, err := index.SecondaryKeyLen(value)
	if err != nil {
		return err
	}
	if n > maxKeyLen {
		return fmt.Errorf("key too long for index %q: %d bytes exceeds the maximum of %d", indexName, n, maxKeyLen)
	}
	return nil
}
```

In `InsertValues`, immediately after `if err := t.checkUniqueness(newRows, uniqueColumns); err != nil { return err }` add `if err := t.checkSecondaryLengths(newRows); err != nil { return err }`. In `Update`, inside `if len(updated) > 0 {` before the unique-length loop add:

```go
		for i, col := range t.secondaryColumns() {
			if assigned[col] != nil && col >= 0 {
				if err := t.checkSecondaryValue(t.indexDefs[i].name, assigned[col]); err != nil {
					return err
				}
			}
		}
```

Check how `assigned` is indexed in `Update` (it is indexed by schema column, as `assigned[idx]` is used above); if it only has entries for assigned columns, a missing column reads as nil, which is what this relies on. If `assigned` is a map, the same code works; if it is a slice sized to the columns, guard `col < len(assigned)`.

Run: `go test -race ./internal/engine/ 2>&1 | tail -15`
Expected: PASS, including every existing test (the helper change keeps the unique comparison).

- [ ] **Step 7: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean.

```bash
git add internal/index internal/engine
git commit -m "feat(engine): build and maintain the trees of CREATE INDEX definitions

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Use the secondary trees, compare with scans, and finish

**Files:**
- Modify: `internal/engine/lookup.go` (`rangePlan`, `planLookup`, `lookupRows`), `CLAUDE.md`, `docs/specs/2026-10-15-create-index-design.md`
- Test: `internal/engine/secondaryindex_test.go`

**Interfaces:**
- Consumes: Task 1's `treeSet`, `indexSet`, `secondaryColumns`; `rangeRows`, `collectRangeIDs`, `rangeCostFactor`, `wideKeys`/`idRange`-style helpers in `range_test.go`.
- Produces: `rangePlan.secondary string` (the index name; empty for a unique column's own tree); `(*SqlTable).secondaryFor(column int) (indexDef, bool)`.

- [ ] **Step 1: Failing planner tests**

Append to `internal/engine/secondaryindex_test.go`:

```go
func TestPlanLookup_SecondaryIndex(t *testing.T) {
	table := namedIndex(t)
	columnIndex := map[string]int{"id": 0, "email": 1, "name": 2}
	lo, hi, err := index.SecondaryEqualRange("n1")
	require.NoError(t, err)
	lo2, hi2, err := index.SecondaryEqualRange("n3")
	require.NoError(t, err)

	tests := []struct {
		name  string
		where parser.Expression
		want  []rangePlan
	}{
		{"an equality is the range of one value", eqCmp("name", strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo, hi: hi}}},
		{"reversed operands", cmp(strTok("n1"), parser.EQ, identTok("name")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo, hi: hi}}},
		{">= starts at the value", cmp(identTok("name"), parser.GTE, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo}}},
		{"> starts after the value's last entry", cmp(identTok("name"), parser.GT, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: hi}}},
		{"< ends before the value", cmp(identTok("name"), parser.LT, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", hi: lo}}},
		{"<= ends after the value's last entry", cmp(identTok("name"), parser.LTE, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", hi: hi}}},
		{"a pair of bounds is one range", andExpr(cmp(identTok("name"), parser.GTE, strTok("n1")), cmp(identTok("name"), parser.LT, strTok("n3"))),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo, hi: lo2}}},
		{"the tighter bound wins", andExpr(cmp(identTok("name"), parser.GT, strTok("n1")), cmp(identTok("name"), parser.LTE, strTok("n3"))),
			[]rangePlan{{column: colName, secondary: "by_name", lo: hi, hi: hi2}}},
		{"no plan for !=", cmp(identTok("name"), parser.NEQ, strTok("n1")), nil},
		{"no plan for OR", binaryExpr(eqCmp("name", strTok("n1")), parser.OR, eqCmp("name", strTok("n2"))), nil},
		{"no plan for a column without an index", eqCmp("id", numTok("1")), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plans := table.planLookup(tc.where, columnIndex)

			if tc.want == nil {
				// a key column still plans its own equality; only the secondary part is absent
				assert.Empty(t, plans.ranges)
				return
			}
			assert.Equal(t, tc.want, plans.ranges)
		})
	}
}

func TestPlanLookup_AKeyColumnNeverUsesASecondaryIndex(t *testing.T) {
	table := keyedTable(t)
	require.NoError(t, table.CreateIndex("by_id", "id", false))
	columnIndex := map[string]int{"id": 0, "email": 1, "name": 2}

	plans := table.planLookup(cmp(identTok("id"), parser.GTE, numTok("3")), columnIndex)

	require.Len(t, plans.ranges, 1)
	assert.Empty(t, plans.ranges[0].secondary, "the unique tree serves it")
	plans = table.planLookup(eqCmp("id", numTok("3")), columnIndex)
	assert.Len(t, plans.equalities, 1)
	assert.Empty(t, plans.ranges)
}
```

Add `secondary string` to `rangePlan` (between `column` and `lo`) so the file compiles; run `go test ./internal/engine/ -run 'TestPlanLookup_(SecondaryIndex|AKeyColumn)' 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL)'`. Expected: FAIL on assertions (no secondary ranges are planned).

- [ ] **Step 2: Implement planning**

In `internal/engine/lookup.go`, extend the `rangePlan` doc: "secondary names the CREATE INDEX whose tree the range is over; it is empty for a PRIMARY KEY/UNIQUE column's own tree." In `planLookup`, replace

```go
		if !t.isKeyColumn(column) {
			continue
		}
		switch op {
```

with

```go
		if !t.isKeyColumn(column) {
			t.planSecondary(&plans, column, op, value)
			continue
		}
		switch op {
```

and add:

```go
// planSecondary adds what "column op value" offers the first CREATE INDEX on
// column, if there is one. Every entry of a value ends in its row id, so an
// equality is the range that holds all of the value's entries, and the bounds
// of an ordering operator are the ends of that range.
func (t *SqlTable) planSecondary(plans *lookupPlans, column int, op parser.TokenType, value any) {
	def, ok := t.secondaryFor(column)
	if !ok {
		return
	}
	lo, hi, err := index.SecondaryEqualRange(value)
	if err != nil {
		return
	}
	switch op {
	case parser.EQ:
		plans.addSecondaryBounds(column, def.name, lo, hi)
	case parser.GTE:
		plans.addSecondaryBounds(column, def.name, lo, nil)
	case parser.GT:
		plans.addSecondaryBounds(column, def.name, hi, nil)
	case parser.LT:
		plans.addSecondaryBounds(column, def.name, nil, lo)
	case parser.LTE:
		plans.addSecondaryBounds(column, def.name, nil, hi)
	}
}

// addSecondaryBounds tightens the range over the named index with a lower and
// an upper bound; nil leaves that side alone.
func (p *lookupPlans) addSecondaryBounds(column int, name string, lower, upper []byte) {
	i := slices.IndexFunc(p.ranges, func(r rangePlan) bool { return r.column == column && r.secondary == name })
	if i < 0 {
		p.ranges = append(p.ranges, rangePlan{column: column, secondary: name})
		i = len(p.ranges) - 1
	}
	r := &p.ranges[i]
	if lower != nil {
		r.lo = tighterLower(r.lo, lower)
	}
	if upper != nil {
		r.hi = tighterUpper(r.hi, upper)
	}
}

// secondaryFor returns the first CREATE INDEX on column.
func (t *SqlTable) secondaryFor(column int) (indexDef, bool) {
	at := slices.IndexFunc(t.indexDefs, func(d indexDef) bool { return d.column == t.Columns[column].Name })
	if at < 0 {
		return indexDef{}, false
	}
	return t.indexDefs[at], true
}
```

The existing unique range code in `addBound` matches `r.column == column` only; change its `IndexFunc` predicate to `r.column == column && r.secondary == ""` so a unique and a secondary range on one column never merge (a key column never gets a secondary range, so this is a guard).

Run the planner tests. Expected: PASS.

- [ ] **Step 3: Failing lookup tests**

Append to `internal/engine/secondaryindex_test.go`:

```go
// wideGroups is a table of 1000-byte-padded rows (about fifteen to a page) with
// an INT id, an INT grp and a CREATE INDEX on grp; groupOf gives each id's group.
// Its indexes are built.
func wideGroups(t *testing.T, n int, groupOf func(id int) int) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "groups", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "grp", DataType: parser.IntDataType{}},
		{Name: "pad", DataType: parser.VarCharDataType{Size: 1200}},
	})
	require.NoError(t, err)
	pad := strings.Repeat("x", 1000)
	rows := make([][]parser.Token, n)
	for id := 0; id < n; id++ {
		rows[id] = tokRow(numTok(fmt.Sprint(id)), numTok(fmt.Sprint(groupOf(id))), strTok(pad))
	}
	require.NoError(t, table.InsertValues(nil, rows))
	require.NoError(t, table.CreateIndex("by_grp", "grp", false))
	_, err = table.indexSet()
	require.NoError(t, err)
	return table
}

func grpEq(v int) parser.Expression { return eqCmp("grp", numTok(fmt.Sprint(v))) }

func TestSecondaryLookup_ServesAnEqualityWithoutAScan(t *testing.T) {
	withCostFactor(t, 2)
	table := wideGroups(t, 160, func(id int) int { return id / 40 }) // four clustered groups
	plain := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain.noIndexes = true
	cf := countOn(table)

	got := idsOf(t, table, grpEq(1))

	assert.Equal(t, idsOf(t, plain, grpEq(1)), got)
	assert.Len(t, got, 40)
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 1, cf.getManyCalls.Load())
	assert.EqualValues(t, 40, cf.gets.Load())
	assert.Less(t, cf.pageReads.Load(), int64(5), "each page of the group is read once")
}

func TestSecondaryLookup_ServesRanges(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain.noIndexes = true
	cf := countOn(table)
	wheres := map[string]parser.Expression{
		">= 1 and < 3": andExpr(cmp(identTok("grp"), parser.GTE, numTok("1")), cmp(identTok("grp"), parser.LT, numTok("3"))),
		"> 1":          cmp(identTok("grp"), parser.GT, numTok("1")),
		"<= 1":         cmp(identTok("grp"), parser.LTE, numTok("1")),
		"< 1":          cmp(identTok("grp"), parser.LT, numTok("1")),
		"> 3":          cmp(identTok("grp"), parser.GT, numTok("3")),
		"= 9":          grpEq(9),
		"5 > grp":      cmp(numTok("5"), parser.GT, identTok("grp")),
	}
	for name, where := range wheres {
		t.Run(name, func(t *testing.T) {
			cf.reset()

			got := idsOf(t, table, where)

			assert.Equal(t, idsOf(t, plain, where), got)
			assert.EqualValues(t, 0, cf.scans.Load(), "served from the index")
		})
	}
}

func TestSecondaryLookup_ValuesSpreadOverMostPagesScan(t *testing.T) {
	withCostFactor(t, 2)
	table := wideGroups(t, 160, func(id int) int { return id % 4 }) // every group touches every page
	plain := wideGroups(t, 160, func(id int) int { return id % 4 })
	plain.noIndexes = true
	cf := countOn(table)

	got := idsOf(t, table, grpEq(1))

	assert.Equal(t, idsOf(t, plain, grpEq(1)), got, "the same answer")
	assert.EqualValues(t, 1, cf.scans.Load())
	assert.EqualValues(t, 0, cf.getManyCalls.Load())
}

func TestSecondaryLookup_UpdateAndDeleteTouchOnlyTheMatches(t *testing.T) {
	withCostFactor(t, 0)
	indexed := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain.noIndexes = true
	cf := countOn(indexed)
	assign := []parser.Assignment{{Column: "pad", Value: strTok(strings.Repeat("y", 1000))}}

	require.NoError(t, indexed.Update(assign, grpEq(2)))
	require.NoError(t, plain.Update(assign, grpEq(2)))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 40, cf.gets.Load())
	assert.Equal(t, fileRows(t, plain), fileRows(t, indexed), "the same file")
	assertIndexesMatchHeap(t, indexed)

	cf.reset()
	require.NoError(t, indexed.Delete(grpEq(2)))
	require.NoError(t, plain.Delete(grpEq(2)))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.Equal(t, fileRows(t, plain), fileRows(t, indexed))
	assertIndexesMatchHeap(t, indexed)

	cf.reset()
	assert.Empty(t, idsOf(t, indexed, grpEq(2)), "and the moved or deleted rows are no longer found")
}

func TestSecondaryLookup_TheWholeWhereIsStillChecked(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 160, func(id int) int { return id / 40 })

	got := idsOf(t, table, andExpr(grpEq(1), eqCmp("id", numTok("50"))))

	assert.Equal(t, [][]string{{"50"}}, got)
	got = idsOf(t, table, andExpr(grpEq(1), cmp(identTok("id"), parser.LT, numTok("45"))))
	assert.Len(t, got, 5, "ids 40..44")
}

func TestSecondaryLookup_APrimaryKeyEqualityWinsOverASecondaryOne(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 160, func(id int) int { return id / 40 })
	cf := countOn(table)

	got := idsOf(t, table, andExpr(grpEq(1), eqCmp("id", numTok("50"))))

	assert.Equal(t, [][]string{{"50"}}, got)
	assert.EqualValues(t, 1, cf.gets.Load(), "one row fetched through the unique tree")
	assert.EqualValues(t, 0, cf.getManyCalls.Load())
}

func TestSecondaryLookup_ANullIsFoundByNoLookup(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 40, func(id int) int { return id / 20 })
	require.NoError(t, table.Update([]parser.Assignment{{Column: "grp", Value: parser.Token{Type: parser.NULL, Value: "NULL"}}}, eqCmp("id", numTok("3"))))
	plain := wideGroups(t, 40, func(id int) int { return id / 20 })
	plain.noIndexes = true
	require.NoError(t, plain.Update([]parser.Assignment{{Column: "grp", Value: parser.Token{Type: parser.NULL, Value: "NULL"}}}, eqCmp("id", numTok("3"))))

	for _, where := range []parser.Expression{grpEq(0), cmp(identTok("grp"), parser.GTE, numTok("0")), cmp(identTok("grp"), parser.LT, numTok("5"))} {
		assert.Equal(t, idsOf(t, plain, where), idsOf(t, table, where))
	}
}

func TestSecondaryLookup_AStaleEntryFallsBackToTheScan(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 40, func(id int) int { return id / 20 })
	set, err := table.indexSet()
	require.NoError(t, err)
	ids := idsOfName(t, set.secondary["by_grp"], int64(0))
	require.NoError(t, table.file.Delete(ids[0])) // behind the index's back
	require.NoError(t, table.file.Sync())

	got := idsOf(t, table, grpEq(0))

	assert.Len(t, got, 19, "the scan's answer")
	assert.Nil(t, table.builtSet(), "the wrong index is discarded")
}

func TestSecondaryLookup_ADegradedIndexScans(t *testing.T) {
	withCostFactor(t, 0)
	table := namedIndex(t)
	long := strings.Repeat("x", maxKeyLen)
	writeRaw(t, table, []any{int64(1), "a@x.com", long})
	writeRaw(t, table, []any{int64(2), "b@x.com", "n1"})
	cf := countOn(table)

	resp, err := table.Select([]string{"id"}, eqCmp("name", strTok("n1")))

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"2"}}, resp.Rows)
	assert.EqualValues(t, 1, cf.scans.Load())
}
```

(`idsOf`, `andExpr`, `eqCmp`, `numTok`, `countOn`, `withCostFactor`, `cmp`, `binaryExpr` exist; `ids[0]` of a `RowID` tree walk is the first row of group 0.)

Run: `go test ./internal/engine/ -run 'TestSecondaryLookup_' 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL)'`
Expected: FAIL on assertions (everything scans: `scans` is 1 where 0 is expected) except the tests whose answer is the scan's anyway (`ValuesSpread`, `TheWholeWhere...`, `ANullIsFound...`, `ADegradedIndexScans`, which pass already).

- [ ] **Step 4: Implement the lookup**

In `lookupRows`, replace `trees, err := t.indexes()` with `set, err := t.indexSet()` and add `trees := set.unique`. Replace the final range loop with two passes:

```go
	// A range on a unique tree is preferred; then one on a secondary tree.
	for _, plan := range plans.ranges {
		if plan.secondary == "" {
			if tree := trees[plan.column]; tree != nil {
				return t.rangeRows(tree, plan, where, columnIndex)
			}
		}
	}
	for _, plan := range plans.ranges {
		if plan.secondary != "" {
			if tree := set.secondary[plan.secondary]; tree != nil {
				return t.rangeRows(tree, plan, where, columnIndex)
			}
		}
	}
	return nil, false, nil
```

Update the doc comment of `lookupRows`: "a bounded range on a PRIMARY KEY/UNIQUE column or on a column with a CREATE INDEX yields its rows when they lie on few enough pages".

Run: `go test -race ./internal/engine/ 2>&1 | tail -15` — Expected: PASS.

- [ ] **Step 5: A differential test where only the secondary index exists**

Append to `internal/engine/secondaryindex_test.go` (add `math/rand` to its imports):

```go
// events has no PRIMARY KEY or UNIQUE column, so every index lookup in this
// comparison is a lookup in the secondary tree.
func eventsTable(t *testing.T, noIndexes bool) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "events", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "kind", DataType: parser.VarCharDataType{Size: 20}},
	})
	require.NoError(t, err)
	require.NoError(t, table.CreateIndex("by_kind", "kind", false))
	table.noIndexes = noIndexes
	return table
}

func randKind(rng *rand.Rand) string { return fmt.Sprintf("k%d", rng.Intn(5)) }

func randomKindWhere(rng *rand.Rand) parser.Expression {
	id := func() parser.Token { return numTok(fmt.Sprint(rng.Intn(30))) }
	kind := func() parser.Token { return strTok(randKind(rng)) }
	ops := []parser.TokenType{parser.LT, parser.LTE, parser.GT, parser.GTE}
	switch rng.Intn(13) {
	case 0:
		return nil
	case 1, 2:
		return cmp(identTok("kind"), parser.EQ, kind())
	case 3:
		return cmp(kind(), parser.EQ, identTok("kind"))
	case 4:
		return cmp(identTok("kind"), ops[rng.Intn(4)], kind())
	case 5:
		return cmp(kind(), ops[rng.Intn(4)], identTok("kind"))
	case 6:
		return andExpr(cmp(identTok("kind"), ops[2+rng.Intn(2)], kind()), cmp(identTok("kind"), ops[rng.Intn(2)], kind()))
	case 7:
		return andExpr(cmp(identTok("kind"), parser.EQ, kind()), cmp(identTok("id"), ops[rng.Intn(4)], id()))
	case 8:
		return andExpr(cmp(identTok("kind"), parser.EQ, kind()), cmp(identTok("kind"), parser.NEQ, kind()))
	case 9:
		return binaryExpr(cmp(identTok("kind"), parser.EQ, kind()), parser.OR, cmp(identTok("id"), parser.EQ, id()))
	case 10:
		return cmp(identTok("id"), parser.GTE, id())
	case 11:
		return andExpr(cmp(identTok("kind"), parser.EQ, kind()), cmp(identTok("nosuch"), parser.EQ, numTok("1")))
	default:
		return cmp(identTok("kind"), parser.EQ, numTok("7")) // a literal that does not coerce
	}
}

func randomEventStatement(rng *rand.Rand) func(*SqlTable) (Response, error) {
	switch rng.Intn(10) {
	case 0, 1, 2, 3:
		rows := make([][]parser.Token, 1+rng.Intn(4))
		for i := range rows {
			if rng.Intn(8) == 0 {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(30))), parser.Token{Type: parser.NULL, Value: "NULL"})
			} else {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(30))), strTok(randKind(rng)))
			}
		}
		return func(table *SqlTable) (Response, error) { return Response{}, table.InsertValues(nil, rows) }
	case 4, 5:
		assignment := parser.Assignment{Column: "kind", Value: strTok(randKind(rng))}
		if rng.Intn(3) == 0 {
			assignment = parser.Assignment{Column: "id", Value: numTok(fmt.Sprint(rng.Intn(30)))}
		}
		where := randomKindWhere(rng)
		return func(table *SqlTable) (Response, error) {
			return Response{}, table.Update([]parser.Assignment{assignment}, where)
		}
	case 6, 7:
		where := randomKindWhere(rng)
		return func(table *SqlTable) (Response, error) { return Response{}, table.Delete(where) }
	default:
		where := randomKindWhere(rng)
		return func(table *SqlTable) (Response, error) { return table.Select([]string{"*"}, where) }
	}
}

func TestSecondaryIndexMatchesAnUnindexedTable(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			indexed, plain := eventsTable(t, false), eventsTable(t, true)
			cf := countOn(indexed)

			for op := 0; op < 400; op++ {
				statement := randomEventStatement(rng)

				gotResp, gotErr := statement(indexed)
				wantResp, wantErr := statement(plain)

				require.Equal(t, errText(wantErr), errText(gotErr), "op %d", op)
				require.Equal(t, wantResp, gotResp, "op %d", op)
				require.Equal(t, fileRows(t, plain), fileRows(t, indexed), "op %d", op)
				if indexed.builtSet() != nil {
					assertIndexesMatchHeap(t, indexed)
				}
			}
			assert.Greater(t, cf.getManyCalls.Load(), int64(0), "the secondary tree must have served some statements")
		})
	}
}
```

Only the secondary tree can have produced a `GetMany` here, because the table has no other index; `fileRows` and `assertIndexesMatchHeap` scan through the counting wrapper, so a scan count would prove nothing.

Run: `go test -race ./internal/engine/ -run SecondaryIndexMatches -v 2>&1 | tail -10`
Expected: PASS. (This test exercises code already written, so it is a differential check, not a RED step; its value is shown by the mutations below.)

- [ ] **Step 6: Mutation checks**

For each, edit, run `go test ./internal/engine/ 2>&1 | grep -E '^(--- FAIL|ok)' | head`, confirm a failure, then `git checkout internal` (commit first). A mutant that survives means a missing test: add one and rerun.

1. `addEntries` returns `true` before its secondary loop (no secondary entry is ever added).
2. `indexDeleted` skips `removeKeys`' secondary part.
3. `indexMoved` adds the new secondary entries at the old `RowID`s (`t.addEntries(set, rows, oldIDs)`), the bug an UPDATE that keeps the value would hide.
4. `GT` uses `lo` instead of `hi` in `planSecondary`.
5. `EQ` uses `lo‖0x00` as the upper bound (build `hi` by appending a zero byte to `lo` in `planSecondary`).
6. `lookupRows` tries the secondary ranges before the unique ones, and an equality is not preferred (swap the two passes and move the equality loop after them).
7. `CreateIndex` no longer calls `t.dropIndexes()`.
8. `checkSecondaryLengths` is not called from `InsertValues`.
9. `buildIndexes` does not set the secondary tree to `nil` on `ErrKeyTooLong` (return the error instead).
10. Skip the page rule for secondary plans (in `collectRangeIDs`, never refuse when the plan is over a secondary tree: pass `filePages = math.MaxInt32`).

- [ ] **Step 7: Documentation and measurement**

In `CLAUDE.md`, replace the sentence `A definition does nothing yet beyond being stored: building and using the entries is the next step.` with:

`An indexed column also gets an in-memory tree, built with the others from the same scan: its keys end in the row id (a prefix-free value encoding, then page and slot), so duplicates are fine and the rows of one value sit together in file order. An `=` or range `WHERE` on it is a lookup under the same page-count rule as a unique range, after a PRIMARY KEY/UNIQUE lookup; a PRIMARY KEY/UNIQUE column always uses its own tree. A value too long to index is rejected on INSERT and UPDATE with `key too long for index`, and one already in the file degrades only that index, which then scans.`

In `docs/specs/2026-10-15-create-index-design.md`, in "Long values" replace "`INSERT`, `UPDATE` and `ADD COLUMN` on a table with an index on a column reject" with "`INSERT` and `UPDATE` on a table with an index on a column reject" and add one sentence: "`ADD COLUMN` needs no check, because a new column cannot have an index yet."

One-off measurement (throwaway test file, deleted afterwards, quoted in the PR): a 100,000-row table with a non-key `INT` column of 1,000 distinct clustered values (`grp = id / 100`): an equality `SELECT` before `CREATE INDEX` (scan) and after (lookup); the same on a column whose values are spread (`grp = id % 1000`), where the rule refuses and the time stays at the scan's.

- [ ] **Step 8: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean.

```bash
git add internal/engine CLAUDE.md docs/specs/2026-10-15-create-index-design.md
git commit -m "feat(engine): serve = and range WHEREs on indexed columns from their trees

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
