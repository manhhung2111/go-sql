# PR B2: ALTER TABLE Rewrites the Table File Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `ALTER TABLE ... ADD COLUMN` and `DROP COLUMN` rebuild the table's heap file in the new schema (as a new file with a new `file_id`), so the file is always in the table's current column layout, without changing what any client can read.

**Architecture:** `addColumn` and `dropColumn` keep computing the new schema and new rows exactly as today. Before committing them to the table they now write a complete new file holding those rows, fsync it, and only then commit `Columns`/`Rows` and swap the table onto the new file, deleting the old one. Any failure before the swap leaves the table, its file and the directory exactly as they were. `RENAME COLUMN` stays schema-only (rows are positional), so it does not touch the file. To allocate the replacement file, a table now holds the `fileAllocator` and its database name, which `NewTable` takes instead of a ready-made path.

**Tech Stack:** Go 1.26.4, `internal/storage`, the row codec, testify.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.0, 5.3, 5.4).

## Prerequisite

PR #6 (B1) is merged. Start from fresh `main`:

```bash
git checkout main && git pull --ff-only origin main
git checkout -b feat/alter-to-file
```

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing. The code blocks in this plan are not guaranteed to be gofmt-aligned, so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks. The plan's Commit steps mark intended commit boundaries; ask the user at execution time whether to make them.
- `plan/` is gitignored; never commit it.
- File ids are never reused; a replacement file gets a fresh id from the shared `fileAllocator`, in the same `<data_dir>/data/<db>/` directory.
- Dropping and replacing follow "commit first, remove files second": once the new file is in place and `Columns`/`Rows` are committed, a failure removing the old file is returned as an error that says the change was applied, and leaves an orphan file, never a table pointing at a missing file.
- Only `NewTable`, `addColumn`, `dropColumn` and their helpers change. `Select`, `Delete`, `Update`, `InsertValues` and `renameColumn` behave exactly as before.
- Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`; the file and the replacement are only touched while holding `Table.mu`.

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test:

1. An `ALTER` that fails (schema validation, or a row that would no longer fit on a page after `ADD COLUMN`) must leave the old file, the schema, `Rows` and the directory exactly as they were, with no stray new file (Task 2 `RejectedAlterLeavesFileSchemaAndRowsUntouched`, `AddColumnThatMakesARowTooLargeIsRejected`).
2. `ALTER` on a closed table must return an error, not panic or silently succeed (Task 2 `AlterOnAClosedTableFails`).
3. The old file must be gone after a successful `ALTER`, and a later `DROP TABLE` must remove the new file, not the old path (Task 2 `AddColumnRewritesTheFileInTheNewSchema`, Task 2 `TestLifecycle_AlterThenDropRemovesTheCurrentFile`).
4. The rewrite builds from `Rows`, so it must re-sync a file that has drifted from `Rows` (Task 2 `AlterResyncsAFileThatDriftedFromRows`).
5. Empty tables and tables spanning many pages must both rewrite correctly, and an `INSERT` after the `ALTER` must write in the new column layout (Task 2 `AlterOnAnEmptyTable`, `AlterManyPages`, `InsertAfterAlterMirrorsInTheNewLayout`).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/engine/table.go` | Modify | `NewTable` takes the allocator; `createTableFile`, `encodeRows`; `newFileHolding`, `adoptFile`, `discardFile`; `addColumn`/`dropColumn` rewrite the file |
| `internal/engine/database.go` | Modify | `CreateTable` passes the allocator and database name to `NewTable` |
| `internal/engine/testhelpers_test.go` | Modify | `newTempTable` builds an allocator over a temp dir |
| `internal/engine/table_file_test.go` | Create | `NewTable` file-creation tests |
| `internal/engine/alter_file_test.go` | Create | the `ALTER` rewrite tests |
| `internal/engine/lifecycle_test.go` | Modify | alter-then-drop lifecycle test |
| `CLAUDE.md` | Modify | describe that `ALTER` now writes the file |

---

### Task 1: A table allocates its own files

A pure refactor with one new behaviour test: `NewTable` receives the `fileAllocator` and the database name instead of a ready-made path, so the table can later allocate a replacement file. The `encodeRows` extraction removes the encode-and-size-check loop from `appendToFile` so Task 2 can reuse it.

**Files:**
- Modify: `internal/engine/table.go`
- Modify: `internal/engine/database.go`
- Modify: `internal/engine/testhelpers_test.go`
- Create: `internal/engine/table_file_test.go`

**Interfaces:**
- Consumes: `fileAllocator.databaseDir(database string) string`, `fileAllocator.newTablePath(database string) string`, `storage.CreateFile(path string) (storage.File, error)`, `storage.MaxRowSize`, `EncodeRow`.
- Produces:
  - `func NewTable(name string, columns []parser.ColumnDefinition, files *fileAllocator, database string) (*SqlTable, error)`
  - `func createTableFile(files *fileAllocator, database string) (string, storage.File, error)` (creates the database directory if missing, allocates a path, creates the empty file)
  - `func encodeRows(columns []parser.ColumnDefinition, rows [][]any) ([][]byte, error)` (encodes every row and rejects any over `storage.MaxRowSize` with `row too large: ...`; writes nothing)
  - `SqlTable` fields `files *fileAllocator` and `database string`

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/table_file_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func TestNewTable_CreatesItsFileInTheDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	files, err := newFileAllocator(DataDir(dir))
	require.NoError(t, err)

	// The database directory does not exist yet; creating the table makes it.
	table, err := NewTable("users", idColumn, files, "shop")
	require.NoError(t, err)
	t.Cleanup(func() { _ = table.Close() })

	assert.Equal(t, filepath.Join(dir, "data", "shop", "1.tbl"), table.path)
	_, statErr := os.Stat(table.path)
	assert.NoError(t, statErr)
}

func TestNewTable_RejectedSchemaCreatesNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	files, err := newFileAllocator(DataDir(dir))
	require.NoError(t, err)

	_, err = NewTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "id", DataType: parser.IntDataType{}},
	}, files, "shop")

	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "data"))
	assert.True(t, os.IsNotExist(statErr), "validation runs before any directory or file is created")
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go vet ./internal/engine 2>&1 | head -5`
Expected: compile errors, `NewTable` called with the wrong arguments (it still takes a path).

- [ ] **Step 3: Write the implementation**

In `internal/engine/table.go`:

1. Add two fields to `SqlTable`, after `file storage.File`:

```go
	// files and database let the table allocate a replacement file when a
	// schema change rewrites its rows.
	files    *fileAllocator
	database string
```

2. Refresh the stale `path`/`file` field comment on `SqlTable` (the B1 review flagged it) so it reads:

```go
	// path and file are the table's heap file. INSERT and ALTER ADD/DROP
	// COLUMN keep it a copy of Rows; the other statements and every read
	// still use Rows alone until they are migrated.
	path string
	file storage.File
```

3. Replace the whole `NewTable` function (keep its doc comment, replacing only the last paragraph) with:

```go
// Rules when for a table schema:
//   - Not yet supported the multi-column primary key
//   - There is only 1 primary key column in a table, and the table can have multiple columns with constrants (NOT NULL & UNIQUE)
//   - Columns do not specify with contraint NOT NULL will default be NULLABLE
//   - A table can not have duplicated columns
//
// The schema is validated before anything is created on disk, so a rejected
// schema never leaves a file or directory behind. The table's file is a new,
// empty file in database's directory.
func NewTable(name string, columns []parser.ColumnDefinition, files *fileAllocator, database string) (*SqlTable, error) {
	validated := make([]parser.ColumnDefinition, 0, len(columns))
	for _, column := range columns {
		if err := validateNewColumn(name, validated, column); err != nil {
			return nil, err
		}
		validated = append(validated, column)
	}

	path, file, err := createTableFile(files, database)
	if err != nil {
		return nil, err
	}

	return &SqlTable{
		Name: name, Columns: columns, Rows: make([][]any, 0),
		path: path, file: file, files: files, database: database,
	}, nil
}

// createTableFile makes a new, empty table file in database's directory,
// creating the directory first if it is missing: names that differ only by
// case share one directory on a case-insensitive filesystem, so dropping one
// can remove a directory another database still uses. The file is created
// exclusively, so it can never adopt an old file's rows.
func createTableFile(files *fileAllocator, database string) (string, storage.File, error) {
	if err := os.MkdirAll(files.databaseDir(database), 0o755); err != nil {
		return "", nil, fmt.Errorf("creating directory for database %q: %w", database, err)
	}

	path := files.newTablePath(database)
	file, err := storage.CreateFile(path)
	if err != nil {
		return "", nil, err
	}
	return path, file, nil
}
```

4. Replace `appendToFile` and add `encodeRows` (the existing `appendToFile` doc comment stays, with its body replaced):

```go
// appendToFile writes rows to the table's file with a single fsync. Every
// row is encoded and size-checked before the first write, so a bad batch
// writes nothing; only an I/O error can leave it partly written, and the
// engine has no WAL to undo that. Callers hold t.mu.
func (t *SqlTable) appendToFile(rows [][]any) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	encoded, err := encodeRows(t.Columns, rows)
	if err != nil {
		return err
	}

	for _, b := range encoded {
		if _, err := t.file.Insert(b); err != nil {
			return err
		}
	}
	return t.file.Sync()
}

// encodeRows encodes every row with columns and rejects any that cannot fit
// on a page, before the caller writes anything.
func encodeRows(columns []parser.ColumnDefinition, rows [][]any) ([][]byte, error) {
	encoded := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := EncodeRow(columns, row)
		if err != nil {
			return nil, err
		}
		if len(b) > storage.MaxRowSize {
			return nil, fmt.Errorf("row too large: %d bytes exceeds the %d-byte limit", len(b), storage.MaxRowSize)
		}
		encoded[i] = b
	}
	return encoded, nil
}
```

In `internal/engine/database.go`, in `CreateTable`, delete the `os.MkdirAll` block and its comment (the table now creates the directory itself), and change the `NewTable` call:

```go
	table, err := NewTable(name, columns, d.files, d.Name)
```

In `internal/engine/testhelpers_test.go`, replace `newTempTable`:

```go
// newTempTable is NewTable over a temp-dir data directory, with the same
// (table, err) result so a call site only changes the function name. The
// table's file is closed when the test ends.
func newTempTable(t *testing.T, name string, columns []parser.ColumnDefinition) (*SqlTable, error) {
	t.Helper()
	files, err := newFileAllocator(DataDir(t.TempDir()))
	require.NoError(t, err)
	table, err := NewTable(name, columns, files, "testdb")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = table.Close() })
	return table, nil
}
```

(`path/filepath` may no longer be imported there; remove the import if the compiler says it is unused.)

- [ ] **Step 4: Run the whole suite to verify it passes**

Run: `gofmt -w internal/engine/*.go; gofmt -l .; go vet ./... && go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`, including the two new tests and every existing lifecycle test (the case-twin and missing-directory tests still pass because the directory is now created inside `createTableFile`).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/table.go internal/engine/database.go internal/engine/testhelpers_test.go internal/engine/table_file_test.go
git commit -m "refactor(engine): let a table allocate its own files

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `ADD COLUMN` and `DROP COLUMN` rewrite the file

**Files:**
- Modify: `internal/engine/table.go`
- Create: `internal/engine/alter_file_test.go`
- Modify: `internal/engine/lifecycle_test.go`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes (Task 1): `createTableFile(files, database) (string, storage.File, error)`, `encodeRows(columns, rows) ([][]byte, error)`, `SqlTable.files`, `SqlTable.database`; (B1) `fileRows(t, table)`, `assertMirrored(t, table)`, `usersTable(t, extra...)`, `numTok`, `strTok`, `tokRow`, `idColumn`, `createDB`, `tableFiles`.
- Produces:
  - `func (t *SqlTable) newFileHolding(columns []parser.ColumnDefinition, rows [][]any) (string, storage.File, error)` (builds and fsyncs a complete replacement file; on any failure removes the partial file and changes nothing)
  - `func (t *SqlTable) adoptFile(path string, file storage.File) error` (makes the replacement the table's file and deletes the old one; callers have already committed `Columns`/`Rows` and hold `t.mu`)
  - `func discardFile(path string, file storage.File)`

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/alter_file_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

var ageColumn = parser.ColumnDefinition{
	Name:        "age",
	DataType:    parser.IntDataType{},
	Constraints: []parser.Constraint{parser.DefaultConstraint{DefaultValue: numTok("18")}},
}

// filesInTableDir lists the file names next to the table's file.
func filesInTableDir(t *testing.T, table *SqlTable) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(table.path))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func twoUsers(t *testing.T, table *SqlTable) {
	t.Helper()
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
	}))
}

func TestTable_AddColumn_RewritesTheFileInTheNewSchema(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	oldPath := table.path

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	assert.Equal(t, []any{int64(1), "bob", int64(18)}, table.Rows[0])
	assertMirrored(t, table)
	assert.NotEqual(t, oldPath, table.path, "the rewrite goes to a new file")
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table), "and the old file is gone")
}

func TestTable_AddColumn_NullableColumnBackfillsNull(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}},
	}))

	assertMirrored(t, table)
	assert.Equal(t, []any{int64(1), "bob", nil}, fileRows(t, table)[0])
}

func TestTable_DropColumn_RewritesTheFileInTheNewSchema(t *testing.T) {
	table := usersTable(t, ageColumn)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob"), numTok("30")),
		tokRow(numTok("2"), strTok("sam"), numTok("40")),
	}))
	oldPath := table.path

	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	assert.Equal(t, []any{int64(1), int64(30)}, table.Rows[0])
	assertMirrored(t, table)
	assert.NotEqual(t, oldPath, table.path)
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table))
}

func TestTable_RenameColumn_LeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	path := table.path

	require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "full_name"}))

	assert.Equal(t, path, table.path, "rows are positional, so a rename needs no rewrite")
	assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table))
	assertMirrored(t, table)
}

func TestTable_Alter_RejectedAlterLeavesFileSchemaAndRowsUntouched(t *testing.T) {
	tests := []struct {
		name   string
		action parser.AlterAction
	}{
		{"duplicate column name", parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "id", DataType: parser.IntDataType{}}}},
		{"NOT NULL without a default", parser.AddColumnAction{Column: parser.ColumnDefinition{
			Name: "req", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.NotNullConstraint{}},
		}}},
		{"UNIQUE default backfilled onto two rows", parser.AddColumnAction{Column: parser.ColumnDefinition{
			Name: "code", DataType: parser.IntDataType{},
			Constraints: []parser.Constraint{parser.UniqueConstraint{}, parser.DefaultConstraint{DefaultValue: numTok("1")}},
		}}},
		{"drop an unknown column", parser.DropColumnAction{Column: "nope"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := usersTable(t)
			twoUsers(t, table)
			path := table.path
			wantColumns := append([]parser.ColumnDefinition(nil), table.Columns...)
			wantRows := append([][]any(nil), table.Rows...)

			err := table.AlterColumns(tc.action)

			require.Error(t, err)
			assert.Equal(t, path, table.path)
			assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table), "no stray new file")
			assert.Equal(t, wantColumns, table.Columns)
			assert.Equal(t, wantRows, table.Rows)
			assertMirrored(t, table)
		})
	}
}

func TestTable_DropColumn_TheOnlyColumnIsRejectedWithoutTouchingTheFile(t *testing.T) {
	table, err := newTempTable(t, "solo", idColumn)
	require.NoError(t, err)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"))}))
	path := table.path

	err = table.AlterColumns(parser.DropColumnAction{Column: "id"})

	assert.EqualError(t, err, "cannot drop the only column")
	assert.Equal(t, path, table.path)
	assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table))
	assertMirrored(t, table)
}

func TestTable_AddColumn_ThatMakesARowTooLargeIsRejected(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	// This row encodes to exactly storage.MaxRowSize bytes: a 12-byte offset
	// table, the 8-byte id, "bob" and the bio. Any new column pushes it over.
	bio := strings.Repeat("x", storage.MaxRowSize-12-8-len("bob"))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"), strTok(bio))}))
	path := table.path

	err := table.AlterColumns(parser.AddColumnAction{Column: ageColumn})

	assert.ErrorContains(t, err, "row too large")
	assert.Len(t, table.Columns, 3, "the schema is unchanged")
	assert.Equal(t, path, table.path)
	assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table), "no stray new file")
	assertMirrored(t, table)
}

func TestTable_Alter_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.Close())

	err := table.AlterColumns(parser.AddColumnAction{Column: ageColumn})

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Len(t, table.Columns, 2, "the schema is unchanged")
}

func TestTable_Alter_OnAnEmptyTable(t *testing.T) {
	table := usersTable(t)
	oldPath := table.path

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))
	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	assert.Empty(t, table.Rows)
	assertMirrored(t, table)
	assert.NotEqual(t, oldPath, table.path)
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table))
}

func TestTable_Alter_ManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	assertMirrored(t, table)
	info, err := os.Stat(table.path)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(2*16*1024), "the rewritten rows span several pages")
	assert.Zero(t, info.Size()%(16*1024))
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table))
}

func TestTable_Alter_InsertAfterAlterMirrorsInTheNewLayout(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("amy"), numTok("50"))}))

	assertMirrored(t, table)
	assert.Equal(t, []any{int64(3), "amy", int64(50)}, fileRows(t, table)[2])
}

// The rewrite is built from Rows, not from the old file, so it also re-syncs a
// file that drifted from Rows: until DELETE and UPDATE are migrated they change
// Rows without touching the file. Cutting Rows directly reproduces that drift
// regardless of which statements have been migrated.
func TestTable_Alter_ResyncsAFileThatDriftedFromRows(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	table.Rows = table.Rows[:1] // the file still holds two rows

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	assert.Len(t, fileRows(t, table), 1)
	assertMirrored(t, table)
}
```

Append to `internal/engine/lifecycle_test.go`:

```go
// After an ALTER the table lives in a new file, so dropping it must remove
// that file, not the path the table started with.
func TestLifecycle_AlterThenDropRemovesTheCurrentFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false)) // 1.tbl
	table, _ := db.GetTable("users")

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}},
	}))

	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"), "1.tbl was replaced by a fresh id, never reused")

	require.NoError(t, db.DropTable("users", false))

	assert.Empty(t, tableFiles(t, dir, "shop"))
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/engine -run 'Test(Table_(AddColumn|DropColumn|RenameColumn|Alter)|Lifecycle_AlterThenDrop)' 2>&1 | grep -E '^(--- |FAIL|ok|panic)'`
Expected: FAIL for the `ADD`/`DROP COLUMN` mirror tests (the file still holds the old layout, so `assertMirrored` fails or decoding errors), `OnAnEmptyTable` (the path never changes without a rewrite), `ManyPages`, `InsertAfterAlter`, `ResyncsAFileThatDrifted`, `AlterThenDropRemovesTheCurrentFile` (`1.tbl` remains), `ThatMakesARowTooLargeIsRejected` (no error), and `OnAClosedTableFails` (no error). `RenameColumn_LeavesTheFileAlone`, `RejectedAlterLeavesFileSchemaAndRowsUntouched` and `TheOnlyColumnIsRejected...` already pass; they are regression guards for behaviour that must not change.

- [ ] **Step 3: Write the implementation**

In `internal/engine/table.go`, add these helpers (for example right after `appendToFile`/`encodeRows`):

```go
// newFileHolding builds a complete replacement for the table's file: a new
// file in the table's database directory holding rows encoded with columns,
// fsynced. Nothing about the table changes. Every row is encoded and
// size-checked before the file is created, and on any later failure the
// partial file is removed, so a failed call leaves the directory as it was.
// Callers hold t.mu.
func (t *SqlTable) newFileHolding(columns []parser.ColumnDefinition, rows [][]any) (string, storage.File, error) {
	if t.file == nil {
		return "", nil, fmt.Errorf("table %q is closed", t.Name)
	}

	encoded, err := encodeRows(columns, rows)
	if err != nil {
		return "", nil, err
	}

	path, file, err := createTableFile(t.files, t.database)
	if err != nil {
		return "", nil, err
	}
	for _, b := range encoded {
		if _, err := file.Insert(b); err != nil {
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

// adoptFile makes file the table's file and deletes the old one. Callers have
// already committed the new Columns and Rows and hold t.mu: if the old file
// cannot be removed the change is still applied and the old file is a
// harmless orphan, so the error says so.
func (t *SqlTable) adoptFile(path string, file storage.File) error {
	oldPath := t.path
	closeErr := t.closeLocked()

	t.file, t.path = file, path

	if err := os.Remove(oldPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("column change applied, but removing the old table file %q failed: %w", oldPath, err))
	}
	return closeErr
}

// discardFile closes and deletes a file that was never adopted.
func discardFile(path string, file storage.File) {
	_ = file.Close()
	_ = os.Remove(path)
}
```

In `addColumn`, replace the last three lines

```go
	t.Columns = append(t.Columns, column)
	t.Rows = newRows
	return nil
}
```

with

```go
	// Build the replacement file before touching the table, so a failure
	// (a row that no longer fits on a page, an I/O error) changes nothing.
	newColumns := append(append([]parser.ColumnDefinition{}, t.Columns...), column)
	path, file, err := t.newFileHolding(newColumns, newRows)
	if err != nil {
		return err
	}

	t.Columns = newColumns
	t.Rows = newRows
	return t.adoptFile(path, file)
}
```

In `dropColumn`, replace the last three lines

```go
	t.Columns = newColumns
	t.Rows = newRows
	return nil
}
```

with

```go
	path, file, err := t.newFileHolding(newColumns, newRows)
	if err != nil {
		return err
	}

	t.Columns = newColumns
	t.Rows = newRows
	return t.adoptFile(path, file)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -w internal/engine/table.go internal/engine/alter_file_test.go internal/engine/lifecycle_test.go; gofmt -l internal; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'Test(Table_(AddColumn|DropColumn|RenameColumn|Alter)|Lifecycle_AlterThenDrop)' -v 2>&1 | grep -E '^(--- |FAIL|ok)'`
Expected: all PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race -count=1 ./... 2>&1 | grep -v 'no test files'`
Expected: every package `ok`. The existing `ALTER` tests (`TestTable_AlterColumns_*`, engine-level `AlterTable` tests) must still pass unchanged.

- [ ] **Step 6: Mutation-check the tests**

Make each change in `internal/engine/table.go`, run `go test -count=1 ./internal/engine`, confirm a failure, then restore the file (compare it with a saved copy):

1. In `addColumn`, commit `t.Columns`/`t.Rows` first and build the file afterwards (swap the order). Expect `ThatMakesARowTooLargeIsRejected` to fail because the schema changed.
2. In `adoptFile`, delete the `os.Remove(oldPath)` block. Expect `RewritesTheFileInTheNewSchema` and the lifecycle test to fail (the old file remains).
3. In `adoptFile`, drop the `t.file, t.path = file, path` assignment's `path` part (leave `t.path` as it was). Expect `AlterThenDropRemovesTheCurrentFile` to fail.
4. In `newFileHolding`, remove the `if t.file == nil` check. Expect `OnAClosedTableFails` to fail.
5. In `dropColumn`, skip the rewrite (keep only the commit of `Columns`/`Rows`). Expect `DropColumn_RewritesTheFileInTheNewSchema` to fail.

- [ ] **Step 7: Update CLAUDE.md**

In the **Storage** paragraph, replace `INSERT writes it now.` with:

```
INSERT writes it now, and so does ALTER ADD/DROP COLUMN: it rebuilds the file from `Rows` as a new file (a fresh `file_id`, the old one deleted) in the new column layout; RENAME COLUMN needs no rewrite because rows are positional.
```

- [ ] **Step 8: Commit**

```bash
git add internal/engine/table.go internal/engine/alter_file_test.go internal/engine/lifecycle_test.go CLAUDE.md
git status --short   # only these files; plan/ must not appear
git commit -m "feat(engine): rewrite the table file on ALTER ADD/DROP COLUMN

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR: title `feat(engine): rewrite the table file on ALTER ADD/DROP COLUMN`. The body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Summary should say this is step 2 of 6, that `Rows` is still the authority and nothing reads the file yet, that `NewTable` now takes the allocator (and the `Database.CreateTable` directory re-creation moved into it), and that `ADD COLUMN` can now fail with `row too large` when a row would no longer fit on a page. The Test plan should list the rewrite, rejected-ALTER, closed-table, many-page and drift tests and the mutation checks.

## What comes next

B3 `DELETE`, B4 `UPDATE`, B5 `SELECT` and B6 cleanup follow the order in the spec (sections 3 and 5.0), each planned after the previous PR merges. After B2 the file is always in the table's current schema, which is what lets B3 and B4 decode it safely.

## Self-review (done while writing)

- **Spec coverage:** the `ALTER` flow in 5.3 (scan, transform, insert into a new file with a new `file_id`, `Sync`, swap, delete the old file) is Task 2, building from `Rows` as 5.0 prescribes for B2; `RENAME COLUMN` stays schema-only; the all-or-nothing and "commit first, remove files second" rules are implemented and tested.
- **Placeholders:** none; every code step has full code and exact commands.
- **Type consistency:** `NewTable(name, columns, files, database)`, `createTableFile`, `encodeRows`, `newFileHolding`, `adoptFile` and `discardFile` are used with the same signatures in both tasks; `ageColumn` is a package-level `ColumnDefinition` used by value in every test.
- **Behaviour changes to note:** `ADD COLUMN` now fails with `row too large` if a row would exceed one page after the new column, and on a closed table with `table "x" is closed`; both are all-or-nothing. The old file is deleted after the swap, so a crash between the swap and the delete would leave an orphan, never a missing file.
- **Known limitation carried forward:** the rewrite encodes every row in memory at once (it builds from `Rows`, which is already all in memory). B6 will make `ALTER` rewrite from a streaming scan of the file instead.
