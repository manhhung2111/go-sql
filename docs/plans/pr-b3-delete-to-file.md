# PR B3: DELETE Tombstones Rows in the Table File Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `DELETE` also tombstone every matching row in the table's heap file, keeping the file an exact copy of `Rows`, without changing what any client can read.

**Architecture:** `Delete` keeps its existing `Rows` evaluation first (so every `WHERE` error and its message stay exactly as they are). A new step then scans the file, decodes each live row with the table's schema, evaluates the same `WHERE` against it, collects the matching `RowID`s, tombstones them all and fsyncs once, and only then commits the survivors to `Rows`. `Rows` is still the authority and nothing reads the file yet (that is PR B5), so the file stays a shadow copy.

**Tech Stack:** Go 1.26.4, `internal/storage` (`File.Scan`, `File.Delete`, `File.Sync`), the row codec, testify.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.0, 5.3, 5.4).

## Prerequisite

Local `main` is at the merged B2 (`b2fa115`). Start from it; no network is needed:

```bash
git checkout main
git checkout -b feat/delete-to-file
```

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing. The code blocks in this plan are not guaranteed to be gofmt-aligned, so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks. The plan's Commit steps mark intended commit boundaries; ask the user at execution time whether to make them.
- `plan/` is gitignored; never commit it.
- Only `Delete` and its new helper change. `InsertValues`, `Update`, `Select`, `AlterColumns` and the `PRIMARY KEY`/`UNIQUE` checks behave exactly as before.
- Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`; the file is only touched while holding `Table.mu`.
- A deleted row's slot becomes a tombstone (`Offset=0, Length=0`); its space is not reclaimed (spec non-goal).

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test:

1. A `DELETE` whose `WHERE` fails (unknown column, a literal of the wrong kind) must leave both `Rows` and the file untouched, with the same error message as before (Task 1 `FailedWhereLeavesFileAndRowsUntouched`).
2. `DELETE` on a closed table must return an error and leave `Rows` untouched, not panic on the nil file (Task 1 `OnAClosedTableFails`).
3. `NULL` never matches a comparison, and a file that drifted from `Rows` (un-migrated `UPDATE` changes `Rows` only) must not make a `DELETE` fail (Task 1 `NullNeverMatches`, `ToleratesAFileThatDriftedFromRows`).
4. Deleting rows on pages other than the cached tail page, wiping the whole table, and inserting afterwards must all keep the file equal to `Rows` (Task 1 `AcrossManyPages`, `WithoutWhereEmptiesTheFileAndItStaysUsable`, `ThenInsertMirrorsInOrder`).
5. The tombstones must be on disk when `DELETE` returns, visible to a fresh handle on the same file (Task 1 `IsOnDiskOnceItReturns`).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/engine/table.go` | Modify | `Delete` mirrors to the file; `deleteFromFile` |
| `internal/engine/delete_file_test.go` | Create | the mirror tests |
| `CLAUDE.md` | Modify | describe that `DELETE` now writes the file |

---

### Task 1: `DELETE` tombstones the matching rows in the file

**Files:**
- Modify: `internal/engine/table.go`
- Create: `internal/engine/delete_file_test.go`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: `storage.File` methods `Scan() iter.Seq2[storage.Row, error]` (each `storage.Row` has `ID storage.RowID` and `Bytes []byte`), `Delete(id storage.RowID) error`, `Sync() error`; `DecodeRow(columns, b) ([]any, error)`; `evalWhere(where, row, columnIndex, columns) (bool, error)`; `SqlTable.file`, `SqlTable.path`, `SqlTable.Columns`, `SqlTable.Rows`; test helpers from earlier PRs: `usersTable(t, extra...)`, `fileRows(t, table)`, `assertMirrored(t, table)`, `cmp(left, op, right)`, `numTok`, `strTok`, `identTok`, `tokRow`.
- Produces: `func (t *SqlTable) deleteFromFile(where parser.Expression, columnIndex map[string]int) error` (called with `t.mu` held, after the `Rows` side has been evaluated successfully).

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/delete_file_test.go`:

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

func fourUsers(t *testing.T, table *SqlTable) {
	t.Helper()
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
		tokRow(numTok("3"), strTok("amy")),
		tokRow(numTok("4"), strTok("lee")),
	}))
}

func TestTable_Delete_MirrorsToFile(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("2"))))

	assert.Len(t, table.Rows, 3)
	assertMirrored(t, table)
}

func TestTable_Delete_WithoutWhereEmptiesTheFileAndItStaysUsable(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(nil))

	assert.Empty(t, table.Rows)
	assert.Empty(t, fileRows(t, table))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("zed"))}))
	assertMirrored(t, table)
	assert.Len(t, table.Rows, 1)
}

func TestTable_Delete_NoMatchLeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("99"))))

	assert.Len(t, table.Rows, 4)
	assert.Len(t, fileRows(t, table), 4)
	assertMirrored(t, table)
}

func TestTable_Delete_AcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	// Ids 0..699 sit on the first pages, which are not the cached tail page.
	require.NoError(t, table.Delete(cmp(identTok("id"), parser.LT, numTok("700"))))

	assert.Len(t, table.Rows, n-700)
	assertMirrored(t, table)
}

func TestTable_Delete_FailedWhereLeavesFileAndRowsUntouched(t *testing.T) {
	tests := []struct {
		name    string
		where   parser.Expression
		wantErr string
	}{
		{"unknown column", cmp(identTok("nope"), parser.EQ, numTok("1")), `unknown column "nope"`},
		{"literal of the wrong kind", cmp(identTok("id"), parser.EQ, strTok("abc")), "expected a numeric value, got abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := usersTable(t)
			fourUsers(t, table)
			wantRows := append([][]any(nil), table.Rows...)

			err := table.Delete(tc.where)

			assert.EqualError(t, err, tc.wantErr)
			assert.Equal(t, wantRows, table.Rows)
			assertMirrored(t, table)
		})
	}
}

func TestTable_Delete_NullNeverMatches(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}})
	// Row 1 has no age, so it is NULL; row 2 has age 5.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("sam"), numTok("5"))}))

	require.NoError(t, table.Delete(cmp(identTok("age"), parser.NEQ, numTok("7"))))

	// NULL != 7 never matches, so only the row with age 5 goes.
	require.Len(t, table.Rows, 1)
	assert.Equal(t, int64(1), table.Rows[0][0])
	assertMirrored(t, table)
}

func TestTable_Delete_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Close())

	err := table.Delete(nil)

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Len(t, table.Rows, 4, "Rows is only committed after the file step succeeds")
}

func TestTable_Delete_ThenInsertMirrorsInOrder(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("2"))))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("5"), strTok("kay"))}))

	assertMirrored(t, table)
	ids := []int64{}
	for _, row := range fileRows(t, table) {
		ids = append(ids, row[0].(int64))
	}
	assert.Equal(t, []int64{1, 3, 4, 5}, ids, "a deleted slot is never reused, so the new row goes at the end")
}

func TestTable_Delete_IsOnDiskOnceItReturns(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.LT, numTok("3"))))

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
	assert.Equal(t, table.Rows, onDisk)
}

// Until UPDATE is migrated it changes Rows without touching the file, so a row
// can differ between the two. DELETE decides the file's matches from the
// file's own rows, so such drift is not an error and does not fail the
// statement.
func TestTable_Delete_ToleratesAFileThatDriftedFromRows(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	table.Rows[1][1] = "zed" // Rows says id 2 is "zed"; the file still says "sam"

	require.NoError(t, table.Delete(cmp(identTok("name"), parser.EQ, strTok("zed"))))

	assert.Len(t, table.Rows, 3, "Rows deleted the row it knows as zed")
	assert.Len(t, fileRows(t, table), 4, "the file has no zed, so it deleted nothing and raised no error")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/engine -run 'Test(Table_Delete_(MirrorsToFile|WithoutWhere|NoMatchLeaves|AcrossMany|FailedWhere|NullNever|OnAClosed|ThenInsert|IsOnDisk|Tolerates))' 2>&1 | grep -E '^(--- |FAIL|ok|panic)'`
Expected: FAIL for `MirrorsToFile`, `WithoutWhereEmptiesTheFileAndItStaysUsable`, `AcrossManyPages`, `NullNeverMatches`, `ThenInsertMirrorsInOrder`, `IsOnDiskOnceItReturns` (the file still holds the deleted rows) and `OnAClosedTableFails` (no error). `NoMatchLeavesTheFileAlone`, `FailedWhereLeavesFileAndRowsUntouched` (both subtests) and `ToleratesAFileThatDriftedFromRows` appear in the output as PASS even before the change: they are regression guards for behaviour that must not change.

- [ ] **Step 3: Write the implementation**

In `internal/engine/table.go`, in `Delete`, replace the last two lines

```go
	t.Rows = kept
	return nil
}
```

(the ones at the end of `Delete`, directly after the `for _, row := range t.Rows { ... }` loop) with

```go
	// Mirror the delete onto the table's file before committing it to Rows,
	// so a failed write leaves Rows untouched.
	if err := t.deleteFromFile(where, columnIndex); err != nil {
		return err
	}

	t.Rows = kept
	return nil
}

// deleteFromFile tombstones every live row of the table's file that matches
// where, then fsyncs once. The matching rows are collected before the first
// tombstone is written, so an evaluation error touches nothing, and a row is
// never tombstoned while the scan is still reading its page; only an I/O error
// can leave the delete partly applied, and the engine has no WAL to undo that.
// The matches come from the file's own rows, not from Rows: the two can drift
// until every statement is migrated, and a row that exists in only one of them
// is simply not a match in the other. Callers hold t.mu, and have already
// evaluated where against Rows, so an invalid where was reported there first.
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal/engine/table.go internal/engine/delete_file_test.go; gofmt -l internal; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'Test(Table_Delete)' -v 2>&1 | grep -E '^(--- |FAIL|ok)'`
Expected: all PASS, including the existing `TestTable_Delete_*` tests.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`; the engine-level `DELETE` tests and `TestTable_Delete_Concurrent` pass unchanged.

- [ ] **Step 6: Mutation-check the tests**

Make each change in `internal/engine/table.go`, run `go test -count=1 ./internal/engine`, confirm a failure, then restore the file (compare with a saved copy). Make sure each mutant compiles; if it fails only to compile, adjust it (for example with `_ = x`) so it actually runs the tests.

1. In `Delete`, remove the `t.deleteFromFile(...)` call. Expect the mirror tests to fail.
2. In `Delete`, commit `t.Rows = kept` before calling `deleteFromFile`. Expect `OnAClosedTableFails` to fail (`Rows` changed although the statement errored).
3. In `deleteFromFile`, tombstone only the first matched id (`matched = matched[:1]` when non-empty). Expect `AcrossManyPages` and `WithoutWhere...` to fail.
4. In `deleteFromFile`, remove the `if t.file == nil` check. Expect `OnAClosedTableFails` to fail.
5. In `deleteFromFile`, remove the final `t.file.Sync()` and return `nil` instead. This is not observable by the tests (the OS has the write either way), so expect it to survive; that is the known limit of what a test can see, and the fsync is covered by review and by the spec, not by a test.

- [ ] **Step 7: Update CLAUDE.md**

In the **Storage** paragraph, replace `RENAME COLUMN needs no rewrite because rows are positional.` with:

```
RENAME COLUMN needs no rewrite because rows are positional. DELETE tombstones the matching rows in the file, scanning it with the same WHERE; the space of deleted rows is not reclaimed yet.
```

And in `plan/on-disk-persistence-design.md` section 5.0, change `(both all-or-nothing);` to `(both all-or-nothing); from B3 on, \`DELETE\` on a closed table fails with the same \`closed\` error;`.

- [ ] **Step 8: Commit**

```bash
git add internal/engine/table.go internal/engine/delete_file_test.go CLAUDE.md
git status --short   # only these files; plan/ must not appear
git commit -m "feat(engine): mirror DELETE onto the table's heap file

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR: title `feat(engine): mirror DELETE onto the table's heap file`. If the branches below this one are already on `main` the base is `main`; if this branch is stacked on an unmerged branch, set the base to that branch and say so in the Summary. Stacks and squash merges: a squash merge leaves the lower branch's original commits as non-ancestors of `main`, so after each lower PR is squash-merged, move the next branch with `git rebase --onto origin/main <old tip of the lower branch> <this branch>` instead of a plain rebase (or merge the stack with merge commits). When pushing a stack over a slow connection, bound each network call with `-c http.lowSpeedLimit=1000 -c http.lowSpeedTime=30` and an explicit timeout. The body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Summary should say this is step 3 of 6, that `Rows` is still the authority and nothing reads the file yet, and that the one client-visible change is `DELETE` on a closed table failing. The Test plan should list the mirror tests, the failed-`WHERE` and closed-table cases, the on-disk check with a second handle, and the mutation checks.

## What comes next

B4 `UPDATE`, B5 `SELECT` and B6 cleanup follow the order in the spec (sections 3 and 5.0). B4 can be branched from this branch without waiting for it to merge. After B4 the file order and `Rows` order differ for updated rows (updated rows move to the end of the file), so B4's mirror tests compare as a multiset and `DELETE` keeps working because it decides its matches from the file's own rows.

## Self-review (done while writing)

- **Spec coverage:** the `DELETE` flow in 5.3 (collect the matching `RowID`s first so an evaluation error touches nothing, tombstone them all, `Sync()` once) is Task 1, with the shadow-phase rule from 5.0 that the file's matches come from the file's own rows and benign drift is not an error. The space of deleted rows is not reclaimed, as the spec's non-goals say.
- **Placeholders:** none; every code step has full code and exact commands.
- **Type consistency:** `deleteFromFile(where parser.Expression, columnIndex map[string]int) error` is called with the `where` and `columnIndex` already computed in `Delete`; `storage.RowID` and the `Scan`/`Delete`/`Sync` signatures match the merged storage package; the test helpers are the ones B1 and B2 defined.
- **Behaviour change to note:** `DELETE` on a closed table now returns `table "x" is closed` instead of silently succeeding. A `WHERE` error still reports from the `Rows` evaluation first, so existing messages are unchanged. Evaluating the `WHERE` twice (on `Rows`, then on the file) costs one extra scan until B6 removes `Rows`.
- **Known limitations carried forward:** each tombstone is a separate page read and write (`File.Delete` per row), so deleting many rows on one page rewrites that page many times; batching deletes per page is a storage-level optimisation for later. A `DELETE` without `WHERE` leaves every page full of tombstones rather than replacing the file with an empty one; reclaiming space is a spec non-goal for now. An I/O error partway through leaves some rows tombstoned while `Rows` is unchanged, which the spec accepts without a WAL.
