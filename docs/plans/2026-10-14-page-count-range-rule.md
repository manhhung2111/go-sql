# Page-count range rule Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a bounded key range from the index when its rows lie on at most half the file's pages, instead of when it holds at most 100 rows, reading each page once.

**Architecture:** `storage.File` gains `NumPages()` and `GetMany(ids)`, which reads each run of ids on one page once (`Get` is rebuilt on it). `rangeRows` collects the ids, tracks the distinct pages seen and gives up as soon as `rangeCostFactor × pages > filePages`, sorts the ids into file order, and fetches them with one `GetMany`. `maxRangeRows` is deleted.

**Tech Stack:** Go 1.26, testify, `internal/storage`, `internal/engine`.

**Spec:** `docs/specs/2026-10-14-page-count-range-rule-design.md`

## Global Constraints

- The rule is `rangeCostFactor * distinctPages <= filePages`, `rangeCostFactor = 2` by default; `0` means every range is a lookup (test seam only).
- Equality lookups stay unconditional and keep using `File.Get`.
- Results, errors and row order stay exactly what the scan gives; the safety rule, the whole-WHERE re-check, file order and the stale-entry fallback are unchanged.
- `GetMany` returns all rows or an error; a stale address wraps `ErrRowNotFound` and returns no rows; damage never wraps it.
- Error messages: lowercase, no trailing punctuation, "out of range [lo, hi)" phrasing. Do not import stdlib `cmp` in `internal/engine` non-test code (a test helper named `cmp` exists in the package).
- Commits: Conventional Commits, ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Run `gofmt -w` on files you touch. `go test -race ./...` before the last commit of each task.

## Review Focus

- A range whose rows are spread over most pages must scan even when it is small (the old cap would have served it).
- A range with many rows on few pages must be a lookup (the old cap would have refused it).
- `GetMany` with ids not sorted by page must still return the right rows in the order given.
- A stale entry found by `GetMany` (deleted row, row beyond the file) must fall back to the scan and drop the indexes, never return a partial result.
- An empty file or an empty range must not divide by zero or read a page.

## File Structure

- `internal/storage/file.go`: `NumPages`, `GetMany`, shared page-reading helper behind `Get`.
- `internal/storage/file_test.go`: `GetMany` and `NumPages` tests.
- `internal/engine/lookup.go`: `rangeCostFactor`, new `rangeRows`, `matchRow` split out of `fetchMatching`; `maxRangeRows` removed.
- `internal/engine/testhelpers_test.go`: `countingFile` counts `GetMany` and page reads.
- `internal/engine/main_test.go` (new): `TestMain` sets `rangeCostFactor = 0`; helper `withCostFactor`.
- `internal/engine/range_test.go`, `indexdiff_test.go`: rule tests; the old cap tests are replaced.
- `CLAUDE.md`, `docs/specs/2026-10-13-range-lookup-design.md`: wording.

---

### Task 1: `File.NumPages` and `File.GetMany`

**Files:**
- Modify: `internal/storage/file.go` (interface at the `File` type; `Get` at the end of `sqlFile`)
- Test: `internal/storage/file_test.go`

**Interfaces:**
- Consumes: `PageFile.ReadPage`, `PageFile.NumPages`, `validateLayout`, `sqlSlottedPage`, `ErrRowNotFound`.
- Produces: `File.NumPages() int32`; `File.GetMany(ids []RowID) ([][]byte, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/storage/file_test.go` (add `"bytes"` to the imports):

```go
// countingPages counts the pages read through it.
type countingPages struct {
	PageFile
	reads int
}

func (c *countingPages) ReadPage(n int32) ([]byte, error) {
	c.reads++
	return c.PageFile.ReadPage(n)
}

// threePages inserts nine 5000-byte rows, three to a page, so ids[0:3] are on
// page 0, ids[3:6] on page 1 and ids[6:9] on page 2.
func threePages(t *testing.T, f File) ([]RowID, [][]byte) {
	t.Helper()
	ids := make([]RowID, 9)
	rows := make([][]byte, 9)
	for i := range ids {
		rows[i] = bytes.Repeat([]byte{byte('a' + i)}, 5000)
		id, err := f.Insert(rows[i])
		require.NoError(t, err)
		ids[i] = id
	}
	require.Equal(t, int32(0), ids[2].Page)
	require.Equal(t, int32(1), ids[3].Page)
	require.Equal(t, int32(2), ids[8].Page)
	return ids, rows
}

func TestSqlFile_NumPages(t *testing.T) {
	f, _ := newTestFile(t)
	assert.Equal(t, int32(0), f.NumPages())

	threePages(t, f)

	assert.Equal(t, int32(3), f.NumPages())
}

func TestSqlFile_GetMany(t *testing.T) {
	t.Run("returns the rows Get returns, in the order of ids, before and after a reopen", func(t *testing.T) {
		f, path := newTestFile(t)
		ids, rows := threePages(t, f)
		reversed := []RowID{ids[8], ids[0], ids[4], ids[3]}

		got, err := f.GetMany(reversed)
		require.NoError(t, err)
		require.Equal(t, [][]byte{rows[8], rows[0], rows[4], rows[3]}, got)

		require.NoError(t, f.Close())
		reopened, err := OpenFile(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = reopened.Close() })
		got, err = reopened.GetMany(ids)
		require.NoError(t, err)
		assert.Equal(t, rows, got)
	})

	t.Run("reads each run of ids on one page once", func(t *testing.T) {
		inner, err := CreatePageFile(filepath.Join(t.TempDir(), "table.tbl"))
		require.NoError(t, err)
		pages := &countingPages{PageFile: inner}
		f := &sqlFile{pages: pages}
		t.Cleanup(func() { _ = f.Close() })
		ids, _ := threePages(t, f)

		tests := []struct {
			name  string
			ids   []RowID
			reads int
		}{
			{"all nine, sorted", ids, 3},
			{"two on one page and one on another", []RowID{ids[0], ids[1], ids[3]}, 2},
			{"a page revisited after another is read again", []RowID{ids[0], ids[3], ids[1]}, 3},
			{"the same id twice", []RowID{ids[4], ids[4]}, 1},
			{"no ids", nil, 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				pages.reads = 0

				got, err := f.GetMany(tc.ids)

				require.NoError(t, err)
				assert.Len(t, got, len(tc.ids))
				assert.NotNil(t, got, "an empty result is an empty slice")
				assert.Equal(t, tc.reads, pages.reads)
			})
		}
	})

	t.Run("an address with no live row fails the whole call, wherever it sits", func(t *testing.T) {
		f, _ := newTestFile(t)
		ids, _ := threePages(t, f)
		require.NoError(t, f.Delete(ids[4]))
		bad := map[string]RowID{
			"a deleted row":       ids[4],
			"a slot out of range": {Page: 1, Slot: 9},
			"a page out of range": {Page: 3, Slot: 0},
			"a negative page":     {Page: -1, Slot: 0},
		}
		for name, id := range bad {
			for position, list := range map[string][]RowID{
				"first":  {id, ids[0], ids[8]},
				"middle": {ids[0], id, ids[8]},
				"last":   {ids[0], ids[8], id},
			} {
				t.Run(name+" "+position, func(t *testing.T) {
					got, err := f.GetMany(list)

					require.ErrorIs(t, err, ErrRowNotFound)
					assert.Nil(t, got)
				})
			}
		}
	})

	t.Run("a corrupt page is an error but not ErrRowNotFound", func(t *testing.T) {
		f, path := newTestFile(t)
		ids, _ := threePages(t, f)
		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = raw.WriteAt([]byte{0xFF}, maxPageSize*2-1) // last byte of page 1
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		got, err := f.GetMany([]RowID{ids[0], ids[3]})

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrRowNotFound)
		assert.ErrorContains(t, err, "checksum mismatch")
		assert.Nil(t, got)
	})

	t.Run("the returned bytes belong to the caller, even over a cached page file", func(t *testing.T) {
		inner, err := CreatePageFile(filepath.Join(t.TempDir(), "table.tbl"))
		require.NoError(t, err)
		cached, err := NewCachedPageFile(inner, 4)
		require.NoError(t, err)
		f := &sqlFile{pages: cached}
		t.Cleanup(func() { _ = f.Close() })
		id, err := f.Insert([]byte("abc"))
		require.NoError(t, err)

		first, err := f.GetMany([]RowID{id})
		require.NoError(t, err)
		first[0][0] = 'X'
		second, err := f.GetMany([]RowID{id})

		require.NoError(t, err)
		assert.Equal(t, "abc", string(second[0]))
	})
}

// GetMany holds only the table's read lock, like Get and Scan. Meaningful
// under -race.
func TestSqlFile_GetManyMayRunConcurrentlyWithGetsAndScans(t *testing.T) {
	f, path := newTestFile(t)
	ids, rows := threePages(t, f)
	require.NoError(t, f.Close())
	f, err := OpenFile(path) // reopened: the tail cache is empty
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				got, err := f.GetMany(ids)
				if assert.NoError(t, err) {
					assert.Equal(t, rows, got)
				}
				_, err = f.Get(ids[i%9])
				assert.NoError(t, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, err := range f.Scan() {
			assert.NoError(t, err)
		}
	}()
	wg.Wait()
}
```

- [ ] **Step 2: Add stubs and watch the tests fail**

In `internal/storage/file.go`, add to the `File` interface (after `Get`):

```go
	// NumPages returns the number of pages in the file.
	NumPages() int32
	// GetMany returns a copy of the row at each id, in the order of ids. Each
	// run of consecutive ids on one page reads that page once, so ids sorted
	// by page read each page once. It fails with an error wrapping
	// ErrRowNotFound if any id has no live row, and returns no rows then. Like
	// Get, any number of GetManys may run concurrently with each other and
	// with Gets and Scans.
	GetMany(ids []RowID) ([][]byte, error)
```

and stubs on `sqlFile`:

```go
func (f *sqlFile) NumPages() int32 { return 0 }

func (f *sqlFile) GetMany(ids []RowID) ([][]byte, error) { return nil, nil }
```

Run: `go test ./internal/storage/ -run 'NumPages|GetMany' -v`
Expected: FAIL (NumPages is 0 not 3; GetMany results empty / no error where one is expected). It must compile and fail on assertions, not on a build error.

- [ ] **Step 3: Implement**

Replace the stubs and the body of `Get` in `internal/storage/file.go`:

```go
func (f *sqlFile) NumPages() int32 { return f.pages.NumPages() }

func (f *sqlFile) Get(id RowID) ([]byte, error) {
	rows, err := f.GetMany([]RowID{id})
	if err != nil {
		return nil, err
	}
	return rows[0], nil
}

func (f *sqlFile) GetMany(ids []RowID) ([][]byte, error) {
	numPages := f.pages.NumPages()
	rows := make([][]byte, len(ids))
	var page *sqlSlottedPage
	loaded := int32(-1)
	for i, id := range ids {
		if id.Page < 0 || id.Page >= numPages {
			return nil, fmt.Errorf("%w: page %d out of range [0, %d)", ErrRowNotFound, id.Page, numPages)
		}
		if id.Page != loaded {
			// Read straight from the page file, like Scan: the tail cache
			// equals the disk, so the result is the same and this touches no
			// mutable state.
			buf, err := f.pages.ReadPage(id.Page)
			if err != nil {
				return nil, err
			}
			if err := validateLayout(buf); err != nil {
				return nil, fmt.Errorf("page %d: %w", id.Page, err)
			}
			page, loaded = &sqlSlottedPage{buf: buf}, id.Page
		}
		if id.Slot < 0 || id.Slot >= page.SlotCount() {
			return nil, fmt.Errorf("%w: page %d slot %d out of range [0, %d)", ErrRowNotFound, id.Page, id.Slot, page.SlotCount())
		}
		row, live := page.Row(id.Slot)
		if !live {
			return nil, fmt.Errorf("%w: page %d slot %d is deleted", ErrRowNotFound, id.Page, id.Slot)
		}
		rows[i] = bytes.Clone(row)
	}
	return rows, nil
}
```

(Delete the old body of `Get`; keep its doc comment.)

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/storage/ -v 2>&1 | tail -30`
Expected: PASS, including every existing `TestSqlFile_Get` case (messages unchanged).

- [ ] **Step 5: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: no gofmt output, all packages ok (the engine's wrappers embed `File`, so they compile).

```bash
git add internal/storage/file.go internal/storage/file_test.go
git commit -m "feat(storage): add File.NumPages and File.GetMany

GetMany reads each run of row ids on one page once; Get is now a call of it.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: The page-count rule in `rangeRows`

**Files:**
- Modify: `internal/engine/lookup.go` (`maxRangeRows`, `rangeRows`, `fetchMatching`)
- Modify: `internal/engine/testhelpers_test.go` (`countingFile`)
- Create: `internal/engine/main_test.go`
- Modify: `internal/engine/range_test.go` (replace `TestRangeLookup_GivesUpOnABigRange`; add rule tests)
- Modify: `internal/engine/indexdiff_test.go` (the lowered-cap test)

**Interfaces:**
- Consumes: Task 1's `File.NumPages`, `File.GetMany`; existing `lookupRows`, `evalWhere`, `DecodeRow`, helpers `intKeys`, `sequence`, `idsOf`, `andExpr`, `cmp`, `countOn`, `newTempTable`, `tokRow`, `strTok`, `numTok`, `fileRows`, `assertIndexesMatchHeap`.
- Produces: `rangeCostFactor` (package var, default 2); `countingFile.pageReads`, `countingFile.getManyCalls`, `countingFile.reset()`; `withCostFactor(t, n)`; `wideKeys(t, ids)`; `distinctPages(t, table, lo, hi)`.

- [ ] **Step 1: Test scaffolding (no behaviour yet)**

Create `internal/engine/main_test.go`:

```go
package engine

import (
	"os"
	"testing"
)

// The engine's tables in tests are a few rows on one page, which the page-count
// rule would always scan. Run the package with every range a lookup, and let
// the tests of the rule itself set the real factor.
func TestMain(m *testing.M) {
	rangeCostFactor = 0
	os.Exit(m.Run())
}

// withCostFactor sets rangeCostFactor for one test.
func withCostFactor(t *testing.T, factor int) {
	t.Helper()
	saved := rangeCostFactor
	rangeCostFactor = factor
	t.Cleanup(func() { rangeCostFactor = saved })
}
```

In `internal/engine/testhelpers_test.go`, extend `countingFile`:

```go
type countingFile struct {
	storage.File
	scans        atomic.Int64
	gets         atomic.Int64 // rows fetched by Get or GetMany
	pageReads    atomic.Int64 // pages a Get or GetMany would read
	getManyCalls atomic.Int64
}

func (f *countingFile) Get(id storage.RowID) ([]byte, error) {
	f.gets.Add(1)
	f.pageReads.Add(1)
	return f.File.Get(id)
}

func (f *countingFile) GetMany(ids []storage.RowID) ([][]byte, error) {
	f.getManyCalls.Add(1)
	f.gets.Add(int64(len(ids)))
	runs := 0
	for i, id := range ids {
		if i == 0 || id.Page != ids[i-1].Page {
			runs++
		}
	}
	f.pageReads.Add(int64(runs))
	return f.File.GetMany(ids)
}

func (f *countingFile) reset() {
	f.scans.Store(0)
	f.gets.Store(0)
	f.pageReads.Store(0)
	f.getManyCalls.Store(0)
}
```

(Replace the existing `Get` method.) In `internal/engine/lookup.go` add, next to `maxRangeRows` (keep that for now):

```go
var rangeCostFactor = 2
```

Run: `go test ./internal/engine/ 2>&1 | tail -5`
Expected: PASS (the package behaves as before: the new variable is unused, and `GetMany` is not called yet).

- [ ] **Step 2: Write the failing rule tests**

Add to `internal/engine/range_test.go` (add `strings` to its imports if absent):

```go
// wideKeys is a table of rows with a 1000-byte padding column, about fifteen to
// a page, inserted in the order given; ids is also the set of keys. Its indexes
// are built.
func wideKeys(t *testing.T, ids []int) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "wide", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "email", DataType: parser.VarCharDataType{Size: 100}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
		{Name: "pad", DataType: parser.VarCharDataType{Size: 1200}},
	})
	require.NoError(t, err)
	pad := strings.Repeat("x", 1000)
	rows := make([][]parser.Token, len(ids))
	for i, id := range ids {
		rows[i] = tokRow(numTok(fmt.Sprint(id)), strTok(fmt.Sprintf("e%05d", id)), strTok(pad))
	}
	require.NoError(t, table.InsertValues(nil, rows))
	_, err = table.indexes()
	require.NoError(t, err)
	return table
}

// distinctPages is how many pages hold the rows with lo <= id < hi.
func distinctPages(t *testing.T, table *SqlTable, lo, hi int) int {
	t.Helper()
	pages := map[int32]bool{}
	for row, err := range table.file.Scan() {
		require.NoError(t, err)
		decoded, err := DecodeRow(table.Columns, row.Bytes)
		require.NoError(t, err)
		if id := int(decoded[0].(int64)); id >= lo && id < hi {
			pages[row.ID.Page] = true
		}
	}
	return len(pages)
}

func idRange(lo, hi int) parser.Expression {
	return andExpr(cmp(identTok("id"), parser.GTE, numTok(fmt.Sprint(lo))), cmp(identTok("id"), parser.LT, numTok(fmt.Sprint(hi))))
}

func TestRangeLookup_UsesTheIndexWhileHalfTheFileOrLess(t *testing.T) {
	withCostFactor(t, 2)
	table := wideKeys(t, sequence(0, 159))
	pages := int(table.file.NumPages())
	require.Greater(t, pages, 6, "the table must span several pages")
	cf := countOn(table)
	lookups, scans := 0, 0

	for k := 1; k <= 160; k++ {
		d := distinctPages(t, table, 0, k)
		cf.reset() // distinctPages scanned through the wrapper
		got := idsOf(t, table, idRange(0, k))

		require.Len(t, got, k)
		if 2*d <= pages {
			lookups++
			require.EqualValues(t, 0, cf.scans.Load(), "k=%d d=%d pages=%d should be a lookup", k, d, pages)
			require.EqualValues(t, d, cf.pageReads.Load(), "k=%d reads each page once", k)
			require.EqualValues(t, k, cf.gets.Load(), "k=%d", k)
		} else {
			scans++
			require.EqualValues(t, 1, cf.scans.Load(), "k=%d d=%d pages=%d should scan", k, d, pages)
			require.EqualValues(t, 0, cf.pageReads.Load(), "k=%d", k)
		}
	}
	assert.Greater(t, lookups, 0)
	assert.Greater(t, scans, 0, "both sides of the boundary must be exercised")
}

func TestRangeLookup_ManyRowsOnFewPagesIsALookup(t *testing.T) {
	withCostFactor(t, 2)
	table := wideKeys(t, sequence(0, 1599))
	pages := int(table.file.NumPages())
	d := distinctPages(t, table, 0, 300)
	require.LessOrEqual(t, 2*d, pages, "the setup must satisfy the rule")
	cf := countOn(table)

	got := idsOf(t, table, idRange(0, 300))

	assert.Len(t, got, 300, "more rows than the old cap of 100")
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 1, cf.getManyCalls.Load())
	assert.EqualValues(t, d, cf.pageReads.Load())
}

func TestRangeLookup_FewRowsSpreadOverMostPagesScan(t *testing.T) {
	withCostFactor(t, 2)
	// Ids 0..9 are placed a page apart, so a range of ten rows touches ten of
	// the file's pages: the old cap served it, a scan is cheaper.
	order := make([]int, 160)
	for r := 0; r < 10; r++ {
		for c := 0; c < 16; c++ {
			order[r*16+c] = c*10 + r
		}
	}
	indexed, plain := wideKeys(t, order), wideKeys(t, order)
	plain.noIndexes = true
	pages := int(indexed.file.NumPages())
	require.Greater(t, 2*distinctPages(t, indexed, 0, 10), pages, "the setup must break the rule")
	cf := countOn(indexed)

	got := idsOf(t, indexed, idRange(0, 10))

	assert.Equal(t, idsOf(t, plain, idRange(0, 10)), got, "the same answer")
	assert.EqualValues(t, 1, cf.scans.Load())
	assert.EqualValues(t, 0, cf.getManyCalls.Load(), "a refused range reads no page")
}

func TestRangeLookup_AnEmptyRangeAndAnEmptyFileReadNothing(t *testing.T) {
	withCostFactor(t, 2)
	table := wideKeys(t, sequence(0, 39))
	cf := countOn(table)

	assert.Empty(t, idsOf(t, table, idRange(50, 60)))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 0, cf.getManyCalls.Load())

	empty := keyedTable(t)
	assert.Empty(t, idsOf(t, empty, idRange(0, 10)))
}
```

Delete `TestRangeLookup_GivesUpOnABigRange` (the cap no longer exists). Run:

`go test ./internal/engine/ -run 'TestRangeLookup_(UsesTheIndex|ManyRows|FewRowsSpread|AnEmpty)' -v 2>&1 | tail -30`

Expected: `UsesTheIndexWhileHalfTheFileOrLess` FAIL (the old code serves ranges of up to 100 rows, so a `k` whose pages exceed half the file is a lookup, not a scan, and `getManyCalls` is 0); `ManyRowsOnFewPagesIsALookup` FAIL (300 rows exceed the cap, so `scans` is 1); `FewRowsSpreadOverMostPagesScan` FAIL (ten rows are served by lookup); the empty test passes already. These are assertion failures; fix any compile error (the tests use only helpers that exist after Step 1) before continuing.

- [ ] **Step 3: Implement**

In `internal/engine/lookup.go`, delete `maxRangeRows` and its comment, and put the factor in its place:

```go
// rangeCostFactor is how many pages a scan may read per page a range lookup
// reads before the lookup stops being worth it: a lookup's reads are scattered
// and a scan's are sequential. A range is a lookup while its rows lie on at
// most 1/rangeCostFactor of the file's pages. It is a variable so a test can
// set it to 0, which makes every range a lookup.
var rangeCostFactor = 2
```

Update the `lookupRows` doc comment: replace "a bounded range yields at most maxRangeRows" with "a bounded range yields its rows when they lie on few enough pages (see rangeCostFactor)". Replace `rangeRows` and `fetchMatching`:

```go
// rangeRows fetches the rows whose key lies in plan's range and that satisfy
// where, in file order. It reports not usable if the rows lie on more than
// 1/rangeCostFactor of the file's pages, or an entry is stale.
func (t *SqlTable) rangeRows(tree *index.Tree, plan rangePlan, where parser.Expression, columnIndex map[string]int) ([]fetchedRow, bool, error) {
	filePages := int(t.file.NumPages())
	seen := make(map[int32]struct{})
	var ids []storage.RowID
	for _, id := range tree.Range(plan.lo, plan.hi) {
		seen[id.Page] = struct{}{}
		if rangeCostFactor*len(seen) > filePages {
			return nil, false, nil
		}
		ids = append(ids, id)
	}
	// The scan returns rows by page and slot; so must we, or results and the
	// order of an UPDATE's new versions would depend on the access path. It
	// also puts the rows of one page next to each other for GetMany.
	slices.SortFunc(ids, func(a, b storage.RowID) int {
		if a.Page != b.Page {
			if a.Page < b.Page {
				return -1
			}
			return 1
		}
		return a.Slot - b.Slot
	})

	encoded, err := t.file.GetMany(ids)
	if errors.Is(err, storage.ErrRowNotFound) {
		t.dropIndexes()
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rows []fetchedRow
	for i, b := range encoded {
		row, matched, err := t.matchRow(b, where, columnIndex)
		if err != nil {
			return nil, false, err
		}
		if matched {
			rows = append(rows, fetchedRow{id: ids[i], row: row})
		}
	}
	return rows, true, nil
}

// fetchMatching reads the row at id and reports whether it satisfies where.
// stale is true when the file has no row there, which means the index is wrong.
func (t *SqlTable) fetchMatching(id storage.RowID, where parser.Expression, columnIndex map[string]int) (row []any, matched, stale bool, err error) {
	b, err := t.file.Get(id)
	if errors.Is(err, storage.ErrRowNotFound) {
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	row, matched, err = t.matchRow(b, where, columnIndex)
	return row, matched, false, err
}

// matchRow decodes an encoded row and reports whether it satisfies where.
func (t *SqlTable) matchRow(b []byte, where parser.Expression, columnIndex map[string]int) (row []any, matched bool, err error) {
	if row, err = DecodeRow(t.Columns, b); err != nil {
		return nil, false, err
	}
	matched, err = evalWhere(where, row, columnIndex, t.Columns)
	if err != nil {
		return nil, false, err
	}
	return row, matched, nil
}
```

Replace the lowered-cap test in `internal/engine/indexdiff_test.go`:

```go
// With the factor so high that every non-empty range is too expensive, all
// ranges scan: the answers must not change. (The rest of the package runs with
// every range a lookup; see TestMain.)
func TestIndexedTableMatchesWhenEveryRangeScans(t *testing.T) {
	withCostFactor(t, 1_000_000)
	for seed := int64(7); seed <= 8; seed++ {
		compareWithUnindexed(t, seed)
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/engine/ 2>&1 | tail -30`
Expected: PASS. If a pre-existing test fails because it counted `gets` against the old per-row `Get` (for example `DeleteAndUpdateTouchOnlyTheRange` expects 5 gets: `countingFile.GetMany` adds `len(ids)`, so it should still hold), fix the test, not the counter semantics.

- [ ] **Step 5: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: clean and all ok.

```bash
git add internal/engine
git commit -m "feat(engine): serve a key range from the index when its rows lie on few pages

Replaces the cap of 100 rows with a comparison of the distinct pages the rows
lie on against the file's pages, and reads each page once through GetMany.

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Differential on wide rows, mutation checks, docs, measurement

**Files:**
- Modify: `internal/engine/range_test.go` (differential on wide rows; stale entry via `GetMany`)
- Modify: `CLAUDE.md`, `docs/specs/2026-10-13-range-lookup-design.md`

**Interfaces:**
- Consumes: Task 2's `wideKeys`, `withCostFactor`, `countOn`, `idRange`.

- [ ] **Step 1: Differential test with both outcomes**

Add to `internal/engine/range_test.go` (add `math/rand` to its imports):

```go
// With the real factor and wide rows some random ranges are lookups and some
// scan; either way the answers and the file must match an unindexed table, also
// after UPDATEs have moved rows.
func TestRangeLookup_MatchesTheScanOnWideRowsWithTheRealFactor(t *testing.T) {
	withCostFactor(t, 2)
	rng := rand.New(rand.NewSource(11))
	order := rng.Perm(320)
	indexed, plain := wideKeys(t, order), wideKeys(t, order)
	plain.noIndexes = true
	cf := countOn(indexed)
	ops := []parser.TokenType{parser.LT, parser.LTE, parser.GT, parser.GTE}

	for step := 0; step < 300; step++ {
		lo, width := rng.Intn(320), 1+rng.Intn(60)
		where := andExpr(
			cmp(identTok("id"), ops[2+rng.Intn(2)], numTok(fmt.Sprint(lo))),
			cmp(identTok("id"), ops[rng.Intn(2)], numTok(fmt.Sprint(lo+width))))
		if rng.Intn(5) == 0 {
			assign := []parser.Assignment{{Column: "pad", Value: strTok(strings.Repeat("y", 1000))}}
			require.NoError(t, indexed.Update(assign, where))
			require.NoError(t, plain.Update(assign, where))
			require.Equal(t, fileRows(t, plain), fileRows(t, indexed), "step %d", step)
			continue
		}
		require.Equal(t, idsOf(t, plain, where), idsOf(t, indexed, where), "step %d", step)
	}
	assertIndexesMatchHeap(t, indexed)
	assert.Greater(t, cf.scans.Load(), int64(0), "some ranges must have scanned")
	assert.Greater(t, cf.getManyCalls.Load(), int64(0), "and some must have been lookups")
}
```

Run: `go test -race ./internal/engine/ -run MatchesTheScanOnWideRows -v 2>&1 | tail`
Expected: PASS (it exercises code already written, so it is a differential check, not a RED step; its value is shown by the mutations below).

- [ ] **Step 2: Mutation checks**

For each mutation below, edit `internal/engine/lookup.go` (or `internal/storage/file.go` for the last), run `go test ./internal/engine/ ./internal/storage/ 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | head`, confirm at least one failure, then `git checkout internal` to restore. Record each result in the PR body. A mutation that no test kills is a missing test: add one, and rerun.

1. Compare without the factor: `rangeCostFactor*len(seen) > filePages` becomes `len(seen) > filePages`.
2. Count rows instead of pages: `len(seen)` becomes `len(ids)+1`.
3. Skip the early exit: delete the `if rangeCostFactor*len(seen) > filePages { ... }` block.
4. Read per row: replace the `GetMany` call by a loop of `Get`s collecting the bytes (the `getManyCalls`/`pageReads` assertions must fail).
5. Skip the sort: delete the `slices.SortFunc` call.
6. Treat `ErrRowNotFound` from `GetMany` as no rows: return `nil, true, nil` in that branch (the stale-entry tests must fail).
7. Keep the old cap beside the rule: add `if len(ids) == 100 { return nil, false, nil }` before the append (`ManyRowsOnFewPages` must fail).
8. In `GetMany`, reload the page for every id (`if id.Page != loaded` becomes `if true`): the page-read count test must fail.

- [ ] **Step 3: Documentation**

In `CLAUDE.md`, replace

`A bounded range on a key column (`<`, `<=`, `>`, `>=`, alone or as a pair) is a lookup too when it holds at most 100 rows: the rows are fetched in file order, so results, their order and the file an UPDATE writes are the scan's; a bigger range scans.`

with

`A bounded range on a key column (`<`, `<=`, `>`, `>=`, alone or as a pair) is a lookup too when its rows lie on at most half of the file's pages (the index knows the exact ids, so the pages are counted, not estimated): the rows are fetched in file order, each page read once, so results, their order and the file an UPDATE writes are the scan's; a range spread over more of the file scans.`

In `docs/specs/2026-10-13-range-lookup-design.md`, after the "Give up on big ranges" bullet and after step 1 of "Fetching", add a sentence: `The fixed cap was replaced by a comparison of pages; see [the page-count rule](2026-10-14-page-count-range-rule-design.md).`

Run: `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: clean, all ok.

- [ ] **Step 4: One-off measurement (not committed)**

Write a throwaway `_test.go` in the scratchpad copy of the repo (or a temp test file deleted afterwards) that builds a 100,000-row `wideKeys`-style table of small rows (id, email, name; inserted in key order), builds its indexes, and times `SELECT` over (a) `id >= 1000 AND id < 2000` (clustered, 1,000 rows, formerly over the cap), (b) the same with the table built from a shuffled insert order (spread), each with `rangeCostFactor` 2 and with `noIndexes = true`. Quote the four numbers in the PR. Delete the file; `git status` must show it gone.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/range_test.go CLAUDE.md docs/specs/2026-10-13-range-lookup-design.md
git commit -m "test(engine): compare page-count range lookups with scans on wide rows

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
