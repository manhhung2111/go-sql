# PR B6: Delete the In-Memory Rows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove `SqlTable.Rows` so a table's rows live only in its heap file and memory use no longer grows with table size, by moving the `PRIMARY KEY`/`UNIQUE` checks and the `ALTER` rebuild onto streaming scans of the file.

**Architecture:** Every statement already writes the file (B1 to B4) and `SELECT` already reads it (B5), but `INSERT`, `UPDATE`, `DELETE` and both `ALTER` paths still read `Rows` for matching, uniqueness or rebuilding. Because those statements depend on each other through `Rows`, they cannot be converted one at a time, so this PR has two tasks. Task 1 moves every test off `Rows` (test-only, `Rows` still exists), so the tests assert on the file's contents directly. Task 2 then rewrites the statements to be file-only and deletes the field in one commit, with the migrated tests as the safety net. Uniqueness is checked by streaming: build a small set from only the key values the statement inserts or assigns, then scan the file once against it, so memory is proportional to the statement, not the table (time is still one scan per statement until an index exists).

**Tech Stack:** Go 1.26.4, `internal/storage` (`File.Scan`, `Insert`, `Delete`, `Sync`), the row codec, testify.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.0, 5.3, 5.4).

## Prerequisite

This PR is stacked on B5, which is not merged yet. Branch from the B5 branch; no network is needed:

```bash
git checkout feat/select-from-file
git checkout -b feat/delete-rows
```

Task 1 can be its own PR (it changes no production code); if so, branch Task 2's work from it.

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing. The code blocks in this plan are not guaranteed to be gofmt-aligned, so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks. The plan's Commit steps mark intended commit boundaries; the user approved per-task commits for this stack.
- `plan/` is gitignored; never commit it.
- Every error message and its precedence must stay exactly as before: a `WHERE`/`SET` error outranks a uniqueness error, a duplicate key in an earlier `INSERT` row outranks an error in a later row, and an `UPDATE` reports the first conflicting unique column in assignment order.
- Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`. `Select` holds the read lock while scanning; every other statement holds the write lock for its whole scan-and-write.
- `UPDATE` still never assigns NULL (the parser only accepts a STRING or NUMBER literal after `SET`); do not add NULL assignment.
- A row whose encoding exceeds one page (`storage.MaxRowSize`) is rejected with `row too large`; the space of tombstoned rows is not reclaimed (spec non-goals).

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test:

1. A duplicate key in an earlier `INSERT` row is still reported before an error in a later row, a coercion error in an earlier row before a later duplicate, and a conflict with an existing row is found wherever it sits in the file (Task 2 `DuplicateBeatsALaterRowError`, `CoercionErrorBeatsALaterDuplicate`, `DuplicateOnALatePage`).
2. An `UPDATE` that assigns several unique columns still reports the first conflicting column in assignment order, including when two matched rows would collide with each other (Task 2 `UniqueConflictReportsTheFirstConflictingColumn`, `SeveralMatchesReportTheFirstUniqueColumn`, `UniquenessAcrossManyPages`).
3. `ALTER ADD COLUMN` with a `UNIQUE`/`PRIMARY KEY` default is still rejected on two or more rows and accepted on one, and a rebuild that fails (row too large, I/O) leaves the old file, schema and directory untouched (existing `TestTable_AlterColumns_AddColumn_*`, `TestTable_Alter_*`, and Task 2 `AddColumn_UniqueDefaultIsAcceptedOnFewerThanTwoRows`).
4. After the field is gone no statement may depend on state outside the file; the temporary white-box tests that empty `Rows` first prove the new code does not read it (Task 2 Step 2).
5. Nothing holds a whole table in memory: `rebuildFile` streams row by row and the uniqueness checks keep only the statement's own keys (reviewed by reading; not observable by a test).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/engine/testhelpers_test.go` | Modify | `assertTableRows`, `assertTableRowsAnyOrder`; drop `assertMirrored` |
| `internal/engine/{insert,alter,delete,update,select}_file_test.go` | Modify | assert on the file's contents; drop shadow-copy tests |
| `internal/engine/table_test.go` | Modify | read `fileRows` instead of `Rows` |
| `internal/engine/uniqueness_test.go` | Create | precedence and many-page uniqueness tests |
| `internal/engine/table.go` | Modify | file-only statements; `Rows` removed |
| `CLAUDE.md`, `plan/on-disk-persistence-design.md` | Modify | describe the finished design |

---

### Task 1: Move every test off `Rows`

Test-only. `Rows` still exists and is still maintained, but no test reads or writes it afterwards, so Task 2 can delete it. The mirror assertions of B1 to B5 (which compared `Rows` with the file) become explicit expectations on the file's contents; the tests that existed only to prove the shadow-copy design (`ReadsTheFileNotRows`, the three "drifted from Rows" tests) are removed.

**Files:**
- Modify: `internal/engine/testhelpers_test.go`, `insert_file_test.go`, `alter_file_test.go`, `delete_file_test.go`, `update_file_test.go`, `select_file_test.go`, `table_test.go`

**Interfaces:**
- Consumes: `fileRows(t, table) [][]any` (decodes every live row of the table's file with its current schema).
- Produces:
  - `func assertTableRows(t *testing.T, table *SqlTable, want ...[]any)` (the file holds exactly `want`, in file order)
  - `func assertTableRowsAnyOrder(t *testing.T, table *SqlTable, want ...[]any)` (same, in any order; an `UPDATE` appends the new version at the end of the file)

- [ ] **Step 1: Replace the mirror helper**

In `internal/engine/testhelpers_test.go`, replace the `fileRows` doc comment's second line and the whole `assertMirrored` function (the last two declarations in the file) with:

```go
// fileRows scans the table's file and decodes every live row with the
// table's current schema: the table's contents.
func fileRows(t *testing.T, table *SqlTable) [][]any {
	t.Helper()
	rows := make([][]any, 0)
	for row, err := range table.file.Scan() {
		require.NoError(t, err)
		decoded, err := DecodeRow(table.Columns, row.Bytes)
		require.NoError(t, err)
		rows = append(rows, decoded)
	}
	return rows
}

// assertTableRows checks the table holds exactly want, in file order: insertion
// order, except that an updated row has moved to the end.
func assertTableRows(t *testing.T, table *SqlTable, want ...[]any) {
	t.Helper()
	if want == nil {
		want = [][]any{}
	}
	assert.Equal(t, want, fileRows(t, table))
}

// assertTableRowsAnyOrder checks the table holds exactly want, in any order.
// An UPDATE writes the new version of a row at the end of the file, so after
// one the order is unspecified.
func assertTableRowsAnyOrder(t *testing.T, table *SqlTable, want ...[]any) {
	t.Helper()
	assert.ElementsMatch(t, want, fileRows(t, table))
}
```

(That is: delete the existing `fileRows` and `assertMirrored` and put the three functions above in their place; keep every other helper in the file.)

- [ ] **Step 2: Rewrite the assertions in the new test files**

Create a scratch script and run it from the repository root (the script asserts that every replacement matches exactly once, so a mismatch stops it instead of silently skipping):

```bash
python3 - <<'EOF'
import re

def func_bounds(src, name):
    m = re.search(r'^func ' + re.escape(name) + r'\(', src, re.M)
    assert m, "no such function: " + name
    start = m.start()
    end = src.index('\n}\n', start) + 3
    return start, end

def comment_start(src, start):
    # include the contiguous // comment lines directly above the function
    lines_before = src[:start].split('\n')
    i = len(lines_before) - 1  # the empty string after the last newline
    while i - 1 >= 0 and lines_before[i - 1].startswith('//'):
        i -= 1
    return len('\n'.join(lines_before[:i])) + (1 if i > 0 else 0)

def edit(path, name, pairs):
    src = open(path).read()
    s, e = func_bounds(src, name)
    body = src[s:e]
    for old, new in pairs:
        assert body.count(old) == 1, (path, name, old[:60], body.count(old))
        body = body.replace(old, new)
    open(path, 'w').write(src[:s] + body + src[e:])

def remove(path, name):
    src = open(path).read()
    s, e = func_bounds(src, name)
    cs = comment_start(src, s)
    rest = src[e:]
    if rest.startswith('\n'):
        rest = rest[1:]
    open(path, 'w').write(src[:cs] + rest)

D = 'internal/engine/'

# ---- insert_file_test.go
p = D + 'insert_file_test.go'
edit(p, 'TestTable_InsertValues_MirrorsRowsToFile', [(
 '\tassert.Len(t, table.Rows, 3)\n\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob"},\n\t\t[]any{int64(2), "sam"},\n\t\t[]any{int64(3), "amy"},\n\t)\n')])
edit(p, 'TestTable_InsertValues_MirrorsNullAndEmptyStringDistinctly', [(
 '\tassertMirrored(t, table)\n\tonDisk', '\tonDisk')])
edit(p, 'TestTable_InsertValues_MirrorsAcrossManyPages', [(
 '\tassertMirrored(t, table)\n',
 '\tonDisk := fileRows(t, table)\n\trequire.Len(t, onDisk, n)\n\tassert.Equal(t, []any{int64(0), name}, onDisk[0])\n\tassert.Equal(t, []any{int64(n - 1), name}, onDisk[n-1])\n')])
edit(p, 'TestTable_InsertValues_RejectedBatchWritesNothingToFile', [(
 '\tassert.Empty(t, table.Rows)\n\tassert.Empty(t, fileRows(t, table))\n',
 '\tassert.Empty(t, fileRows(t, table))\n')])
edit(p, 'TestTable_InsertValues_OversizedRowIsRejectedBeforeAnythingIsWritten', [(
 '\tassert.Empty(t, table.Rows, "the batch is all-or-nothing")\n\tassert.Empty(t, fileRows(t, table), "the row that did fit was not written either")\n',
 '\tassert.Empty(t, fileRows(t, table), "the batch is all-or-nothing: the row that did fit was not written either")\n')])
edit(p, 'TestTable_InsertValues_LargestRowThatFitsIsStored', [(
 '\tassertMirrored(t, table)\n',
 '\tonDisk := fileRows(t, table)\n\trequire.Len(t, onDisk, 1)\n\tassert.Equal(t, bio, onDisk[0][2])\n')])
edit(p, 'TestTable_InsertValues_OnAClosedTableFails', [('\tassert.Empty(t, table.Rows)\n', '')])
edit(p, 'TestTable_InsertValues_TableWithNoColumns', [(
 '\tassert.Len(t, table.Rows, 2)\n\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table, []any{}, []any{})\n')])

# ---- alter_file_test.go
p = D + 'alter_file_test.go'
edit(p, 'TestTable_AddColumn_RewritesTheFileInTheNewSchema', [(
 '\tassert.Equal(t, []any{int64(1), "bob", int64(18)}, table.Rows[0])\n\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob", int64(18)},\n\t\t[]any{int64(2), "sam", int64(18)},\n\t)\n')])
edit(p, 'TestTable_AddColumn_NullableColumnBackfillsNull', [(
 '\tassertMirrored(t, table)\n\tassert.Equal(t, []any{int64(1), "bob", nil}, fileRows(t, table)[0])\n',
 '\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob", nil},\n\t\t[]any{int64(2), "sam", nil},\n\t)\n')])
edit(p, 'TestTable_DropColumn_RewritesTheFileInTheNewSchema', [(
 '\tassert.Equal(t, []any{int64(1), int64(30)}, table.Rows[0])\n\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table, []any{int64(1), int64(30)}, []any{int64(2), int64(40)})\n')])
edit(p, 'TestTable_RenameColumn_LeavesTheFileAlone', [(
 '\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table, []any{int64(1), "bob"}, []any{int64(2), "sam"})\n')])
edit(p, 'TestTable_Alter_RejectedAlterLeavesFileSchemaAndRowsUntouched', [
 ('\t\t\twantRows := append([][]any(nil), table.Rows...)\n', '\t\t\twantRows := fileRows(t, table)\n'),
 ('\t\t\tassert.Equal(t, wantRows, table.Rows)\n\t\t\tassertMirrored(t, table)\n', '\t\t\tassert.Equal(t, wantRows, fileRows(t, table))\n')])
edit(p, 'TestTable_DropColumn_TheOnlyColumnIsRejectedWithoutTouchingTheFile', [(
 '\tassertMirrored(t, table)\n', '\tassertTableRows(t, table, []any{int64(1)})\n')])
edit(p, 'TestTable_AddColumn_ThatMakesARowTooLargeIsRejected', [(
 '\tassertMirrored(t, table)\n',
 '\tassert.Len(t, fileRows(t, table), 1, "the row is still in the file, in the old layout")\n')])
edit(p, 'TestTable_Alter_OnAnEmptyTable', [(
 '\tassert.Empty(t, table.Rows)\n\tassertMirrored(t, table)\n', '\tassertTableRows(t, table)\n')])
edit(p, 'TestTable_Alter_ManyPages', [(
 '\tassertMirrored(t, table)\n',
 '\tonDisk := fileRows(t, table)\n\trequire.Len(t, onDisk, n)\n\tassert.Equal(t, []any{int64(0), name, int64(18)}, onDisk[0])\n')])
edit(p, 'TestTable_Alter_InsertAfterAlterMirrorsInTheNewLayout', [(
 '\tassertMirrored(t, table)\n\tassert.Equal(t, []any{int64(3), "amy", int64(50)}, fileRows(t, table)[2])\n',
 '\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob", int64(18)},\n\t\t[]any{int64(2), "sam", int64(18)},\n\t\t[]any{int64(3), "amy", int64(50)},\n\t)\n')])
remove(p, 'TestTable_Alter_ResyncsAFileThatDriftedFromRows')

# ---- delete_file_test.go
p = D + 'delete_file_test.go'
edit(p, 'TestTable_Delete_MirrorsToFile', [(
 '\tassert.Len(t, table.Rows, 3)\n\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob"},\n\t\t[]any{int64(3), "amy"},\n\t\t[]any{int64(4), "lee"},\n\t)\n')])
edit(p, 'TestTable_Delete_WithoutWhereEmptiesTheFileAndItStaysUsable', [
 ('\tassert.Empty(t, table.Rows)\n\tassert.Empty(t, fileRows(t, table))\n', '\tassert.Empty(t, fileRows(t, table))\n'),
 ('\tassertMirrored(t, table)\n\tassert.Len(t, table.Rows, 1)\n', '\tassertTableRows(t, table, []any{int64(9), "zed"})\n')])
edit(p, 'TestTable_Delete_NoMatchLeavesTheFileAlone', [(
 '\tassert.Len(t, table.Rows, 4)\n\tassert.Len(t, fileRows(t, table), 4)\n\tassertMirrored(t, table)\n',
 '\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob"},\n\t\t[]any{int64(2), "sam"},\n\t\t[]any{int64(3), "amy"},\n\t\t[]any{int64(4), "lee"},\n\t)\n')])
edit(p, 'TestTable_Delete_AcrossManyPages', [(
 '\tassert.Len(t, table.Rows, n-700)\n\tassertMirrored(t, table)\n',
 '\tonDisk := fileRows(t, table)\n\trequire.Len(t, onDisk, n-700)\n\tassert.Equal(t, int64(700), onDisk[0][0], "the survivors keep their order")\n')])
edit(p, 'TestTable_Delete_FailedWhereLeavesFileAndRowsUntouched', [
 ('\t\t\twantRows := append([][]any(nil), table.Rows...)\n', '\t\t\twantRows := fileRows(t, table)\n'),
 ('\t\t\tassert.Equal(t, wantRows, table.Rows)\n\t\t\tassertMirrored(t, table)\n', '\t\t\tassert.Equal(t, wantRows, fileRows(t, table))\n')])
edit(p, 'TestTable_Delete_NullNeverMatches', [(
 '\trequire.Len(t, table.Rows, 1)\n\tassert.Equal(t, int64(1), table.Rows[0][0])\n\tassertMirrored(t, table)\n',
 '\tonDisk := fileRows(t, table)\n\trequire.Len(t, onDisk, 1)\n\tassert.Equal(t, int64(1), onDisk[0][0])\n')])
edit(p, 'TestTable_Delete_OnAClosedTableFails', [(
 '\tassert.Len(t, table.Rows, 4, "Rows is only committed after the file step succeeds")\n', '')])
edit(p, 'TestTable_Delete_ThenInsertMirrorsInOrder', [(
 '\tassertMirrored(t, table)\n\tids := []int64{}\n', '\tids := []int64{}\n')])
edit(p, 'TestTable_Delete_IsOnDiskOnceItReturns', [(
 '\tassert.Equal(t, table.Rows, onDisk)\n', '\tassert.Equal(t, fileRows(t, table), onDisk)\n')])
remove(p, 'TestTable_Delete_ToleratesAFileThatDriftedFromRows')

# ---- update_file_test.go
p = D + 'update_file_test.go'
remove(p, 'assertMirroredAsSet')
edit(p, 'TestTable_Update_MirrorsToFile', [(
 '\tassert.Equal(t, []any{int64(2), "zed"}, table.Rows[1])\n\tassertMirroredAsSet(t, table)\n\tassert.Len(t, fileRows(t, table), 4, "the old version is tombstoned, not left beside the new one")\n',
 '\t// Exactly four rows: the old version is tombstoned, not left beside the new one.\n\tassertTableRowsAnyOrder(t, table,\n\t\t[]any{int64(1), "bob"},\n\t\t[]any{int64(2), "zed"},\n\t\t[]any{int64(3), "amy"},\n\t\t[]any{int64(4), "lee"},\n\t)\n')])
edit(p, 'TestTable_Update_WithoutWhereUpdatesEveryRow', [(
 '\tfor _, row := range table.Rows {\n\t\tassert.Equal(t, "x", row[1])\n\t}\n\tassertMirroredAsSet(t, table)\n\tassert.Len(t, fileRows(t, table), 4)\n',
 '\tassertTableRowsAnyOrder(t, table,\n\t\t[]any{int64(1), "x"},\n\t\t[]any{int64(2), "x"},\n\t\t[]any{int64(3), "x"},\n\t\t[]any{int64(4), "x"},\n\t)\n')])
edit(p, 'TestTable_Update_MultipleAssignmentsMirror', [(
 '\tassert.Equal(t, []any{int64(1), "z", int64(99)}, table.Rows[0])\n\tassertMirroredAsSet(t, table)\n',
 '\tassertTableRowsAnyOrder(t, table,\n\t\t[]any{int64(1), "z", int64(99)},\n\t\t[]any{int64(2), "sam", int64(40)},\n\t)\n')])
edit(p, 'TestTable_Update_NoMatchLeavesTheFileAlone', [(
 '\tassertMirrored(t, table) // nothing moved, so even the order is unchanged\n',
 '\t// Nothing moved, so even the order is unchanged.\n\tassertTableRows(t, table,\n\t\t[]any{int64(1), "bob"},\n\t\t[]any{int64(2), "sam"},\n\t\t[]any{int64(3), "amy"},\n\t\t[]any{int64(4), "lee"},\n\t)\n')])
edit(p, 'TestTable_Update_AcrossManyPages', [(
 '\tassert.Len(t, table.Rows, n)\n\tassertMirroredAsSet(t, table)\n\tassert.Len(t, fileRows(t, table), n)\n',
 '\tonDisk := fileRows(t, table)\n\trequire.Len(t, onDisk, n)\n\tupdated := 0\n\tfor _, row := range onDisk {\n\t\tif row[1] == "m" {\n\t\t\tupdated++\n\t\t}\n\t}\n\tassert.Equal(t, 700, updated)\n')])
edit(p, 'TestTable_Update_FailedUpdateLeavesFileAndRowsUntouched', [
 ('\t\t\twantRows := append([][]any(nil), table.Rows...)\n', '\t\t\twantRows := fileRows(t, table)\n'),
 ('\t\t\tassert.Equal(t, wantRows, table.Rows)\n\t\t\tassertMirrored(t, table) // untouched, so even the order is unchanged\n',
  '\t\t\tassert.Equal(t, wantRows, fileRows(t, table)) // untouched, so even the order is unchanged\n')])
edit(p, 'TestTable_Update_RowThatGrowsPastAPageIsRejectedAtomically', [
 ('\twantRows := append([][]any(nil), table.Rows...)\n', '\twantRows := fileRows(t, table)\n'),
 ('\tassert.Equal(t, wantRows, table.Rows)\n\tassertMirrored(t, table)\n', '\tassert.Equal(t, wantRows, fileRows(t, table))\n')])
edit(p, 'TestTable_Update_OnAClosedTableFails', [
 ('\twantRows := append([][]any(nil), table.Rows...)\n', ''),
 ('\tassert.Equal(t, wantRows, table.Rows, "Rows is only committed after the file step succeeds")\n', '')])
edit(p, 'TestTable_Update_IsOnDiskOnceItReturns', [(
 '\tassert.ElementsMatch(t, table.Rows, onDisk)\n', '\tassert.ElementsMatch(t, fileRows(t, table), onDisk)\n')])
edit(p, 'TestTable_Update_ThenDeleteThenInsertStayMirrored', [(
 '\tassert.Len(t, table.Rows, 3) // ids 3, 4 and 5\n\tassertMirroredAsSet(t, table)\n',
 '\tassertTableRowsAnyOrder(t, table,\n\t\t[]any{int64(3), "amy"},\n\t\t[]any{int64(4), "lee"},\n\t\t[]any{int64(5), "kay"},\n\t)\n')])
edit(p, 'TestTable_Update_UnassignedNullColumnsStayNull', [(
 '\tassert.Equal(t, []any{int64(1), "zed", nil}, table.Rows[0])\n\tassertMirroredAsSet(t, table)\n\tassert.Equal(t, []any{int64(1), "zed", nil}, fileRows(t, table)[0])\n',
 '\tassertTableRows(t, table, []any{int64(1), "zed", nil})\n')])
remove(p, 'TestTable_Update_ToleratesAFileThatDriftedFromRows')

# ---- select_file_test.go
p = D + 'select_file_test.go'
remove(p, 'stringRows')
remove(p, 'TestTable_Select_ReadsTheFileNotRows')
edit(p, 'TestTable_Select_SeesEveryMigratedStatement', [(
 '\tassert.ElementsMatch(t, stringRows(table.Rows), resp.Rows, "the file answers exactly what Rows holds")\n', '')])
edit(p, 'TestTable_Select_ConcurrentWithWriters', [(
 '\tassertMirrored(t, table)\n',
 '\tseen := make(map[int64]bool)\n\tfor _, row := range fileRows(t, table) {\n\t\tseen[row[0].(int64)] = true\n\t}\n\tassert.Len(t, seen, writers*perWriter, "every insert is in the file exactly once")\n')])

# ---- table_test.go: the pre-existing tests read Rows by position
p = D + 'table_test.go'
src = open(p).read()
old = ('\tassert.Equal(t, []any{int64(1), "robert"}, table.Rows[0])\n'
       '\tassert.Equal(t, []any{int64(2), "sam"}, table.Rows[1], "unmatched rows must be untouched")\n')
new = ('\t// The updated row is rewritten at the end of the file, so the order is unspecified.\n'
       '\tassert.ElementsMatch(t, [][]any{{int64(1), "robert"}, {int64(2), "sam"}}, fileRows(t, table),\n'
       '\t\t"the matched row is updated and the unmatched row untouched")\n')
assert src.count(old) == 1
src = src.replace(old, new)
src = src.replace('table.Rows', 'fileRows(t, table)')
open(p, 'w').write(src)
print("edits applied")
EOF
gofmt -w internal/engine/*_test.go; go vet ./internal/engine
```

Expected: `edits applied`, then `go vet` clean. If `go vet` reports an unused import in a test file (for example `strconv` or `strings` after a helper was removed), delete that import.

- [ ] **Step 3: Verify no test touches `Rows` any more**

Run: `grep -n 'table\.Rows\|\.Rows\[\|assertMirrored\|stringRows' internal/engine/*_test.go | grep -v 'resp\.Rows\|eq\.Rows\|neq\.Rows\|all\.Rows\|late\.Rows\|rows\.Rows'`
Expected: no output.

- [ ] **Step 4: Run the whole suite**

Run: `gofmt -l .; go vet ./... && go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`. If a test fails only because an updated row moved to the end of the file, change that assertion to `assertTableRowsAnyOrder`/`ElementsMatch` and ledger it; any other failure means a replacement above was wrong, so fix the replacement.

- [ ] **Step 5: Mutation-check that the tests still bite**

Production code is unchanged, so check a few real bugs are still caught by the migrated tests. In `internal/engine/table.go`, one at a time, run `go test -count=1 ./internal/engine`, confirm a failure, then restore:
1. In `deleteFromFile`, tombstone only the first match (`matched = matched[:1]` when it has more than one). Expect `Delete` tests to fail.
2. In `updateFile`, skip the tombstone loop. Expect `Update` tests to fail (duplicate versions).
3. In `newFileHolding`, drop the last row from `rows` before writing. Expect `ALTER` tests to fail.

- [ ] **Step 6: Commit**

```bash
git add internal/engine
git status --short   # only internal/engine test files
git commit -m "test(engine): assert on the table's file instead of in-memory rows

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: File-only statements; delete `Rows`

**Files:**
- Modify: `internal/engine/table.go`
- Create: `internal/engine/uniqueness_test.go`
- Create (temporary, deleted in Step 6): `internal/engine/rows_removal_red_test.go`
- Modify: `CLAUDE.md`, `plan/on-disk-persistence-design.md` (local only)

**Interfaces:**
- Consumes (Task 1): `assertTableRows`, `assertTableRowsAnyOrder`, `fileRows`; earlier PRs: `usersTable`, `uniqueUsersTable`, `fourUsers`, `setTo`, `ageColumn`, `cmp`, `numTok`, `strTok`, `identTok`, `tokRow`, `storage.File` (`Scan`, `Insert`, `Delete`, `Sync`), `encodeRows`, `createTableFile`, `discardFile`, `adoptFile`, `deleteFromFile`, `appendToFile`.
- Produces (all in `table.go`):
  - `func (t *SqlTable) buildRow(row []parser.Token, targetColumns []string, targetIndices []int, fillers map[int]any) ([]any, error)`
  - `func (t *SqlTable) checkUniqueness(newRows [][]any, uniqueColumns []int) error`
  - `func (t *SqlTable) existingKeys(candidates map[int]map[any]bool) (map[int]map[any]bool, error)`
  - `func (t *SqlTable) countRows(limit int) (int, error)`
  - `func (t *SqlTable) rebuildFile(columns []parser.ColumnDefinition, transform func(row []any) []any) (string, storage.File, error)` (replaces `newFileHolding`)
  - `func encodeRowChecked(columns []parser.ColumnDefinition, row []any) ([]byte, error)`
  - `SqlTable` without a `Rows` field.

- [ ] **Step 1: Write the permanent precedence and many-page tests**

Create `internal/engine/uniqueness_test.go`:

```go
package engine

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// manyUniqueUsers fills a uniqueUsersTable (id PRIMARY KEY, email UNIQUE) with
// n rows, enough to span several 16KiB pages.
func manyUniqueUsers(t *testing.T, n int) *SqlTable {
	t.Helper()
	table := uniqueUsersTable(t)
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok("user"+strconv.Itoa(i)+"@example.com"))
	}
	require.NoError(t, table.InsertValues(nil, rows))
	return table
}

func TestTable_InsertValues_DuplicateOnALatePage(t *testing.T) {
	table := manyUniqueUsers(t, 1500)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1499"), strTok("fresh@example.com"))})
	assert.EqualError(t, err, `duplicate entry 1499 for column "id"`)

	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1500"), strTok("user7@example.com"))})
	assert.EqualError(t, err, `duplicate entry user7@example.com for column "email"`)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1500"), strTok("fresh@example.com"))}))
	assert.Len(t, fileRows(t, table), 1501)
}

// An earlier row's duplicate key has always been reported before a later row's
// error, because rows were checked one at a time.
func TestTable_InsertValues_DuplicateBeatsALaterRowError(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("z@x.com")), // duplicates id 1
		tokRow(strTok("two"), strTok("y@x.com")), // a string where an INT belongs
	})

	assert.EqualError(t, err, `duplicate entry 1 for column "id"`)
}

// ...and an earlier row's error has always been reported before a later row's
// duplicate key.
func TestTable_InsertValues_CoercionErrorBeatsALaterDuplicate(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(strTok("two"), strTok("y@x.com")), // a string where an INT belongs
		tokRow(numTok("1"), strTok("z@x.com")),   // duplicates id 1
	})

	assert.EqualError(t, err, `column "id": expected a numeric value, got two`)
}

func TestTable_Update_UniqueConflictReportsTheFirstConflictingColumn(t *testing.T) {
	// Row 1 would collide with row 3 on id and with row 2 on email.
	setup := func(t *testing.T) *SqlTable {
		table := uniqueUsersTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{
			tokRow(numTok("1"), strTok("a@x.com")),
			tokRow(numTok("2"), strTok("b@x.com")),
			tokRow(numTok("3"), strTok("c@x.com")),
		}))
		return table
	}
	only1 := cmp(identTok("id"), parser.EQ, numTok("1"))

	err := setup(t).Update([]parser.Assignment{setTo("email", strTok("b@x.com")), setTo("id", numTok("3"))}, only1)
	assert.EqualError(t, err, `duplicate entry b@x.com for column "email"`, "email is assigned first")

	err = setup(t).Update([]parser.Assignment{setTo("id", numTok("3")), setTo("email", strTok("b@x.com"))}, only1)
	assert.EqualError(t, err, `duplicate entry 3 for column "id"`, "id is assigned first")
}

// Two matched rows would both receive the assigned value; the row-by-row check
// reported it on the first unique column in assignment order.
func TestTable_Update_SeveralMatchesReportTheFirstUniqueColumn(t *testing.T) {
	setup := func(t *testing.T) *SqlTable {
		table := uniqueUsersTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{
			tokRow(numTok("1"), strTok("a@x.com")),
			tokRow(numTok("2"), strTok("b@x.com")),
		}))
		return table
	}

	err := setup(t).Update([]parser.Assignment{setTo("id", numTok("5")), setTo("email", strTok("q@x.com"))}, nil)
	assert.EqualError(t, err, `duplicate entry 5 for column "id"`)

	err = setup(t).Update([]parser.Assignment{setTo("email", strTok("q@x.com")), setTo("id", numTok("5"))}, nil)
	assert.EqualError(t, err, `duplicate entry q@x.com for column "email"`)
}

func TestTable_Update_UniquenessAcrossManyPages(t *testing.T) {
	table := manyUniqueUsers(t, 1500)
	row0 := cmp(identTok("id"), parser.EQ, numTok("0"))

	// The only row holding this email sits on the last page.
	err := table.Update([]parser.Assignment{setTo("email", strTok("user1499@example.com"))}, row0)
	assert.EqualError(t, err, `duplicate entry user1499@example.com for column "email"`)

	require.NoError(t, table.Update([]parser.Assignment{setTo("email", strTok("fresh@example.com"))}, row0))
	assert.Len(t, fileRows(t, table), 1500)
}

// One value backfilled onto fewer than two rows holds no key twice, so a UNIQUE
// column with a default is accepted on an empty table and on a one-row table;
// two rows are rejected by TestTable_AlterColumns_AddColumn_UniqueWithDefault
// RejectedOnMultipleRows.
func TestTable_AddColumn_UniqueDefaultIsAcceptedOnFewerThanTwoRows(t *testing.T) {
	region := func() parser.ColumnDefinition {
		return parser.ColumnDefinition{
			Name:     "region",
			DataType: parser.VarCharDataType{Size: 10},
			Constraints: []parser.Constraint{
				parser.UniqueConstraint{},
				parser.DefaultConstraint{DefaultValue: strTok("us")},
			},
		}
	}

	empty := usersTable(t)
	require.NoError(t, empty.AlterColumns(parser.AddColumnAction{Column: region()}))
	assert.Len(t, empty.Columns, 3)

	one := usersTable(t)
	require.NoError(t, one.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))
	require.NoError(t, one.AlterColumns(parser.AddColumnAction{Column: region()}))
	assertTableRows(t, one, []any{int64(1), "bob", "us"})
}
```

- [ ] **Step 2: Write the temporary white-box tests that drive the rewrite**

These empty `Rows` first, so they fail while the code still reads it and pass once it does not. Create `internal/engine/rows_removal_red_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// TEMPORARY: these drive the removal of Rows and are deleted with it, because
// they cannot compile once the field is gone.

func TestRowsRemoval_InsertChecksUniquenessAgainstTheFile(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))
	table.Rows = nil

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("b@x.com"))})

	assert.EqualError(t, err, `duplicate entry 1 for column "id"`)
}

func TestRowsRemoval_UpdateChecksUniquenessAgainstTheFile(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("a@x.com")),
		tokRow(numTok("2"), strTok("b@x.com")),
	}))
	table.Rows = nil

	err := table.Update([]parser.Assignment{setTo("email", strTok("b@x.com"))}, cmp(identTok("id"), parser.EQ, numTok("1")))

	assert.EqualError(t, err, `duplicate entry b@x.com for column "email"`)
}

func TestRowsRemoval_AddColumnRebuildsFromTheFile(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	table.Rows = nil

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	assertTableRows(t, table,
		[]any{int64(1), "bob", int64(18)},
		[]any{int64(2), "sam", int64(18)},
	)
}

func TestRowsRemoval_DropColumnRebuildsFromTheFile(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	table.Rows = nil

	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	assertTableRows(t, table, []any{int64(1)}, []any{int64(2)})
}
```

- [ ] **Step 3: Run the new tests to see which fail**

Run: `go test -count=1 ./internal/engine -run 'Test(Table_(InsertValues_(DuplicateOnALatePage|DuplicateBeatsALater|CoercionErrorBeatsALater)|Update_(UniqueConflictReports|SeveralMatchesReport|UniquenessAcrossManyPages)|AddColumn_UniqueDefaultIsAccepted)|RowsRemoval_)' -v 2>&1 | grep -E '^(--- |FAIL|ok|panic)'`
Expected: the four `RowsRemoval_*` tests FAIL (the code still reads `Rows`: no duplicate error, and the rebuilt file is empty); the seven new `uniqueness_test.go` tests PASS even before the change (they pin behaviour the rewrite must preserve).

- [ ] **Step 4: Rewrite the statements and delete `Rows`**

Run this from the repository root. It replaces whole functions in `internal/engine/table.go` (each replacement includes the function's doc comment) and asserts every target exists exactly once:

```bash
python3 - <<'PYEOF'
import re
P = 'internal/engine/table.go'
src = open(P).read()

def span(name, recv=True):
    pat = r'^func ' + (r'\(t \*SqlTable\) ' if recv else '') + re.escape(name) + r'\('
    m = re.search(pat, src, re.M)
    assert m, name
    start = m.start()
    end = src.index('\n}\n', start) + 3
    # include the contiguous // comment block directly above
    before = src[:start].split('\n')
    i = len(before) - 1
    while i - 1 >= 0 and before[i - 1].startswith('//'):
        i -= 1
    cstart = len('\n'.join(before[:i])) + (1 if i > 0 else 0)
    return cstart, end

def replace_func(name, new, recv=True):
    global src
    s, e = span(name, recv)
    src = src[:s] + new + src[e:]

def remove_func(name, recv=True):
    global src
    s, e = span(name, recv)
    rest = src[e:]
    if rest.startswith('\n'):
        rest = rest[1:]
    src = src[:s] + rest

def sub(old, new):
    global src
    assert src.count(old) == 1, (old[:60], src.count(old))
    src = src.replace(old, new)

# --- NewTable no longer fills Rows. The field itself stays declared (and nil)
# until Step 6, so the temporary white-box tests can run against the new code.
sub("Name: name, Columns: columns, Rows: make([][]any, 0),\n", "Name: name, Columns: columns,\n")

# --- INSERT
replace_func('InsertValues', '''// InsertValues appends rows to the table's file, coercing each value against
// its column's DataType and enforcing NOT NULL/PRIMARY KEY/UNIQUE.
func (t *SqlTable) InsertValues(columns []string, values [][]parser.Token) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Phase 1: resolve the statement-level mapping once — identical for
	// every row in this batch, so it's wasted work (and a correctness
	// risk) to redo it per row.
	targetColumns := columns
	if len(targetColumns) == 0 {
		targetColumns = make([]string, len(t.Columns))
		for i, col := range t.Columns {
			targetColumns[i] = col.Name
		}
	}

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	targetIndices := make([]int, len(targetColumns))
	provided := make([]bool, len(t.Columns))
	seen := make(map[string]bool, len(targetColumns))
	for i, name := range targetColumns {
		idx, ok := columnIndex[name]
		if !ok {
			return fmt.Errorf("unknown column %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate column %q in column list", name)
		}
		seen[name] = true
		targetIndices[i] = idx
		provided[idx] = true
	}

	// Fillers for columns not covered by the insert's column list, resolved
	// once — safe to reuse across rows because DefaultConstraint.DefaultValue
	// is a static literal Token here, not a dynamic expression. Coerced
	// once here too, for the same reason.
	fillers := make(map[int]any)
	for i, col := range t.Columns {
		if provided[i] {
			continue
		}
		if def, ok := col.DefaultValue(); ok {
			coerced, err := coerceValue(col.DataType, def)
			if err != nil {
				return fmt.Errorf("column %q: %v", col.Name, err)
			}
			fillers[i] = coerced
			continue
		}
		if col.RequiresValue() {
			return fmt.Errorf("field %q doesn't have a default value", col.Name)
		}
		fillers[i] = nil
	}

	uniqueColumns := make([]int, 0)
	for i, col := range t.Columns {
		if col.IsPrimaryKey() || col.IsUnique() {
			uniqueColumns = append(uniqueColumns, i)
		}
	}

	// Phase 2: build every row into a fresh buffer, stopping at the first row
	// that cannot be built. That error is held back rather than returned: a
	// duplicate key in an earlier row has always been reported before a later
	// row's error, because rows used to be checked one at a time, and still is.
	newRows := make([][]any, 0, len(values))
	var rowErr error
	for _, row := range values {
		rowValues, err := t.buildRow(row, targetColumns, targetIndices, fillers)
		if err != nil {
			rowErr = err
			break
		}
		newRows = append(newRows, rowValues)
	}

	// Phase 3: PRIMARY KEY/UNIQUE, against the rows already in the file and
	// against earlier rows of this batch.
	if err := t.checkUniqueness(newRows, uniqueColumns); err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}

	// Phase 4: write the whole batch; nothing is written unless every row is
	// good, so a failure never leaves the table half-inserted.
	return t.appendToFile(newRows)
}

// buildRow validates one row of an INSERT, coerces its values against their
// columns' DataTypes, and fills in the columns the insert did not name.
func (t *SqlTable) buildRow(row []parser.Token, targetColumns []string, targetIndices []int, fillers map[int]any) ([]any, error) {
	if len(row) != len(targetColumns) {
		return nil, fmt.Errorf("expected %d values, got %d", len(targetColumns), len(row))
	}

	rowValues := make([]any, len(t.Columns))
	for j, tok := range row {
		idx := targetIndices[j]
		coerced, err := coerceValue(t.Columns[idx].DataType, tok)
		if err != nil {
			return nil, fmt.Errorf("column %q: %v", t.Columns[idx].Name, err)
		}
		rowValues[idx] = coerced
	}
	for idx, filler := range fillers {
		rowValues[idx] = filler // already coerced once, in phase 1
	}
	return rowValues, nil
}

// checkUniqueness reports the first PRIMARY KEY/UNIQUE violation among
// newRows, checked in row order against the rows already in the table's file
// and against the earlier rows of the batch. Only the key values newRows
// insert can conflict with an existing row, so those are collected first and
// the file is streamed once against them: memory is proportional to the
// statement, not the table. NULLs never conflict.
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

	taken, err := t.existingKeys(candidates)
	if err != nil {
		return err
	}

	for _, row := range newRows {
		for _, idx := range uniqueColumns {
			value := row[idx]
			if value == nil {
				continue // multiple NULLs are allowed in a UNIQUE column
			}
			if taken[idx][value] {
				return fmt.Errorf("duplicate entry %v for column %q", value, t.Columns[idx].Name)
			}
			taken[idx][value] = true
		}
	}
	return nil
}

// existingKeys streams the table's file once and returns, per unique column,
// which of the candidate values some existing row already holds.
func (t *SqlTable) existingKeys(candidates map[int]map[any]bool) (map[int]map[any]bool, error) {
	if t.file == nil {
		return nil, fmt.Errorf("table %q is closed", t.Name)
	}

	found := make(map[int]map[any]bool, len(candidates))
	for idx := range candidates {
		found[idx] = make(map[any]bool)
	}
	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return nil, err
		}
		for idx, wanted := range candidates {
			if v := row[idx]; v != nil && wanted[v] {
				found[idx][v] = true
			}
		}
	}
	return found, nil
}
''')

# --- shared encoding
replace_func('encodeRows', '''// encodeRows encodes every row with columns and rejects any that cannot fit
// on a page, before the caller writes anything.
func encodeRows(columns []parser.ColumnDefinition, rows [][]any) ([][]byte, error) {
	encoded := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := encodeRowChecked(columns, row)
		if err != nil {
			return nil, err
		}
		encoded[i] = b
	}
	return encoded, nil
}

// encodeRowChecked encodes one row and rejects it if it cannot fit on a page.
func encodeRowChecked(columns []parser.ColumnDefinition, row []any) ([]byte, error) {
	b, err := EncodeRow(columns, row)
	if err != nil {
		return nil, err
	}
	if len(b) > storage.MaxRowSize {
		return nil, fmt.Errorf("row too large: %d bytes exceeds the %d-byte limit", len(b), storage.MaxRowSize)
	}
	return b, nil
}
''', recv=False)

# --- ALTER: stream the file into the replacement
replace_func('newFileHolding', '''// rebuildFile builds a replacement for the table's file: a new file in the
// table's database directory holding every live row of the current file, each
// passed through transform and encoded with columns, then fsynced. Rows are
// streamed one at a time, so memory does not grow with the table. Nothing
// about the table changes, and on any failure the partial file is removed.
// Callers hold t.mu, and t.Columns must still be the schema of the current
// file, which is what its rows are decoded with.
func (t *SqlTable) rebuildFile(columns []parser.ColumnDefinition, transform func(row []any) []any) (string, storage.File, error) {
	if t.file == nil {
		return "", nil, fmt.Errorf("table %q is closed", t.Name)
	}

	path, file, err := createTableFile(t.files, t.database)
	if err != nil {
		return "", nil, err
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			discardFile(path, file)
			return "", nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			discardFile(path, file)
			return "", nil, err
		}
		encoded, err := encodeRowChecked(columns, transform(row))
		if err != nil {
			discardFile(path, file)
			return "", nil, err
		}
		if _, err := file.Insert(encoded); err != nil {
			discardFile(path, file)
			return "", nil, err
		}
	}
	if err := file.Sync(); err != nil {
		discardFile(path, file)
		return "", nil, err
	}
	return path, file, nil
}

// countRows counts the live rows of the table's file, stopping once it has
// seen limit of them.
func (t *SqlTable) countRows(limit int) (int, error) {
	if t.file == nil {
		return 0, fmt.Errorf("table %q is closed", t.Name)
	}

	n := 0
	for _, err := range t.file.Scan() {
		if err != nil {
			return 0, err
		}
		n++
		if n >= limit {
			break
		}
	}
	return n, nil
}
''')
sub("// already committed the new Columns and Rows and hold t.mu: if the old file",
    "// already committed the new Columns and hold t.mu: if the old file")

replace_func('addColumn', '''// addColumn appends column to the schema and backfills every existing
// row with its value, streaming the file into a replacement in the new
// layout. Reuses the exact same coercion/uniqueness rules InsertValues
// already enforces: a NOT NULL column with no default, or a
// PRIMARY KEY/UNIQUE column with a default backfilled onto 2+ rows,
// fails via the same checks INSERT uses — no new special-casing needed.
func (t *SqlTable) addColumn(column parser.ColumnDefinition) error {
	if err := validateNewColumn(t.Name, t.Columns, column); err != nil {
		return err
	}

	var filler any
	if def, ok := column.DefaultValue(); ok {
		coerced, err := coerceValue(column.DataType, def)
		if err != nil {
			return fmt.Errorf("column %q: %v", column.Name, err)
		}
		filler = coerced
	} else if column.RequiresValue() {
		return fmt.Errorf("field %q doesn't have a default value", column.Name)
	}

	// One value backfilled onto two or more rows would hold the same key twice.
	if (column.IsPrimaryKey() || column.IsUnique()) && filler != nil {
		rows, err := t.countRows(2)
		if err != nil {
			return err
		}
		if rows >= 2 {
			return fmt.Errorf("duplicate entry %v for column %q", filler, column.Name)
		}
	}

	// Build the replacement file before touching the table, so a failure
	// (a row that no longer fits on a page, an I/O error) changes nothing.
	newColumns := append(append([]parser.ColumnDefinition{}, t.Columns...), column)
	path, file, err := t.rebuildFile(newColumns, func(row []any) []any {
		return append(row, filler)
	})
	if err != nil {
		return err
	}

	t.Columns = newColumns
	return t.adoptFile(path, file)
}
''')

replace_func('dropColumn', '''// dropColumn removes column at its schema index by streaming the file
// into a replacement without that column's value in any row — the schema
// and every stored row must stay aligned, or every subsequent read of a
// row would be reading the wrong column's value.
func (t *SqlTable) dropColumn(name string) error {
	idx := -1
	for i, col := range t.Columns {
		if col.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("unknown column %q", name)
	}
	if len(t.Columns) == 1 {
		return fmt.Errorf("cannot drop the only column")
	}

	newColumns := append(append([]parser.ColumnDefinition{}, t.Columns[:idx]...), t.Columns[idx+1:]...)
	path, file, err := t.rebuildFile(newColumns, func(row []any) []any {
		return append(row[:idx:idx], row[idx+1:]...)
	})
	if err != nil {
		return err
	}

	t.Columns = newColumns
	return t.adoptFile(path, file)
}
''')
sub("// renameColumn doesn't touch Rows — rows are positional, not keyed by",
    "// renameColumn doesn't touch the file — rows are positional, not keyed by")

# --- DELETE
replace_func('Delete', '''// Delete removes every row matching where (a nil where matches every row,
// so a bare DELETE FROM wipes the table) by tombstoning it in the table's
// file; see deleteFromFile.
func (t *SqlTable) Delete(where parser.Expression) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	return t.deleteFromFile(where, columnIndex)
}
''')
replace_func('deleteFromFile', '''// deleteFromFile tombstones every live row of the table's file that matches
// where, then fsyncs once. The matching rows are collected before the first
// tombstone is written, so an evaluation error touches nothing, and a row is
// never tombstoned while the scan is still reading its page; only an I/O error
// can leave the delete partly applied, and the engine has no WAL to undo that.
// Callers hold t.mu.
func (t *SqlTable) deleteFromFile(where parser.Expression, columnIndex map[string]int) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	var matched []storage.RowID
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
		}
	}

	for _, id := range matched {
		if err := t.file.Delete(id); err != nil {
			return err
		}
	}
	return t.file.Sync()
}
''')

# --- UPDATE
remove_func('updateFile')
replace_func('Update', '''// Update applies assignments to every row matching where (a nil where
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

	var oldIDs []storage.RowID
	var updated [][]any
	// A unique column is in conflict when a row that is NOT being updated
	// already holds the assigned value. A matched row's own old value is
	// deliberately excluded, so re-assigning a column back to its current
	// value never self-conflicts.
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
			for _, idx := range uniqueColumns {
				if row[idx] != nil && row[idx] == assigned[idx] {
					conflicts[idx] = true
				}
			}
			continue
		}
		oldIDs = append(oldIDs, fileRow.ID)
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
	for _, b := range encoded {
		if _, err := t.file.Insert(b); err != nil {
			return err
		}
	}
	for _, id := range oldIDs {
		if err := t.file.Delete(id); err != nil {
			return err
		}
	}
	return t.file.Sync()
}
''')

open(P, 'w').write(src)
print("table.go rewritten")
PYEOF
gofmt -w internal/engine/table.go; go build ./... 2>&1 | head
```

Expected: `table.go rewritten`, then `go build ./...` succeeds. The `Rows [][]any` field is still declared (and now always nil), so the test files, including the temporary ones, still compile; `go vet ./internal/engine` must be clean.

- [ ] **Step 5: Run the temporary tests and the whole suite (RED to GREEN)**

Run: `gofmt -l .; go vet ./... && go test -race -count=1 ./internal/engine -run 'Test(Table_(InsertValues_(DuplicateOnALatePage|DuplicateBeatsALater|CoercionErrorBeatsALater)|Update_(UniqueConflictReports|SeveralMatchesReport|UniquenessAcrossManyPages)|AddColumn_UniqueDefaultIsAccepted)|RowsRemoval_)' -v 2>&1 | grep -E '^(--- |FAIL|ok|panic)'; go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: the four `RowsRemoval_*` tests now PASS (they failed in Step 3), the seven `uniqueness_test.go` tests PASS, and every package is `ok`. This green run, with `Rows` emptied before each statement, is the evidence that the new code does not read it. Any failure here is a regression in the rewrite: check the message precedence against the Global Constraints.

- [ ] **Step 6: Delete the field and the temporary tests**

The temporary tests have done their job and cannot compile once the field is gone. Remove them and the field, then rerun everything:

```bash
rm internal/engine/rows_removal_red_test.go
python3 - <<'PYEOF'
P = 'internal/engine/table.go'
src = open(P).read()
old = """	Columns []parser.ColumnDefinition
	Rows    [][]any

	// path and file are the table's heap file. INSERT and ALTER ADD/DROP
	// COLUMN keep it a copy of Rows; the other statements and every read
	// still use Rows alone until they are migrated.
	path string"""
new = """	Columns []parser.ColumnDefinition

	// path and file are the table's heap file: the only copy of its rows.
	// Every statement reads and writes it under mu, and none holds a whole
	// table in memory.
	path string"""
assert src.count(old) == 1
open(P, 'w').write(src.replace(old, new))
PYEOF
gofmt -w internal/engine/table.go; gofmt -l .; go build ./... && go vet ./... && go test -race -count=1 ./... 2>&1 | grep -v 'no test files'
```

Expected: every package `ok`. `git status --short` must not list `rows_removal_red_test.go` (it was never added to git).

- [ ] **Step 7: Mutation-check the new behaviour**

In `internal/engine/table.go`, one at a time, run `go test -race -count=1 ./internal/engine`, confirm a failure, then restore the file (compare with a saved copy). Make sure each mutant compiles.
1. In `checkUniqueness`, replace the `taken, err := t.existingKeys(candidates)` call with an empty map per candidate column. Expect `DuplicateOnALatePage` and the existing duplicate tests to fail.
2. In `InsertValues`, return `rowErr` before calling `checkUniqueness`. Expect `DuplicateBeatsALaterRowError` to fail.
3. In `Update`, delete the `if len(updated) > 1 && len(uniqueColumns) > 0 { ... }` block. Expect `SeveralMatchesReportTheFirstUniqueColumn` and `DuplicateWithinSameBatch` to fail.
4. In `Update`, change `if !matched {` to also run the conflict check for matched rows (move the `for _, idx := range uniqueColumns` loop out of the `if`). Expect `ReassigningUniqueColumnToItsOwnValueSucceeds` to fail.
5. In `addColumn`, change `rows >= 2` to `rows >= 1`. Expect `AddColumn_BackfillsDefault`-style tests on a one-row unique column to fail.
6. In `rebuildFile`, remove the `transform(row)` call (encode `row` as is). Expect the `ALTER` tests to fail.

- [ ] **Step 8: Update the docs**

In `CLAUDE.md`, replace the sentences of the **Storage** paragraph from `Tables are moving onto these files one statement at a time` up to the end of that paragraph with:

```
A table's rows live only in its file; no statement keeps them in memory. INSERT appends; SELECT streams the file a page at a time under the table's read lock (scans may run concurrently with one another); DELETE tombstones the matching rows; UPDATE inserts each new row version at the end of the file and tombstones the old one, so updated rows move to the end and row order after an UPDATE is unspecified; ALTER ADD/DROP COLUMN streams the file into a new file (a fresh `file_id`, the old one deleted) in the new column layout, and RENAME COLUMN needs no rewrite because rows are positional. PRIMARY KEY/UNIQUE checks stream the file against a small set of the statement's own key values, so memory is proportional to the statement and time to the table until an index exists. The space of deleted rows is not reclaimed yet. A row whose encoding exceeds one page (`storage.MaxRowSize`) is rejected with `row too large`, since there are no overflow pages.
```

(Do this with a small script that finds the paragraph's start marker and replaces to the end of the line, asserting the marker occurs once.)

Also in `CLAUDE.md`, replace the **Engine** bullet

```
- Multi-row/multi-step mutations (`InsertValues`, `AlterColumns`'s `ADD COLUMN`) build into a fresh buffer and only commit in a single final assignment, so a failure partway through never leaves partial state applied.
```

with

```
- Multi-row/multi-step mutations (`InsertValues`, `Update`, `AlterColumns`'s `ADD`/`DROP COLUMN`) validate and encode everything before the first write to the table's file, so a bad statement writes nothing; only an I/O error can leave one partly applied, since there is no WAL.
```

In `plan/on-disk-persistence-design.md` section 5.0, add this paragraph directly under its heading: `Status: complete. B1 to B6 are done; \`Rows\` no longer exists, so the shadow-copy rules below describe the transition that has finished.`

- [ ] **Step 9: Whole-repo verification**

Run: `gofmt -l .; go build ./... && go vet ./... && go test -race -count=3 ./internal/engine ./internal/storage && go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`, no flakiness over three runs; `grep -n 'Rows' internal/engine/table.go` shows only `Response.Rows` and prose about "rows".

- [ ] **Step 10: Commit**

```bash
git add internal/engine/table.go internal/engine/uniqueness_test.go CLAUDE.md
git status --short   # only these files; plan/ must not appear
git commit -m "feat(engine): keep a table's rows only in its file

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR(s): Task 1 can be its own PR (`test(engine): assert on the table's file instead of in-memory rows`, no production change) with Task 2 as `feat(engine): keep a table's rows only in its file`, or both together. The branch is stacked on B5, so if the lower PRs are not on `main` yet, set the base to the B5 branch and say so. Each body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Task 2 Summary should say this is step 6 of 6, that no statement keeps a table's rows in memory any more and uniqueness is checked by streaming the file against the statement's own keys, and that no client-visible behaviour changes beyond what B1 to B5 already introduced. After a lower PR is squash-merged, move this branch with `git rebase --onto origin/main <old tip of the lower branch> <this branch>`; bound network calls with `-c http.lowSpeedLimit=1000 -c http.lowSpeedTime=30` and an explicit timeout.

## What comes next

The on-disk row storage is complete for the in-memory catalog. The remaining planned work is PR C, catalog persistence: system tables for databases and table schemas, a schema serializer, startup load and crash recovery, plus a directory fsync after creating and removing files and a clean `Catalog.Close()` on server shutdown. After that, the B-tree index from the original roadmap replaces the one-scan-per-statement uniqueness check.

## Self-review (done while writing)

- **Spec coverage:** the spec's B6 (streaming `PRIMARY KEY`/`UNIQUE` checks, `ALTER` rebuilding from the file, deleting `Rows`, moving the tests that read `table.Rows`) is Tasks 1 and 2; the 5.3 uniqueness wording (a small set from the statement's own keys, one scan) is implemented in `checkUniqueness` and `Update`.
- **Placeholders:** none; every code step has full code and exact commands.
- **Type consistency:** the helper names in Task 2 (`buildRow`, `checkUniqueness`, `existingKeys`, `countRows`, `rebuildFile`, `encodeRowChecked`) are used consistently between the replacement script and the tests; `assertTableRows`/`assertTableRowsAnyOrder`/`fileRows` are defined in Task 1 and used in Task 2's tests.
- **Behaviour preserved:** the one-at-a-time `INSERT` check order is kept by building rows up to the first bad one, checking uniqueness on those, then reporting the held error; `UPDATE` reports the first conflicting unique column in assignment order, and a collision between two matched rows on the first unique column; `ADD COLUMN` still rejects a unique default on two or more rows before doing any file work. Remaining edge changes: a closed table with several simultaneous errors may report the closed error before a later row's or `WHERE` error, and an engine-level `UPDATE` that assigns the same unique column twice (`SET email = 'x', email = 'y'`) used to be rejected by the row-by-row check but is now accepted with the last value winning; the parser already rejects duplicate assignments, so SQL clients cannot reach it (ledger it as a `Ruling:`).
- **Known limitations carried forward:** every uniqueness check and every `UPDATE`/`DELETE`/`SELECT` still scans the whole file until an index exists; `DecodeRow` decodes every column even when only some are needed; the space of tombstoned rows is not reclaimed; an I/O failure partway through a statement can leave it partly applied (no WAL).
