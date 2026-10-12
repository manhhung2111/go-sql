# PR B4: UPDATE Writes New Row Versions to the Table File Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `UPDATE` also write each updated row to the table's heap file, as a new version appended to the file with the old version tombstoned, keeping the file the same set of rows as `Rows`, without changing what any client can read.

**Architecture:** `Update` keeps its existing `Rows` work first (resolving the assignments, evaluating the `WHERE`, the `PRIMARY KEY`/`UNIQUE` checks, building the new rows), so every error and its message stay exactly as they are. A new step then scans the file, decodes each live row, evaluates the same `WHERE` against it, applies the assignments to a copy of each matching row, encodes and size-checks all the new versions, inserts them, tombstones the old versions, and fsyncs once, and only then commits `newRows` to `Rows`. `Rows` is still the authority and nothing reads the file yet (PR B5), so the file stays a shadow copy.

**Tech Stack:** Go 1.26.4, `internal/storage` (`File.Scan`, `File.Insert`, `File.Delete`, `File.Sync`), the row codec, testify.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.0, 5.3, 5.4).

## Prerequisite

This PR is stacked on B3, which is not merged yet. Branch from the B3 branch; no network is needed:

```bash
git checkout feat/delete-to-file
git checkout -b feat/update-to-file
```

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing. The code blocks in this plan are not guaranteed to be gofmt-aligned, so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks. The plan's Commit steps mark intended commit boundaries; the user approved per-task commits for this stack.
- `plan/` is gitignored; never commit it.
- Only `Update` and its new helper change. `InsertValues`, `Delete`, `Select`, `AlterColumns` and the `PRIMARY KEY`/`UNIQUE` checks (still computed from `Rows`) behave exactly as before.
- Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`; the file is only touched while holding `Table.mu`.
- An update inserts the new version before it tombstones the old one, so a crash between the two can duplicate a row but never lose one (spec 5.3). Space of tombstoned versions is not reclaimed (spec non-goal).
- The claim in spec 5.0 that a migrated statement's file step cannot fail where its `Rows` step succeeded relies on `UPDATE` never assigning NULL: the parser only accepts a STRING or NUMBER literal after `SET`. Do not add NULL assignment in this PR.

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test:

1. An `UPDATE` that fails (unknown column in `SET` or `WHERE`, a literal of the wrong kind, a `UNIQUE` violation) must leave `Rows` and the file untouched, with the same error message as before (Task 1 `FailedUpdateLeavesFileAndRowsUntouched`).
2. A row that would grow past one page after the update must reject the whole statement and write nothing, even for rows that would still fit (Task 1 `RowThatGrowsPastAPageIsRejectedAtomically`).
3. `UPDATE` on a closed table must return an error and leave `Rows` untouched, not panic on the nil file (Task 1 `OnAClosedTableFails`).
4. Updated rows must keep working in later statements, and columns that were not assigned (including NULLs) must keep their values (Task 1 `ThenDeleteThenInsertStayMirrored`, `UnassignedNullColumnsStayNull`).
5. The new versions must be on disk, with the old versions gone, when `UPDATE` returns, including across many pages (Task 1 `IsOnDiskOnceItReturns`, `AcrossManyPages`).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/engine/table.go` | Modify | `Update` mirrors to the file; `updateFile` |
| `internal/engine/update_file_test.go` | Create | the mirror tests; `assertMirroredAsSet`, `setTo` |
| `CLAUDE.md` | Modify | describe that `UPDATE` now writes the file |

---

### Task 1: `UPDATE` writes new row versions to the file

**Files:**
- Modify: `internal/engine/table.go`
- Create: `internal/engine/update_file_test.go`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: `storage.File` methods `Scan() iter.Seq2[storage.Row, error]`, `Insert(rowBytes []byte) (storage.RowID, error)`, `Delete(id storage.RowID) error`, `Sync() error`; `DecodeRow`, `encodeRows(columns, rows) ([][]byte, error)`, `evalWhere`; the existing `resolvedAssignment{index int; value any}` type; `SqlTable.file`, `.path`, `.Columns`, `.Rows`; test helpers from earlier PRs: `usersTable(t, extra...)`, `uniqueUsersTable(t)`, `fourUsers(t, table)`, `ageColumn`, `fileRows(t, table)`, `assertMirrored(t, table)`, `cmp`, `numTok`, `strTok`, `identTok`, `tokRow`.
- Produces: `func (t *SqlTable) updateFile(assignments []resolvedAssignment, where parser.Expression, columnIndex map[string]int) error` (called with `t.mu` held, after the `Rows` side has succeeded); test helpers `assertMirroredAsSet(t, table)` and `setTo(column string, value parser.Token) parser.Assignment`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/update_file_test.go`:

```go
package engine

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// An UPDATE appends the new version of a row to the file and tombstones the
// old one, so the file holds the same rows as Rows but not necessarily in the
// same order.
func assertMirroredAsSet(t *testing.T, table *SqlTable) {
	t.Helper()
	assert.ElementsMatch(t, table.Rows, fileRows(t, table), "the file must hold exactly the table's rows, in any order")
}

func setTo(column string, value parser.Token) parser.Assignment {
	return parser.Assignment{Column: column, Value: value}
}

func TestTable_Update_MirrorsToFile(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.EQ, numTok("2")),
	))

	assert.Equal(t, []any{int64(2), "zed"}, table.Rows[1])
	assertMirroredAsSet(t, table)
	assert.Len(t, fileRows(t, table), 4, "the old version is tombstoned, not left beside the new one")
}

func TestTable_Update_WithoutWhereUpdatesEveryRow(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("x"))}, nil))

	for _, row := range table.Rows {
		assert.Equal(t, "x", row[1])
	}
	assertMirroredAsSet(t, table)
	assert.Len(t, fileRows(t, table), 4)
}

func TestTable_Update_MultipleAssignmentsMirror(t *testing.T) {
	table := usersTable(t, ageColumn)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob"), numTok("30")),
		tokRow(numTok("2"), strTok("sam"), numTok("40")),
	}))

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("z")), setTo("age", numTok("99"))},
		cmp(identTok("id"), parser.EQ, numTok("1")),
	))

	assert.Equal(t, []any{int64(1), "z", int64(99)}, table.Rows[0])
	assertMirroredAsSet(t, table)
}

func TestTable_Update_NoMatchLeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.EQ, numTok("99")),
	))

	assertMirrored(t, table) // nothing moved, so even the order is unchanged
}

func TestTable_Update_AcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	// Ids 0..699 sit on the first pages; their new versions go at the end.
	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("m"))},
		cmp(identTok("id"), parser.LT, numTok("700")),
	))

	assert.Len(t, table.Rows, n)
	assertMirroredAsSet(t, table)
	assert.Len(t, fileRows(t, table), n)
}

func TestTable_Update_FailedUpdateLeavesFileAndRowsUntouched(t *testing.T) {
	tests := []struct {
		name        string
		assignments []parser.Assignment
		where       parser.Expression
		wantErr     string
	}{
		{"unknown column in SET", []parser.Assignment{setTo("nope", strTok("x"))}, nil, `unknown column "nope"`},
		{"literal of the wrong kind in SET", []parser.Assignment{setTo("id", strTok("abc"))}, nil, `column "id": expected a numeric value, got abc`},
		{"unknown column in WHERE", []parser.Assignment{setTo("email", strTok("x@x.com"))}, cmp(identTok("nope"), parser.EQ, numTok("1")), `unknown column "nope"`},
		{"UNIQUE violation", []parser.Assignment{setTo("email", strTok("b@x.com"))}, cmp(identTok("id"), parser.EQ, numTok("1")), `duplicate entry b@x.com for column "email"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := uniqueUsersTable(t)
			require.NoError(t, table.InsertValues(nil, [][]parser.Token{
				tokRow(numTok("1"), strTok("a@x.com")),
				tokRow(numTok("2"), strTok("b@x.com")),
			}))
			wantRows := append([][]any(nil), table.Rows...)

			err := table.Update(tc.assignments, tc.where)

			assert.EqualError(t, err, tc.wantErr)
			assert.Equal(t, wantRows, table.Rows)
			assertMirrored(t, table) // untouched, so even the order is unchanged
		})
	}
}

func TestTable_Update_RowThatGrowsPastAPageIsRejectedAtomically(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	// Row 1 is tiny. Row 2 encodes to exactly storage.MaxRowSize: a 12-byte
	// offset table, the 8-byte id, the 1-byte name and the bio.
	bio := strings.Repeat("x", storage.MaxRowSize-12-8-1)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("a"), strTok("short")),
		tokRow(numTok("2"), strTok("b"), strTok(bio)),
	}))
	wantRows := append([][]any(nil), table.Rows...)

	// A longer name pushes row 2 over the limit; row 1 would still fit.
	err := table.Update([]parser.Assignment{setTo("name", strTok("a-much-longer-name"))}, nil)

	assert.ErrorContains(t, err, "row too large")
	assert.Equal(t, wantRows, table.Rows)
	assertMirrored(t, table)
}

func TestTable_Update_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	wantRows := append([][]any(nil), table.Rows...)
	require.NoError(t, table.Close())

	err := table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, nil)

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Equal(t, wantRows, table.Rows, "Rows is only committed after the file step succeeds")
}

func TestTable_Update_IsOnDiskOnceItReturns(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.LT, numTok("3")),
	))

	// A second handle on the same file sees what is really on disk.
	other, err := storage.OpenFile(table.path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	onDisk := make([][]any, 0)
	for row, err := range other.Scan() {
		require.NoError(t, err)
		decoded, err := DecodeRow(table.Columns, row.Bytes)
		require.NoError(t, err)
		onDisk = append(onDisk, decoded)
	}
	assert.ElementsMatch(t, table.Rows, onDisk)
}

func TestTable_Update_ThenDeleteThenInsertStayMirrored(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.LT, numTok("3")),
	))

	// DELETE decides its file matches from the file's own rows, so it must
	// find the new versions the UPDATE appended.
	require.NoError(t, table.Delete(cmp(identTok("name"), parser.EQ, strTok("zed"))))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("5"), strTok("kay"))}))

	assert.Len(t, table.Rows, 3) // ids 3, 4 and 5
	assertMirroredAsSet(t, table)
}

func TestTable_Update_UnassignedNullColumnsStayNull(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// The row has no note, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))

	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, nil))

	assert.Equal(t, []any{int64(1), "zed", nil}, table.Rows[0])
	assertMirroredAsSet(t, table)
	assert.Equal(t, []any{int64(1), "zed", nil}, fileRows(t, table)[0])
}

// Rows and the file can drift until every statement is migrated. UPDATE decides
// the file's matches from the file's own rows, so a row that matches in only one
// of them is simply not updated in the other, and that is not an error.
func TestTable_Update_ToleratesAFileThatDriftedFromRows(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	table.Rows[1][1] = "zed" // Rows says id 2 is "zed"; the file still says "sam"

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("kim"))},
		cmp(identTok("name"), parser.EQ, strTok("zed")),
	))

	assert.Equal(t, []any{int64(2), "kim"}, table.Rows[1], "Rows updated the row it knows as zed")
	assert.Len(t, fileRows(t, table), 4, "the file has no zed, so it updated nothing and raised no error")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/engine -run 'Test(Table_Update_(MirrorsToFile|WithoutWhere|MultipleAssignments|NoMatch|AcrossMany|FailedUpdate|RowThatGrows|OnAClosed|IsOnDisk|ThenDelete|UnassignedNull|Tolerates))' 2>&1 | grep -E '^(--- |    --- |FAIL|ok|panic)'`
Expected: FAIL for `MirrorsToFile`, `WithoutWhereUpdatesEveryRow`, `MultipleAssignmentsMirror`, `AcrossManyPages`, `OnAClosedTableFails` (no error), `IsOnDiskOnceItReturns`, `ThenDeleteThenInsertStayMirrored` and `UnassignedNullColumnsStayNull` (the file still holds the old versions). `RowThatGrowsPastAPageIsRejectedAtomically` also fails (no error). `NoMatchLeavesTheFileAlone`, `FailedUpdateLeavesFileAndRowsUntouched` (all four subtests) and `ToleratesAFileThatDriftedFromRows` appear as PASS even before the change: they are regression guards for behaviour that must not change.

- [ ] **Step 3: Write the implementation**

In `internal/engine/table.go`, at the end of `Update`, replace

```go
	t.Rows = newRows
	return nil
}

// Rename sets the table's own name
```

with

```go
	// Mirror the update onto the table's file before committing it to Rows,
	// so a failed write leaves Rows untouched.
	if err := t.updateFile(resolved, where, columnIndex); err != nil {
		return err
	}

	t.Rows = newRows
	return nil
}

// updateFile writes the update to the table's file: every live row matching
// where gets a new version, appended with the assignments applied, and its
// old version is tombstoned, then one fsync. The matching rows are collected
// and every new version is encoded and size-checked before the first write,
// so a bad update writes nothing; only an I/O error can leave it partly
// applied, and the engine has no WAL to undo that. The new versions are
// inserted before the old ones are tombstoned, so a crash between the two can
// duplicate a row but never lose one. The matches come from the file's own
// rows, not from Rows, which can drift until every statement is migrated, and
// uniqueness is not re-checked here: Rows already did. Callers hold t.mu and
// have already evaluated where and the assignments against Rows, so an
// invalid statement was reported there first.
func (t *SqlTable) updateFile(assignments []resolvedAssignment, where parser.Expression, columnIndex map[string]int) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	var oldIDs []storage.RowID
	var updated [][]any
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
		if !isMatch {
			continue
		}
		for _, a := range assignments {
			decoded[a.index] = a.value
		}
		oldIDs = append(oldIDs, row.ID)
		updated = append(updated, decoded)
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

// Rename sets the table's own name
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal/engine/table.go internal/engine/update_file_test.go; gofmt -l internal; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'Test(Table_Update)' -v 2>&1 | grep -E '^(--- |FAIL|ok)'`
Expected: all PASS, including the existing `TestTable_Update_*` tests and `TestTable_Update_Concurrent` if present.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`; the engine-level `UPDATE` tests pass unchanged.

- [ ] **Step 6: Mutation-check the tests**

Make each change in `internal/engine/table.go`, run `go test -count=1 ./internal/engine`, confirm a failure, then restore the file (compare with a saved copy). Make sure each mutant compiles; if it fails only to compile, adjust it so it actually runs the tests.

1. In `Update`, remove the `t.updateFile(...)` call. Expect the mirror tests to fail.
2. In `Update`, commit `t.Rows = newRows` before calling `updateFile`. Expect `OnAClosedTableFails` to fail.
3. In `updateFile`, skip the insert loop (new versions never written). Expect the mirror tests to fail (the file loses rows).
4. In `updateFile`, skip the tombstone loop (old versions left beside the new). Expect `MirrorsToFile` to fail (the file has duplicates).
5. In `updateFile`, move the insert loop into the scan loop so each new version is written as it is found, before all are encoded. Expect `RowThatGrowsPastAPageIsRejectedAtomically` to fail (row 1's new version was already written when row 2 is rejected).
6. In `updateFile`, remove the `if t.file == nil` check. Expect `OnAClosedTableFails` to fail.
7. In `updateFile`, remove the final `t.file.Sync()` and return `nil`. This is not observable by the tests, so expect it to survive; the fsync is covered by review and the spec.

- [ ] **Step 7: Update the docs**

In `CLAUDE.md`, replace `the space of deleted rows is not reclaimed yet.` with:

```
the space of deleted rows is not reclaimed yet. UPDATE inserts each new row version at the end of the file and tombstones the old one, so updated rows move to the end.
```

In `plan/on-disk-persistence-design.md` section 5.0, replace ``from B3 on, `DELETE` on a closed table fails with the same `closed` error;`` with ``from B3 on, `DELETE`, and from B4 on `UPDATE`, on a closed table fail with the same `closed` error, and from B4 on an `UPDATE` that makes a row too large for a page fails with `row too large` (all-or-nothing);``, and after the sentence `tombstoning a row that is not there matches nothing.` add: ` This relies on `UPDATE` never assigning NULL (the parser only accepts a STRING or NUMBER literal after `SET`): if that ever changes, re-check that a migrated statement's file step cannot fail where its `Rows` step succeeded.`

- [ ] **Step 8: Commit**

```bash
git add internal/engine/table.go internal/engine/update_file_test.go CLAUDE.md
git status --short   # only these files; plan/ must not appear
git commit -m "feat(engine): mirror UPDATE onto the table's heap file

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR: title `feat(engine): mirror UPDATE onto the table's heap file`. This branch is stacked on B3, so if B3 is not on `main` yet, set the PR's base to the B3 branch and say so in the Summary. The body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Summary should say this is step 4 of 6, that `Rows` is still the authority and nothing reads the file yet, that updated rows move to the end of the file, and that the client-visible changes are `UPDATE` on a closed table failing and `row too large` when an update would push a row past one page. After a lower PR is squash-merged, move this branch with `git rebase --onto origin/main <old tip of the lower branch> <this branch>`; bound network calls with `-c http.lowSpeedLimit=1000 -c http.lowSpeedTime=30` and an explicit timeout.

## What comes next

B5 `SELECT` reads from the file instead of `Rows`, and B6 deletes `Rows`. Because updated rows now sit at the end of the file, a `SELECT` after an `UPDATE` will return rows in a different order than before B5; row order after an `UPDATE` is unspecified (spec 5.0), and the existing tests that assert order after an `UPDATE` need to be checked in B5.

## Self-review (done while writing)

- **Spec coverage:** the `UPDATE` flow in 5.3 (scan to find matches, apply the assignments, encode everything before the first write, insert the new versions, tombstone the old ones, `Sync()` once) is Task 1, with the shadow-phase rules from 5.0: the file's matches come from the file's own rows, drift is tolerated, uniqueness stays on `Rows`, and the mirror tests compare as a multiset because updated rows move.
- **Placeholders:** none; every code step has full code and exact commands.
- **Type consistency:** `updateFile(assignments []resolvedAssignment, where parser.Expression, columnIndex map[string]int) error` is called with the `resolved`, `where` and `columnIndex` already computed in `Update`; `storage.RowID` and the `Scan`/`Insert`/`Delete`/`Sync` signatures match the merged storage package; `encodeRows`, `DecodeRow` and the test helpers are the ones earlier PRs defined.
- **Behaviour changes to note:** `UPDATE` on a closed table now returns `table "x" is closed` instead of silently succeeding, and an `UPDATE` that makes a row too large for one page is rejected with `row too large` (all-or-nothing). Every `Rows`-side error keeps its message because the `Rows` pass still runs first. The `WHERE` is evaluated twice until B6 removes `Rows`.
- **Known limitations carried forward:** every updated row costs an insert plus a tombstone (a page rewrite each), and the old versions' space is not reclaimed, so repeatedly updating a table grows its file. An I/O error partway through can leave the file with duplicates or missing versions while `Rows` is unchanged, which the spec accepts without a WAL.
