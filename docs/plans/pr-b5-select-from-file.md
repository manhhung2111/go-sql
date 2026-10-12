# PR B5: SELECT Reads from the Table File Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `SELECT` read the table's rows by scanning its heap file page by page instead of reading the in-memory `Rows`, keeping only the matching, projected rows in memory.

**Architecture:** `Select` keeps resolving the projection and the schema-level errors first, exactly as before. It then streams the file: `File.Scan` yields one live row at a time, each is decoded with the table's schema, tested with the same `WHERE` evaluation, and projected, and only matches are kept. This is the step where the file stops being a shadow copy: B1 to B4 made every mutating statement keep the file equal to `Rows`, so flipping the read path is safe. `Rows` is still maintained (and still used by the `PRIMARY KEY`/`UNIQUE` checks and `ALTER`'s rebuild) until PR B6 deletes it. Because several `SELECT`s share the table's read lock, `File.Scan` must be safe to run concurrently with other scans; this PR documents and tests that.

**Tech Stack:** Go 1.26.4, `internal/storage` (`File.Scan`), the row codec, testify, the race detector.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.0, 5.3, 5.4).

## Prerequisite

This PR is stacked on B4, which is not merged yet. Branch from the B4 branch; no network is needed:

```bash
git checkout feat/update-to-file
git checkout -b feat/select-from-file
```

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing. The code blocks in this plan are not guaranteed to be gofmt-aligned, so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks. The plan's Commit steps mark intended commit boundaries; the user approved per-task commits for this stack.
- `plan/` is gitignored; never commit it.
- `Select` is the only statement whose behaviour changes. `InsertValues`, `Delete`, `Update`, `AlterColumns` and the `PRIMARY KEY`/`UNIQUE` checks are untouched and still maintain `Rows`.
- Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`. `Select` holds the table's read lock while it scans; every write to the file holds the write lock.
- Row order from `SELECT` is the file's order, which is unspecified by the spec: rows come back in insertion order except that an updated row moves to the end (spec 5.0).

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test:

1. `SELECT` must not read `Rows` at all (Task 2 `ReadsTheFileNotRows`).
2. After every kind of mutation (`INSERT`, `UPDATE`, `DELETE`, `ALTER ADD`/`DROP COLUMN`) `SELECT` must return exactly what `Rows` holds (Task 2 `SeesEveryMigratedStatement`).
3. `SELECT` on a closed table must return an error, not panic on the nil file, and a bad projection column must still be reported first (Task 2 `OnAClosedTableFails`).
4. Concurrent `SELECT`s, and `SELECT`s concurrent with `INSERT`s, must be race-free (Task 1 `ScansMayRunConcurrently`, Task 2 `ConcurrentSelects`, `ConcurrentWithWriters`).
5. NULL versus the empty string, a result spanning many pages, and a `WHERE` that matches only rows on late pages must all come back right (Task 2 `NullAndEmptyStringRenderDistinctly`, `AcrossManyPages`).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/storage/file.go` | Modify | document that scans may run concurrently |
| `internal/storage/file_test.go` | Modify | concurrent-scan test |
| `internal/engine/table.go` | Modify | `Select` scans the file |
| `internal/engine/select_file_test.go` | Create | the read-path tests |
| `internal/grpcserver/server_test.go` | Modify | `TestParseQuery_Update` no longer assumes row order |
| `CLAUDE.md` | Modify | describe that `SELECT` reads the file |

---

### Task 1: `File.Scan` may run concurrently with other scans

`Select` holds only the table's read lock, so two `SELECT`s can scan the same file at once. `Scan` only reads (`ReadAt` on the shared `*os.File`, a fresh buffer per page, and the page count), so it is safe, but `File`'s doc comment currently says a `File` is not safe for concurrent use. This task makes the contract precise and pins it with a race-detector test.

**Files:**
- Modify: `internal/storage/file.go`
- Modify: `internal/storage/file_test.go`

**Interfaces:**
- Consumes: `File.Insert`, `File.Scan`, the test helper `newTestFile`.
- Produces: the documented guarantee that any number of `Scan`s may run concurrently with each other, and that `Insert`, `Delete`, `Sync` and `Close` must not run concurrently with anything else.

- [ ] **Step 1: Write the test**

In `internal/storage/file_test.go`, add `"strconv"` and `"sync"` to the imports, then append:

```go
// Select holds only the table's read lock, so several scans of one file can
// run at once; Scan must therefore only read. This is meaningful under -race.
func TestSqlFile_ScansMayRunConcurrently(t *testing.T) {
	f, _ := newTestFile(t)
	const n = 3000 // about 13 bytes a row with its slot, so a few pages
	for i := 0; i < n; i++ {
		_, err := f.Insert([]byte("row-" + strconv.Itoa(i)))
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count := 0
			for _, err := range f.Scan() {
				if !assert.NoError(t, err) {
					return
				}
				count++
			}
			assert.Equal(t, n, count)
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run it**

Run: `go test -race -count=1 ./internal/storage -run TestSqlFile_ScansMayRunConcurrently -v 2>&1 | grep -E '^(--- |FAIL|ok|WARNING)'`
Expected: PASS with no `WARNING: DATA RACE`. This is a characterization test of a property the code already has, so it passes before the doc change; Step 5 proves it can fail.

- [ ] **Step 3: Update the doc comment**

In `internal/storage/file.go`, replace

```go
// A File is not safe for concurrent use; the owning table's lock serializes
// access.
```

with

```go
// Any number of Scans may run concurrently with one another: a scan only
// reads. Insert, Delete, Sync and Close must not run concurrently with
// anything else, so the owning table takes its write lock for them and only
// its read lock for a scan.
```

- [ ] **Step 4: Run the storage suite**

Run: `gofmt -w internal/storage/file.go internal/storage/file_test.go; gofmt -l internal; go vet ./internal/storage && go test -race -count=1 ./internal/storage`
Expected: `ok`.

- [ ] **Step 5: Mutation-check the test**

In `internal/storage/file.go`, make `Scan` write shared state: add `f.tail = nil` as the first statement inside the function returned by `Scan` (right after `numPages := f.numPages`). Run `go test -race -count=1 ./internal/storage -run TestSqlFile_ScansMayRunConcurrently`. Expect a `DATA RACE` failure. Restore the file (compare with a saved copy) and rerun to confirm it passes.

- [ ] **Step 6: Commit**

```bash
git add internal/storage/file.go internal/storage/file_test.go
git commit -m "docs(storage): state that scans may run concurrently

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `SELECT` scans the file

**Files:**
- Modify: `internal/engine/table.go`
- Create: `internal/engine/select_file_test.go`
- Modify: `internal/grpcserver/server_test.go`
- Modify: `CLAUDE.md`
- Modify: `plan/on-disk-persistence-design.md` (local only)

**Interfaces:**
- Consumes: `storage.File.Scan() iter.Seq2[storage.Row, error]`; `DecodeRow(columns, b) ([]any, error)`; `evalWhere`; `stringifyValue(v any) string`; the existing `Select` resolution of `columnIndex`, `outputColumns` and `outputIndices`; test helpers from earlier PRs: `usersTable(t, extra...)`, `fourUsers(t, table)`, `setTo(column, value)`, `ageColumn`, `assertMirrored(t, table)`, `cmp`, `numTok`, `strTok`, `identTok`, `tokRow`.
- Produces: a `Select` that reads only the file; test helper `stringRows(rows [][]any) [][]string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/select_file_test.go`:

```go
package engine

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// stringRows renders rows the way Select does, for comparing a result with
// what Rows holds.
func stringRows(rows [][]any) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = make([]string, len(row))
		for j, v := range row {
			out[i][j] = stringifyValue(v)
		}
	}
	return out
}

func TestTable_Select_ReadsTheFileNotRows(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	table.Rows = nil // the file alone must answer

	resp, err := table.Select([]string{"*"}, nil)

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"1", "bob"}, {"2", "sam"}, {"3", "amy"}, {"4", "lee"}}, resp.Rows)
}

func TestTable_Select_SeesEveryMigratedStatement(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, cmp(identTok("id"), parser.EQ, numTok("2"))))
	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("3"))))
	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("5"), strTok("kay"), numTok("50"))}))
	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	resp, err := table.Select([]string{"*"}, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{"id", "age"}, resp.Columns)
	assert.ElementsMatch(t, [][]string{{"1", "18"}, {"2", "18"}, {"4", "18"}, {"5", "50"}}, resp.Rows)
	assert.ElementsMatch(t, stringRows(table.Rows), resp.Rows, "the file answers exactly what Rows holds")
}

func TestTable_Select_AcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	all, err := table.Select([]string{"id"}, nil)
	require.NoError(t, err)
	late, err := table.Select([]string{"id"}, cmp(identTok("id"), parser.GTE, numTok("1400")))
	require.NoError(t, err)

	assert.Len(t, all.Rows, n)
	assert.Len(t, late.Rows, 100, "a WHERE matching only rows on the last pages")
}

func TestTable_Select_NullAndEmptyStringRenderDistinctly(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// The name is the empty string and the note was omitted, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok(""))}))

	resp, err := table.Select([]string{"*"}, nil)

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"1", "", "NULL"}}, resp.Rows)
}

func TestTable_Select_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Close())

	_, err := table.Select([]string{"*"}, nil)
	assert.EqualError(t, err, `table "users" is closed`)

	// A projection that names no column is a schema error and is reported first.
	_, err = table.Select([]string{"nope"}, nil)
	assert.EqualError(t, err, `unknown column "nope"`)
}

func TestTable_Select_ConcurrentSelects(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				resp, err := table.Select([]string{"*"}, nil)
				assert.NoError(t, err)
				assert.Len(t, resp.Rows, 4)
			}
		}()
	}
	wg.Wait()
}

func TestTable_Select_ConcurrentWithWriters(t *testing.T) {
	table := usersTable(t)
	const writers, perWriter = 4, 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := strconv.Itoa(w*perWriter + i)
				assert.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok(id), strTok("n"))}))
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := table.Select([]string{"*"}, nil)
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	resp, err := table.Select([]string{"*"}, nil)
	require.NoError(t, err)
	assert.Len(t, resp.Rows, writers*perWriter)
	assertMirrored(t, table)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/engine -run 'Test(Table_Select_(ReadsTheFile|SeesEvery|AcrossMany|NullAndEmpty|OnAClosed|Concurrent))' 2>&1 | grep -E '^(--- |FAIL|ok|panic)'`
Expected: FAIL for `ReadsTheFileNotRows` (`Rows` is nil, so `Select` returns nothing) and `OnAClosedTableFails` (the first call returns no error). `SeesEveryMigratedStatement`, `AcrossManyPages`, `NullAndEmptyStringRenderDistinctly`, `ConcurrentSelects` and `ConcurrentWithWriters` already pass because `Rows` and the file agree: they are regression guards that must keep passing once the read path moves.

- [ ] **Step 3: Write the implementation**

In `internal/engine/table.go`, replace the `Select` doc comment

```go
// Select performs a full-table scan, keeping rows where matches (a nil
// where matches every row), then projects each matching row down to
// columns ("*" expands to every column in schema order).
```

with

```go
// Select performs a full-table scan of the table's file, a page at a time,
// keeping rows where matches (a nil where matches every row), then projects
// each matching row down to columns ("*" expands to every column in schema
// order). Only the matching, projected rows are held in memory. Rows come
// back in the file's order: insertion order, except that an updated row has
// moved to the end.
```

and replace the row loop at the end of `Select`

```go
	rows := make([][]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
```

with

```go
	if t.file == nil {
		return Response{}, fmt.Errorf("table %q is closed", t.Name)
	}

	rows := make([][]string, 0)
	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return Response{}, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return Response{}, err
		}
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
```

(the rest of the loop body, the `if err != nil`, `if !matched { continue }`, the projection and `rows = append(rows, projected)`, stays as it is).

- [ ] **Step 4: Run the new tests**

Run: `gofmt -w internal/engine/table.go internal/engine/select_file_test.go; gofmt -l internal; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'Test(Table_Select)' -v 2>&1 | grep -E '^(--- |FAIL|ok)'`
Expected: all PASS, including every existing `TestTable_Select_*`.

- [ ] **Step 5: Run the whole suite and fix the order assumption**

Run: `go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: exactly one failure, `TestParseQuery_Update` in `internal/grpcserver`, because `SELECT` now returns the updated row last (`{"2","sam"}, {"1","robert"}`). If any other test fails, stop and work out why before changing anything.

In `internal/grpcserver/server_test.go`, in `TestParseQuery_Update`, replace

```go
	assert.Equal(t, [][]string{{"1", "robert"}, {"2", "sam"}}, rowStrings(resp.GetRows()))
```

with

```go
	// An UPDATE writes the new version of a row at the end of the table's file,
	// so row order after an UPDATE is unspecified.
	assert.ElementsMatch(t, [][]string{{"1", "robert"}, {"2", "sam"}}, rowStrings(resp.GetRows()))
```

Run the suite again: every package `ok`.

- [ ] **Step 6: Mutation-check the tests**

Make each change in `internal/engine/table.go`, run `go test -race -count=1 ./internal/engine ./internal/grpcserver`, confirm a failure, then restore the file (compare with a saved copy). Make sure each mutant compiles.

1. Revert `Select` to iterate `t.Rows` (the old loop). Expect `ReadsTheFileNotRows` to fail.
2. Remove the `if t.file == nil` check. Expect `OnAClosedTableFails` to fail (a nil-pointer panic counts).
3. In the scan loop, skip the `evalWhere` call and treat every row as a match. Expect the existing `Select` `WHERE` tests and `AcrossManyPages` to fail.
4. In the scan loop, decode with `t.Columns[:len(t.Columns)-1]`. Expect the tests to fail with a decode error or wrong rows.

- [ ] **Step 7: Update the docs**

In `CLAUDE.md`, replace

```
Until SELECT flips, `Rows` is still what statements read and the file is a shadow copy that migrated statements also write; INSERT writes it now, and so does ALTER ADD/DROP COLUMN:
```

with

```
SELECT now reads the file, streaming it a page at a time under the table's read lock (scans may run concurrently with one another); every mutating statement still also updates `Rows`, which only the PRIMARY KEY/UNIQUE checks and ALTER's rebuild still read until it is deleted. INSERT appends to the file, and so does ALTER ADD/DROP COLUMN:
```

and replace `(INSERT, ALTER, DELETE, UPDATE, then SELECT; ` with `(INSERT, ALTER, DELETE, UPDATE, SELECT; `.

In `plan/on-disk-persistence-design.md` section 5.0, replace ``from B3 on, `DELETE`, and from B4 on `UPDATE`, on a closed table fail`` with ``from B3 on, `DELETE`, from B4 on `UPDATE`, and from B5 on `SELECT`, on a closed table fail``, and add this sentence at the end of the `Mirror tests` bullet: ` From B5 on, `SELECT` reads the file, so a partial I/O failure that left duplicate or missing row versions in the file (spec 5.4 accepts this without a WAL) becomes visible to queries until an `ALTER` rebuilds the file from `Rows`.`

- [ ] **Step 8: Commit**

```bash
git add internal/engine/table.go internal/engine/select_file_test.go internal/grpcserver/server_test.go CLAUDE.md
git status --short   # only these files; plan/ must not appear
git commit -m "feat(engine): read SELECT results from the table's heap file

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR: title `feat(engine): read SELECT results from the table's heap file`. This branch is stacked on B4, so if the lower PRs are not on `main` yet, set the base to the B4 branch and say so in the Summary. The body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Summary should say this is step 5 of 6 and the first step where the file stops being a shadow copy, that `Rows` is still maintained for the uniqueness checks and `ALTER`, and that the client-visible changes are `SELECT` on a closed table failing and row order after an `UPDATE` (the updated row now comes back last). After a lower PR is squash-merged, move this branch with `git rebase --onto origin/main <old tip of the lower branch> <this branch>`; bound network calls with `-c http.lowSpeedLimit=1000 -c http.lowSpeedTime=30` and an explicit timeout.

## What comes next

B6 moves the `PRIMARY KEY`/`UNIQUE` checks onto a streaming scan of the file (a small set built from the statement's own key values, checked against every existing row), makes `ALTER`'s rebuild read from the file, deletes `Rows`, and moves the roughly 39 tests that inspect `table.Rows` onto `Select` or `fileRows`. After B6, memory use no longer grows with table size.

## Self-review (done while writing)

- **Spec coverage:** the `SELECT` flow in 5.3 (scan, decode, filter, project, keep only matches) is Task 2; spec 5.0's rule that `SELECT` flips only after every mutator is mirrored is satisfied by B1 to B4 and checked end to end by `SeesEveryMigratedStatement`; Task 1 makes the shared-read-lock scan contract explicit.
- **Placeholders:** none; every code step has full code and exact commands.
- **Type consistency:** `stringRows` is defined in Task 2 and used only there; `setTo`, `ageColumn`, `fourUsers` and `assertMirrored` are the helpers earlier PRs defined; `storage.File.Scan` and `DecodeRow` match the merged signatures.
- **Behaviour changes to note:** `SELECT` on a closed table now returns `table "x" is closed` instead of silently succeeding (a bad projection is still reported first); rows come back in file order, so an updated row moves to the end, which changes `TestParseQuery_Update`'s expectation.
- **Known limitations carried forward:** every `SELECT` decodes every column of every row, even when only some are projected; a `SELECT` that matches few rows still scans the whole file (no index); a partial I/O failure that left duplicate row versions in the file becomes visible until an `ALTER` rebuilds it.
