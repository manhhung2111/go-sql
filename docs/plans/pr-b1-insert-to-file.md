# PR B1: INSERT Writes to the Table File Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `INSERT` also append every new row to the table's heap file, keeping the file an exact copy of `Rows`, without changing what any client can read.

**Architecture:** `InsertValues` keeps its existing validation phases and the `Rows` commit. A new phase between them encodes every new row with the row codec, rejects any row that cannot fit on a page before writing anything, then appends the rows to the file and `fsync`s once. `Rows` is still the authority and nothing reads the file yet (that is PR B5), so the file is a shadow copy.

**Tech Stack:** Go 1.26.4, `internal/storage` (`File.Insert`, `File.Sync`, `File.Scan`), the row codec from PR A, testify.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.0, 5.3, 5.4).

## Prerequisite

PR #5 is merged. Start from fresh `main`:

```bash
git checkout main && git pull --ff-only origin main
git checkout -b feat/insert-to-file
```

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing. The code blocks in this plan are not guaranteed to be gofmt-aligned, so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks. The plan's Commit steps mark intended commit boundaries; ask the user at execution time whether to make them.
- `plan/` is gitignored; never commit it.
- Only `InsertValues` and its helpers change. `Select`, `Delete`, `Update`, `AlterColumns` and the `PRIMARY KEY`/`UNIQUE` checks keep reading and writing `Rows` exactly as before.
- Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`; the file is only touched while holding `Table.mu`.

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test:

1. A batch containing one row too large for any page must write nothing, not even the rows before it (Task 2 `OversizedRowIsRejectedBeforeAnythingIsWritten`).
2. `INSERT` on a closed table must return an error, never dereference the nil file (Task 2 `OnAClosedTableFails`).
3. The empty string and NULL must stay distinct once stored (Task 2 `MirrorsNullAndEmptyStringDistinctly`).
4. A large batch that spans many pages must come back identical and in order (Task 2 `MirrorsAcrossManyPages`).
5. A table with no columns still accepts rows, and a row of exactly `storage.MaxRowSize` bytes is stored (Task 2 `TableWithNoColumns`, `LargestRowThatFitsIsStored`).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/storage/page.go` | Modify | export `MaxRowSize` |
| `internal/storage/file_test.go` | Modify | test the exported constant |
| `internal/engine/table.go` | Modify | `InsertValues` mirrors to the file; `appendToFile` |
| `internal/engine/testhelpers_test.go` | Modify | `fileRows`, `assertMirrored` (reused by PRs B2 to B5) |
| `internal/engine/insert_file_test.go` | Create | the mirror tests |
| `CLAUDE.md` | Modify | describe the shadow copy |

---

### Task 1: Export `storage.MaxRowSize`

The engine must reject an oversized row before it writes any row of the batch, so it needs the limit. `storage` already enforces it inside `File.Insert`, but by then earlier rows of the batch are on disk.

**Files:**
- Modify: `internal/storage/page.go`
- Test: `internal/storage/file_test.go`

**Interfaces:**
- Consumes: the existing unexported `maxRowSize` (`maxPageSize - pageHeaderSize - slotSize`, 16368 bytes).
- Produces: `const MaxRowSize` in package `storage`, equal to `maxRowSize`.

- [ ] **Step 1: Write the failing test**

Append to `internal/storage/file_test.go`:

```go
// MaxRowSize is the limit callers outside the package use to reject an
// oversized row before writing anything: it must be exactly what Insert
// accepts.
func TestMaxRowSize(t *testing.T) {
	f, _ := newTestFile(t)

	_, err := f.Insert(make([]byte, MaxRowSize))
	require.NoError(t, err, "a row of exactly MaxRowSize fits on a page of its own")

	_, err = f.Insert(make([]byte, MaxRowSize+1))
	assert.Error(t, err, "one byte more is rejected")
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/storage -run TestMaxRowSize`
Expected: build failure, `undefined: MaxRowSize`.

- [ ] **Step 3: Write the implementation**

In `internal/storage/page.go`, extend the const block directly after `maxRowSize`:

```go
	maxRowSize = maxPageSize - pageHeaderSize - slotSize

	// MaxRowSize is maxRowSize for callers outside this package: the largest
	// row Insert will accept, so a caller can reject an oversized row before
	// writing the first row of a batch.
	MaxRowSize = maxRowSize
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/storage; go vet ./internal/storage && go test -race -count=1 ./internal/storage`
Expected: PASS, `gofmt` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/page.go internal/storage/file_test.go
git commit -m "feat(storage): export MaxRowSize

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `INSERT` mirrors every row onto the table's file

**Files:**
- Modify: `internal/engine/table.go`
- Modify: `internal/engine/testhelpers_test.go`
- Create: `internal/engine/insert_file_test.go`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: `EncodeRow(columns, row) ([]byte, error)` and `DecodeRow(columns, b) ([]any, error)` (PR A); `storage.File` methods `Insert(rowBytes []byte) (storage.RowID, error)`, `Sync() error`, `Scan() iter.Seq2[storage.Row, error]`; `storage.MaxRowSize` (Task 1); `SqlTable.file`, `SqlTable.path`, `SqlTable.Columns`, `SqlTable.Rows`; the existing test helpers `usersTable(t, extra...)`, `newTempTable`, `numTok`, `strTok`, `tokRow`.
- Produces:
  - `func (t *SqlTable) appendToFile(rows [][]any) error` (called with `t.mu` held)
  - test helpers `fileRows(t *testing.T, table *SqlTable) [][]any` and `assertMirrored(t *testing.T, table *SqlTable)`, which PRs B2 to B5 reuse to prove each statement keeps the file equal to `Rows`.

- [ ] **Step 1: Add the mirror helpers**

In `internal/engine/testhelpers_test.go`, add `"github.com/stretchr/testify/assert"` to the imports (next to the existing `require` import), then append:

```go
// fileRows scans the table's file and decodes every live row with the
// table's current schema: the shadow copy of Rows.
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

// assertMirrored checks the file holds exactly the table's rows, in order.
func assertMirrored(t *testing.T, table *SqlTable) {
	t.Helper()
	assert.Equal(t, table.Rows, fileRows(t, table), "the file must hold exactly the table's rows, in order")
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/engine/insert_file_test.go`:

```go
package engine

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

func TestTable_InsertValues_MirrorsRowsToFile(t *testing.T) {
	table := usersTable(t)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
	}))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("amy"))}))

	assert.Len(t, table.Rows, 3)
	assertMirrored(t, table)
}

func TestTable_InsertValues_MirrorsNullAndEmptyStringDistinctly(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// Row 1: name is the empty string and note was omitted, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok(""))}))
	// Row 2: note is the empty string.
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("sam"), strTok(""))}))

	assertMirrored(t, table)
	onDisk := fileRows(t, table)
	assert.Equal(t, []any{int64(1), "", nil}, onDisk[0])
	assert.Equal(t, []any{int64(2), "sam", ""}, onDisk[1])
}

func TestTable_InsertValues_MirrorsAcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	// Each row encodes to about 56 bytes, so 1500 rows span several 16KiB pages.
	const n = 1500
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}

	require.NoError(t, table.InsertValues(nil, rows))

	assertMirrored(t, table)
	info, err := os.Stat(table.path)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(2*16*1024), "the rows span several pages")
	assert.Zero(t, info.Size()%(16*1024), "the file is a whole number of pages")
}

// This guards the order of the phases: validation must finish before the
// first row is written. It passes even before the mirror exists, because
// nothing was written at all then; it fails if a later change writes rows
// as it validates them.
func TestTable_InsertValues_RejectedBatchWritesNothingToFile(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")), // valid
		tokRow(strTok("two"), strTok("sam")), // a string where an INT belongs
	})

	require.Error(t, err)
	assert.Empty(t, table.Rows)
	assert.Empty(t, fileRows(t, table))
}

func TestTable_InsertValues_OversizedRowIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	huge := strings.Repeat("x", storage.MaxRowSize+1)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob"), strTok("short")), // fits
		tokRow(numTok("2"), strTok("sam"), strTok(huge)),    // fits on no page
	})

	assert.ErrorContains(t, err, "row too large")
	assert.Empty(t, table.Rows, "the batch is all-or-nothing")
	assert.Empty(t, fileRows(t, table), "the row that did fit was not written either")
}

func TestTable_InsertValues_LargestRowThatFitsIsStored(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	// The encoded row is a 12-byte offset table, the 8-byte id, "bob" and the bio.
	bio := strings.Repeat("x", storage.MaxRowSize-12-8-len("bob"))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"), strTok(bio))}))

	assertMirrored(t, table)
}

func TestTable_InsertValues_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.Close())

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))})

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Empty(t, table.Rows)
}

func TestTable_InsertValues_TableWithNoColumns(t *testing.T) {
	table, err := newTempTable(t, "empty", nil)
	require.NoError(t, err)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(), tokRow()}))

	assert.Len(t, table.Rows, 2)
	assertMirrored(t, table)
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/engine -run 'InsertValues_(Mirrors|RejectedBatch|Oversized|LargestRow|OnAClosedTable|TableWithNoColumns)'`
Expected: FAIL for `MirrorsRowsToFile`, `MirrorsNullAndEmptyStringDistinctly`, `MirrorsAcrossManyPages`, `LargestRowThatFitsIsStored` and `TableWithNoColumns` (the file is empty, so `assertMirrored` reports a mismatch), `OversizedRowIsRejectedBeforeAnythingIsWritten` (no error) and `OnAClosedTableFails` (no error). `RejectedBatchWritesNothingToFile` passes: it is the guard described in its comment.

- [ ] **Step 4: Write the implementation**

In `internal/engine/table.go`, at the end of `InsertValues`, replace

```go
	t.Rows = append(t.Rows, newRows...)
	return nil
}
```

with

```go
	// Phase 3: mirror the batch onto the table's file before committing it
	// to Rows, so a failed write leaves Rows untouched.
	if err := t.appendToFile(newRows); err != nil {
		return err
	}

	t.Rows = append(t.Rows, newRows...)
	return nil
}

// appendToFile writes rows to the table's file with a single fsync. Every
// row is encoded and size-checked before the first write, so a bad batch
// writes nothing; only an I/O error can leave it partly written, and the
// engine has no WAL to undo that. Callers hold t.mu.
func (t *SqlTable) appendToFile(rows [][]any) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	encoded := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := EncodeRow(t.Columns, row)
		if err != nil {
			return err
		}
		if len(b) > storage.MaxRowSize {
			return fmt.Errorf("row too large: %d bytes exceeds the %d-byte limit", len(b), storage.MaxRowSize)
		}
		encoded[i] = b
	}

	for _, b := range encoded {
		if _, err := t.file.Insert(b); err != nil {
			return err
		}
	}
	return t.file.Sync()
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `gofmt -w internal/engine/table.go internal/engine/testhelpers_test.go internal/engine/insert_file_test.go; gofmt -l internal; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'InsertValues_(Mirrors|RejectedBatch|Oversized|LargestRow|OnAClosedTable|TableWithNoColumns)' -v 2>&1 | grep -E '^(--- |FAIL|ok)'`
Expected: all PASS.

- [ ] **Step 6: Run the whole suite and note its duration**

Run: `go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`. Every `INSERT` now fsyncs (about 3 ms here), so the engine package may take a little longer than before; note the engine package time. If it exceeds about 15 seconds, stop and ledger a ruling instead of continuing.

- [ ] **Step 7: Mutation-check the tests**

Make each change in `internal/engine/table.go`, run `go test -count=1 ./internal/engine -run InsertValues`, confirm a failure, then restore the file:

1. Remove the `if len(b) > storage.MaxRowSize { ... }` check. Expect `OversizedRowIsRejectedBeforeAnythingIsWritten` to fail, because `File.Insert` now rejects the huge row after the first row of the batch is already written.
2. Move the `t.file.Insert` loop so it runs before the encode loop finishes (write each row as soon as it is encoded). Expect `OversizedRowIsRejectedBeforeAnythingIsWritten` to fail, because the row that fits is written before the oversized one is found. `RejectedBatchWritesNothingToFile` still passes: its bad row fails coercion in phase 2, before `appendToFile` runs, so this mutation cannot affect it.
3. Remove the `if t.file == nil` check. Expect `OnAClosedTableFails` to fail with a nil pointer panic.
4. Replace `t.Rows = append(t.Rows, newRows...)` with nothing (rows never committed to `Rows`). Expect the mirror tests to fail.

Run `cmp` or `git diff` afterwards to confirm `table.go` is back to the intended version.

- [ ] **Step 8: Update CLAUDE.md**

In the **Storage** paragraph, replace the sentence

```
For now the files are created and removed with their tables but stay empty: statements still run against in-memory `Rows`.
```

with

```
Tables are moving onto these files one statement at a time (INSERT, ALTER, DELETE, UPDATE, then SELECT; `Rows` is deleted last). Until SELECT flips, `Rows` is still what statements read and the file is a shadow copy that migrated statements also write; INSERT writes it now. A row whose encoding exceeds one page (`storage.MaxRowSize`) is rejected with `row too large`, since there are no overflow pages.
```

- [ ] **Step 9: Commit**

```bash
git add internal/engine/table.go internal/engine/testhelpers_test.go internal/engine/insert_file_test.go CLAUDE.md
git status --short   # only these files; plan/ must not appear
git commit -m "feat(engine): mirror INSERT onto the table's heap file

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR: title `feat(engine): mirror INSERT onto the table's heap file`. The body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Summary should say this is step 1 of 6 in moving statements onto disk, that `Rows` is still the authority and nothing reads the file yet, and that the one client-visible change is the `row too large` rejection. The Test plan should list the mirror tests, the all-or-nothing and closed-table cases, and the mutation checks.

## What comes next

PRs B2 to B6 follow the order in the spec (section 3 and 5.0): `ALTER`, `DELETE`, `UPDATE`, `SELECT`, then the cleanup that deletes `Rows`. Each gets its own plan after the previous PR merges, reusing `fileRows` and `assertMirrored` from this PR.

## Self-review (done while writing)

- **Spec coverage:** the `INSERT` flow in 5.3 (encode everything before the first write, one fsync, size pre-check) is Task 2; the accepted `row too large` behaviour change in 5.0 is tested and documented; `storage.MaxRowSize` is Task 1. Uniqueness stays on `Rows` as 5.0 requires.
- **Placeholders:** none; every code step has full code and exact commands.
- **Type consistency:** `appendToFile(rows [][]any) error`, `fileRows(t, table) [][]any` and `assertMirrored(t, table)` are used with the same signatures everywhere; `storage.MaxRowSize` is defined in Task 1 and used in Task 2.
- **Known limitations carried forward:** a genuine I/O error partway through a batch can leave the file holding rows `Rows` does not (no WAL, as the spec accepts). The mirror tests compare `Rows` to the file in order, which holds for `INSERT`; PR B4 (`UPDATE`) will compare as a multiset because updated rows move to the end of the file.
