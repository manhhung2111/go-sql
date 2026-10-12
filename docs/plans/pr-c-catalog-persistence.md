# Catalog Persistence (PR C1-C3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Databases, tables and their schemas survive a server restart, and a crash at any point in DDL leaves a state startup can repair.

**Architecture:** Two system heap files (`sys_databases`, `sys_tables`) under `<data_dir>/sys/`, owned by a `catalogStore` that every `Database`/`Table` DDL path writes before changing memory. Startup loads them, resolves duplicate rows against current survivors, opens each table's file, then removes orphans by `file_id`. Delivered as three stacked PRs.

**Tech Stack:** Go 1.26.4, testify, Google Wire, existing `internal/storage` heap files and `engine.EncodeRow`/`DecodeRow`.

**Spec:** `plan/on-disk-persistence-design.md` section 6 and 7 (rewritten for this plan; it is the binding authority).

## Stack

| PR | Branch (base) | Tasks | What changes for users |
|---|---|---|---|
| C1 | `feat/catalog-foundations` (`main`) | 1-3 | nothing |
| C2 | `feat/catalog-store` (C1) | 4-5 | sys files get written; nothing reads them |
| C3 | `feat/catalog-startup-load` (C2) | 6-8 | restart keeps everything; shutdown closes files |

Create each branch from the previous branch's tip. `feat/catalog-foundations` already exists, cut from `origin/main` (f8a9820). Commit per task. Never push or open a PR until asked.

## Global Constraints

- Commits: `<type>(scope): description`, ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Error messages: lowercase, no trailing punctuation, `%q` for identifiers.
- `internal/engine` never imports `proto/sqlpb`. `internal/storage` never imports `internal/parser`.
- Lock order: `Catalog.mu` -> `Database.mu` -> `Table.mu` -> `store.mu`. The store never calls back into a database or table.
- Never `os.RemoveAll`. Remove directories only with non-recursive `os.Remove`. Remove files only if named `<number>.tbl`.
- The catalog write is the commit point: a failed catalog write changes nothing in memory and discards any new file it created.
- Every task finishes with `gofmt -l .` empty, `go vet ./...`, and `go test -race ./internal/engine/... ./internal/storage/...` green (whole `go test -race ./...` in tasks that touch wiring).
- Test helpers `newTempTable(t, name, cols)` and `newTestDatabase(t)` keep their signatures (24 call sites); they build a real temp-dir store internally.
- Known unobservable: removing a directory fsync cannot be seen by any test. Ledger it as such; do not invent a test.

## Review Focus

Failure modes the spec implies that no single task's happy-path tests cover; each has a named test below.

1. Rename then ALTER crash chain `[a,f1] [b,f1] [a,f2]` must keep `f1` for `b` (Task 7, `TestStartup_RenameThenAlterChainKeepsSharedFile`).
2. A case-twin database (`Shop`/`shop` share a directory on macOS) must never lose files to orphan cleanup (Task 7, skipped on case-sensitive FS).
3. DROP DATABASE whose table-row tombstone fails, then CREATE DATABASE of the same name, must not resurrect old tables, in-process and after restart (Tasks 5 and 7).
4. A wide schema must fail `CREATE TABLE` with a clear error and leave no file (Task 5).
5. A live catalog row with a missing or torn file must fail startup and close every handle it opened (Task 6).

---

# PR C1: foundations (no behaviour change)

### Task 1: Durable directory creation

**Files:**
- Create: `internal/storage/dir.go`, `internal/storage/dir_test.go`
- Modify: `internal/storage/file.go` (`CreateFile`), `internal/engine/files.go`, `internal/engine/files_test.go`, `internal/engine/database.go` (`NewDatabase`), `internal/engine/table.go` (`createTableFile`)

**Interfaces:**
- Produces: `storage.SyncDir(path string) error`; `(*fileAllocator).makeDir(dir string) error` (MkdirAll, then fsync `dir`, each ancestor up to the data directory, and the data directory's parent).

- [ ] **Step 1: Write the failing storage test**

`internal/storage/dir_test.go`:

```go
package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncDir(t *testing.T) {
	t.Run("an existing directory syncs", func(t *testing.T) {
		require.NoError(t, SyncDir(t.TempDir()))
	})

	t.Run("a missing directory is an error", func(t *testing.T) {
		err := SyncDir(filepath.Join(t.TempDir(), "missing"))

		assert.ErrorContains(t, err, "opening directory")
	})
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/storage/ -run TestSyncDir -v`
Expected: build failure `undefined: SyncDir`.

- [ ] **Step 3: Implement**

`internal/storage/dir.go`:

```go
package storage

import (
	"fmt"
	"os"
)

// SyncDir fsyncs the directory at path, so entries created in it survive a
// power loss. A file's own fsync does not cover its directory entry.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening directory %q: %w", path, err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return fmt.Errorf("syncing directory %q: %w", path, syncErr)
	}
	return closeErr
}
```

In `CreateFile` (`internal/storage/file.go`), after the successful `os.OpenFile` and before `return`, add:

```go
	if err := SyncDir(filepath.Dir(path)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("creating file %q: %w", path, err)
	}
```

(import `path/filepath`.)

- [ ] **Step 4: Run storage tests**

Run: `go test -race ./internal/storage/...`
Expected: PASS.

- [ ] **Step 5: Write the failing engine test**

Append to `internal/engine/files_test.go`:

```go
func TestFileAllocator_makeDir(t *testing.T) {
	t.Run("creates every missing level", func(t *testing.T) {
		dir := t.TempDir()
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		require.NoError(t, a.makeDir(a.databaseDir("shop")))

		info, err := os.Stat(filepath.Join(dir, "data", "shop"))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("is idempotent", func(t *testing.T) {
		a, err := newFileAllocator(DataDir(t.TempDir()))
		require.NoError(t, err)

		require.NoError(t, a.makeDir(a.databaseDir("shop")))
		require.NoError(t, a.makeDir(a.databaseDir("shop")))
	})

	t.Run("fails when a path component is a file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), []byte("x"), 0o644))
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		assert.Error(t, a.makeDir(a.databaseDir("shop")))
	})
}
```

- [ ] **Step 6: Run, expect FAIL**

Run: `go test ./internal/engine/ -run TestFileAllocator_makeDir`
Expected: build failure `a.makeDir undefined`.

- [ ] **Step 7: Implement `makeDir` and use it**

In `internal/engine/files.go` (add import `manhhung2111/go-sql/internal/storage`):

```go
// makeDir creates dir and any missing parents, then fsyncs dir, each
// ancestor up to the data directory, and the data directory's parent, so a
// catalog row written afterwards can never point into a directory a power
// loss forgets.
func (a *fileAllocator) makeDir(dir string) error {
	root := filepath.Clean(string(a.dataDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for p := dir; ; p = filepath.Dir(p) {
		if err := storage.SyncDir(p); err != nil {
			return err
		}
		if p == root || p == filepath.Dir(p) {
			break
		}
	}
	return storage.SyncDir(filepath.Dir(root))
}
```

Replace `os.MkdirAll(files.databaseDir(name), 0o755)` in `NewDatabase` with `files.makeDir(files.databaseDir(name))`, and the one in `createTableFile` with `files.makeDir(files.databaseDir(database))`, keeping each call's existing error wrapping.

- [ ] **Step 8: Run all, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./internal/...`
Expected: clean, PASS. Ledger: directory fsync removal is unobservable.

```bash
git add internal/storage internal/engine
git commit -m "feat(storage): fsync the parent directory when creating files and directories

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 2: File ids as first-class values

**Files:**
- Modify: `internal/engine/files.go`, `internal/engine/files_test.go`, `internal/engine/table.go`, `internal/engine/table_file_test.go`, other tests that fail to compile

**Interfaces:**
- Produces:
  - `(*fileAllocator).nextFileID() int64` (replaces the id half of `newTablePath`)
  - `(*fileAllocator).tablePath(database string, id int64) string`
  - `type tableFile struct { id int64; path string; file storage.File }` in `table.go`
  - `createTableFile(files *fileAllocator, database string, id int64) (tableFile, error)`
  - `(*SqlTable).rebuildFile(id int64, columns, transform) (tableFile, error)`
  - `(*SqlTable).adoptFile(tf tableFile) error`, `discardFile(tf tableFile)`
  - `SqlTable.fileID int64`
- Removes: `newTablePath`.

- [ ] **Step 1: Write the failing tests**

In `files_test.go`, replace each `a.newTablePath(db)` with `a.tablePath(db, a.nextFileID())` (the compile errors list them), keeping every existing expectation. Add:

```go
func TestFileAllocator_ids(t *testing.T) {
	a, err := newFileAllocator(DataDir(t.TempDir()))
	require.NoError(t, err)

	assert.Equal(t, int64(1), a.nextFileID())
	assert.Equal(t, int64(2), a.nextFileID())
	assert.Equal(t, filepath.Join(string(a.dataDir), "data", "shop", "7.tbl"), a.tablePath("shop", 7))
}
```

In `table_file_test.go` add:

```go
func TestTable_FileIDMatchesItsFile(t *testing.T) {
	table, err := newTempTable(t, "users", idColumn)
	require.NoError(t, err)

	assert.Equal(t, fmt.Sprintf("%d.tbl", table.fileID), filepath.Base(table.path))
}

func TestTable_AlterGivesTheTableANewFileID(t *testing.T) {
	table, err := newTempTable(t, "users", idColumn)
	require.NoError(t, err)
	before := table.fileID

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}},
	}))

	assert.Greater(t, table.fileID, before)
	assert.Equal(t, fmt.Sprintf("%d.tbl", table.fileID), filepath.Base(table.path))
}
```

(add imports `fmt`, `path/filepath`, `parser` as needed.)

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/engine/ 2>&1 | head`
Expected: `a.nextFileID undefined`, `table.fileID undefined`.

- [ ] **Step 3: Implement**

`files.go`: replace `newTablePath` with

```go
// nextFileID returns an id no earlier call has returned.
func (a *fileAllocator) nextFileID() int64 { return a.nextID.Add(1) - 1 }

// tablePath is where the table file with id lives in database's directory.
func (a *fileAllocator) tablePath(database string, id int64) string {
	return filepath.Join(a.databaseDir(database), strconv.FormatInt(id, 10)+tableFileExt)
}
```

`table.go`: add

```go
// tableFile is a table's heap file together with the id its name carries.
type tableFile struct {
	id   int64
	path string
	file storage.File
}
```

Change `createTableFile(files, database)` to take `id int64`, build the path with `files.tablePath(database, id)` and return `tableFile{id, path, file}` (same `makeDir` and exclusive create as today). `NewTable` calls `createTableFile(files, database, files.nextFileID())` and stores `fileID: tf.id, path: tf.path, file: tf.file`. Add `fileID int64` to `SqlTable`. `rebuildFile(id, columns, transform)` creates its file with that id and returns a `tableFile`; `addColumn`/`dropColumn` pass `t.files.nextFileID()`. `adoptFile(tf)` sets `t.file, t.path, t.fileID = tf.file, tf.path, tf.id`. `discardFile(tf)` closes `tf.file` and removes `tf.path`. Fix every other call site the compiler reports.

- [ ] **Step 4: Run all, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./internal/...`
Expected: clean, PASS.

```bash
git add internal/engine
git commit -m "refactor(engine): make a table's file id an explicit value

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 3: Schema codec

**Files:**
- Create: `internal/engine/schemacodec.go`, `internal/engine/schemacodec_test.go`

**Interfaces:**
- Produces: `EncodeSchema(columns []parser.ColumnDefinition) (string, error)`, `DecodeSchema(s string) ([]parser.ColumnDefinition, error)`.

- [ ] **Step 1: Write the failing tests**

`schemacodec_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

var allTypesSchema = []parser.ColumnDefinition{
	{Name: "a", DataType: parser.CharDataType{Size: 3}},
	{Name: "b", DataType: parser.VarCharDataType{Size: 50}},
	{Name: "c", DataType: parser.TextDataType{Size: 9}},
	{Name: "d", DataType: parser.SmallIntDataType{Size: 5}},
	{Name: "e", DataType: parser.MediumIntDataType{Size: 7}},
	{Name: "f", DataType: parser.IntDataType{Size: 11}},
	{Name: "g", DataType: parser.BigIntDataType{}},
	{Name: "h", DataType: parser.BooleanDataType{}},
	{Name: "i", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{
		parser.PrimaryKeyConstraint{}, parser.NotNullConstraint{}, parser.UniqueConstraint{},
		parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.NUMBER, Value: "5"}},
	}},
	{Name: "j", DataType: parser.VarCharDataType{Size: 5}, Constraints: []parser.Constraint{
		parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.STRING, Value: ""}},
	}},
}

func TestSchemaCodec_RoundTripsEveryTypeAndConstraint(t *testing.T) {
	encoded, err := EncodeSchema(allTypesSchema)
	require.NoError(t, err)

	decoded, err := DecodeSchema(encoded)

	require.NoError(t, err)
	assert.Equal(t, allTypesSchema, decoded)
}

func TestSchemaCodec_DropsTokenPosition(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "a", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{
		parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.NUMBER, Value: "5", Position: 42}},
	}}}
	encoded, err := EncodeSchema(columns)
	require.NoError(t, err)

	decoded, err := DecodeSchema(encoded)

	require.NoError(t, err)
	assert.Equal(t, parser.Token{Type: parser.NUMBER, Value: "5"}, decoded[0].Constraints[0].(parser.DefaultConstraint).DefaultValue)
}

// The JSON is the on-disk format: changing it silently would orphan every
// existing catalog.
func TestSchemaCodec_FormatIsStable(t *testing.T) {
	columns := []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{Size: 11}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}, Constraints: []parser.Constraint{
			parser.NotNullConstraint{},
			parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.STRING, Value: "anon"}},
		}},
		{Name: "ok", DataType: parser.BooleanDataType{}},
	}

	encoded, err := EncodeSchema(columns)

	require.NoError(t, err)
	assert.Equal(t, `[{"name":"id","type":{"kind":"int","size":11},"constraints":[{"kind":"primary_key"}]},`+
		`{"name":"name","type":{"kind":"varchar","size":50},"constraints":[{"kind":"not_null"},{"kind":"default","token":"string","value":"anon"}]},`+
		`{"name":"ok","type":{"kind":"boolean"}}]`, encoded)
}

func TestSchemaCodec_RejectsWhatItCannotRepresent(t *testing.T) {
	t.Run("encode: unknown data type", func(t *testing.T) {
		_, err := EncodeSchema([]parser.ColumnDefinition{{Name: "a", DataType: struct{}{}}})
		assert.ErrorContains(t, err, "unsupported data type")
	})
	t.Run("encode: unknown constraint", func(t *testing.T) {
		_, err := EncodeSchema([]parser.ColumnDefinition{{Name: "a", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{struct{}{}}}})
		assert.ErrorContains(t, err, "unsupported constraint")
	})
	t.Run("encode: default that is neither string nor number", func(t *testing.T) {
		_, err := EncodeSchema([]parser.ColumnDefinition{{Name: "a", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{
			parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.IDENT, Value: "x"}},
		}}})
		assert.ErrorContains(t, err, "unsupported default")
	})
}

func TestSchemaCodec_DecodeErrors(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"unknown data type":  {`[{"name":"a","type":{"kind":"float"}}]`, `unknown data type "float"`},
		"unknown constraint": {`[{"name":"a","type":{"kind":"int"},"constraints":[{"kind":"check"}]}]`, `unknown constraint "check"`},
		"unknown token tag":  {`[{"name":"a","type":{"kind":"int"},"constraints":[{"kind":"default","token":"ident","value":"x"}]}]`, `unknown default token "ident"`},
		"unknown field":      {`[{"name":"a","type":{"kind":"int"},"extra":1}]`, "extra"},
		"not json":           {`nope`, "decoding schema"},
		"trailing data":      {`[] []`, "decoding schema"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeSchema(tt.in)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/engine/ -run TestSchemaCodec`
Expected: `undefined: EncodeSchema`.

- [ ] **Step 3: Implement**

`schemacodec.go`:

```go
package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"manhhung2111/go-sql/internal/parser"
)

// The schema of a table is stored as JSON in sys_tables. Every variant has a
// stable string tag; the parser's own enum values are never stored, because
// they are iota-numbered and would shift whenever a token is added.

type jsonColumn struct {
	Name        string           `json:"name"`
	Type        jsonDataType     `json:"type"`
	Constraints []jsonConstraint `json:"constraints,omitempty"`
}

type jsonDataType struct {
	Kind string `json:"kind"`
	Size int    `json:"size,omitempty"`
}

type jsonConstraint struct {
	Kind  string `json:"kind"`
	Token string `json:"token,omitempty"`
	Value string `json:"value,omitempty"`
}

// EncodeSchema renders columns as the JSON stored in sys_tables.
func EncodeSchema(columns []parser.ColumnDefinition) (string, error) {
	out := make([]jsonColumn, len(columns))
	for i, column := range columns {
		dt, err := encodeDataType(column.DataType)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", column.Name, err)
		}
		out[i] = jsonColumn{Name: column.Name, Type: dt}
		for _, constraint := range column.Constraints {
			c, err := encodeConstraint(constraint)
			if err != nil {
				return "", fmt.Errorf("column %q: %w", column.Name, err)
			}
			out[i].Constraints = append(out[i].Constraints, c)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding schema: %w", err)
	}
	return string(b), nil
}

// DecodeSchema is the inverse of EncodeSchema. Unknown tags and unknown
// fields are errors: a schema this build cannot fully understand must not be
// half-loaded.
func DecodeSchema(s string) ([]parser.ColumnDefinition, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var in []jsonColumn
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("decoding schema: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("decoding schema: unexpected data after the column list")
	}

	columns := make([]parser.ColumnDefinition, len(in))
	for i, column := range in {
		dt, err := decodeDataType(column.Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", column.Name, err)
		}
		columns[i] = parser.ColumnDefinition{Name: column.Name, DataType: dt}
		for _, c := range column.Constraints {
			constraint, err := decodeConstraint(c)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", column.Name, err)
			}
			columns[i].Constraints = append(columns[i].Constraints, constraint)
		}
	}
	return columns, nil
}

func encodeDataType(dt parser.DataType) (jsonDataType, error) {
	switch d := dt.(type) {
	case parser.CharDataType:
		return jsonDataType{Kind: "char", Size: d.Size}, nil
	case parser.VarCharDataType:
		return jsonDataType{Kind: "varchar", Size: d.Size}, nil
	case parser.TextDataType:
		return jsonDataType{Kind: "text", Size: d.Size}, nil
	case parser.SmallIntDataType:
		return jsonDataType{Kind: "smallint", Size: d.Size}, nil
	case parser.MediumIntDataType:
		return jsonDataType{Kind: "mediumint", Size: d.Size}, nil
	case parser.IntDataType:
		return jsonDataType{Kind: "int", Size: d.Size}, nil
	case parser.BigIntDataType:
		return jsonDataType{Kind: "bigint"}, nil
	case parser.BooleanDataType:
		return jsonDataType{Kind: "boolean"}, nil
	default:
		return jsonDataType{}, fmt.Errorf("unsupported data type %T", dt)
	}
}

func decodeDataType(d jsonDataType) (parser.DataType, error) {
	switch d.Kind {
	case "char":
		return parser.CharDataType{Size: d.Size}, nil
	case "varchar":
		return parser.VarCharDataType{Size: d.Size}, nil
	case "text":
		return parser.TextDataType{Size: d.Size}, nil
	case "smallint":
		return parser.SmallIntDataType{Size: d.Size}, nil
	case "mediumint":
		return parser.MediumIntDataType{Size: d.Size}, nil
	case "int":
		return parser.IntDataType{Size: d.Size}, nil
	case "bigint":
		return parser.BigIntDataType{}, nil
	case "boolean":
		return parser.BooleanDataType{}, nil
	default:
		return nil, fmt.Errorf("unknown data type %q", d.Kind)
	}
}

func encodeConstraint(constraint parser.Constraint) (jsonConstraint, error) {
	switch c := constraint.(type) {
	case parser.NotNullConstraint:
		return jsonConstraint{Kind: "not_null"}, nil
	case parser.UniqueConstraint:
		return jsonConstraint{Kind: "unique"}, nil
	case parser.PrimaryKeyConstraint:
		return jsonConstraint{Kind: "primary_key"}, nil
	case parser.DefaultConstraint:
		switch c.DefaultValue.Type {
		case parser.STRING:
			return jsonConstraint{Kind: "default", Token: "string", Value: c.DefaultValue.Value}, nil
		case parser.NUMBER:
			return jsonConstraint{Kind: "default", Token: "number", Value: c.DefaultValue.Value}, nil
		default:
			return jsonConstraint{}, fmt.Errorf("unsupported default value %q", c.DefaultValue.Value)
		}
	default:
		return jsonConstraint{}, fmt.Errorf("unsupported constraint %T", constraint)
	}
}

func decodeConstraint(c jsonConstraint) (parser.Constraint, error) {
	switch c.Kind {
	case "not_null":
		return parser.NotNullConstraint{}, nil
	case "unique":
		return parser.UniqueConstraint{}, nil
	case "primary_key":
		return parser.PrimaryKeyConstraint{}, nil
	case "default":
		switch c.Token {
		case "string":
			return parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.STRING, Value: c.Value}}, nil
		case "number":
			return parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.NUMBER, Value: c.Value}}, nil
		default:
			return nil, fmt.Errorf("unknown default token %q", c.Token)
		}
	default:
		return nil, fmt.Errorf("unknown constraint %q", c.Kind)
	}
}
```

Note the unsupported-default encode error text must contain `unsupported default`; the message above does.

- [ ] **Step 4: Run, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./internal/engine/ -run TestSchemaCodec -v` then `go test -race ./...`
Expected: PASS. Mutation check (revert after each): swap `"string"`/`"number"` tags in `decodeConstraint`; drop `DisallowUnknownFields`; drop the trailing-data check. Each must fail a test above.

```bash
git add internal/engine
git commit -m "feat(engine): add a JSON schema codec for table definitions

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

**C1 done.** Run the whole suite, then `git switch -c feat/catalog-store`.

---

# PR C2: the catalog store (writes only)

### Task 4: `catalogStore`

**Files:**
- Create: `internal/engine/catalogstore.go`, `internal/engine/catalogstore_test.go`
- Modify: `internal/engine/testhelpers_test.go`

**Interfaces:**
- Consumes: `EncodeSchema`, `encodeRowChecked`, `DecodeRow`, `fileAllocator`, `storage.File`.
- Produces (all in package `engine`):
  - `openCatalogStore(files *fileAllocator) (*catalogStore, error)`; `(*catalogStore).Close() error` (idempotent)
  - `encodeTableRow(database, name string, fileID int64, columns []parser.ColumnDefinition) ([]byte, error)`
  - `addDatabase(name string) error` (also sweeps stale table rows of that name)
  - `removeDatabase(name string) error` (the commit: tombstone the database row)
  - `removeDatabaseTables(name string) error`
  - `addTable(database, name string, row []byte) error`
  - `removeTable(database, name string) error`
  - `replaceTable(database, oldName, newName string, newRow []byte) error`
  - `type tableRecord struct{ database, name string; fileID int64; schema string }`, `decodeTableRecord(b []byte) (tableRecord, error)`, `decodeDatabaseName(b []byte) (string, error)`
  - fields used by tests and Task 6: `databases`, `tables` (`storage.File`), `dbRows map[string]storage.RowID`, `tableRows map[tableKey]storage.RowID`, `files *fileAllocator`
  - helpers `insertSynced(file, row)`, `deleteSynced(file, id)`.

- [ ] **Step 1: Add test helpers**

Append to `testhelpers_test.go` (add imports `errors`, `storage`):

```go
// newTestStoreAt opens a catalog store over dir and closes it when the test ends.
func newTestStoreAt(t *testing.T, dir string) *catalogStore {
	t.Helper()
	files, err := newFileAllocator(DataDir(dir))
	require.NoError(t, err)
	store, err := openCatalogStore(files)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestStore(t *testing.T) *catalogStore { return newTestStoreAt(t, t.TempDir()) }

var errInjected = errors.New("injected i/o failure")

// failingFile wraps a storage.File and fails the operations a test turns on.
type failingFile struct {
	storage.File
	failInserts bool
	failSyncs   bool
	failDeletes int // fail this many Delete calls, then behave
}

func (f *failingFile) Insert(b []byte) (storage.RowID, error) {
	if f.failInserts {
		return storage.RowID{}, errInjected
	}
	return f.File.Insert(b)
}

func (f *failingFile) Sync() error {
	if f.failSyncs {
		return errInjected
	}
	return f.File.Sync()
}

func (f *failingFile) Delete(id storage.RowID) error {
	if f.failDeletes > 0 {
		f.failDeletes--
		return errInjected
	}
	return f.File.Delete(id)
}

// failSysTables and failSysDatabases wrap the store's system files so a test
// can make catalog writes fail.
func failSysTables(t *testing.T, store *catalogStore) *failingFile {
	t.Helper()
	ff := &failingFile{File: store.tables}
	store.tables = ff
	return ff
}

func failSysDatabases(t *testing.T, store *catalogStore) *failingFile {
	t.Helper()
	ff := &failingFile{File: store.databases}
	store.databases = ff
	return ff
}

func sysDatabaseNames(t *testing.T, store *catalogStore) []string {
	t.Helper()
	names := []string{}
	for row, err := range store.databases.Scan() {
		require.NoError(t, err)
		name, err := decodeDatabaseName(row.Bytes)
		require.NoError(t, err)
		names = append(names, name)
	}
	return names
}

func sysTableRecords(t *testing.T, store *catalogStore) []tableRecord {
	t.Helper()
	records := []tableRecord{}
	for row, err := range store.tables.Scan() {
		require.NoError(t, err)
		record, err := decodeTableRecord(row.Bytes)
		require.NoError(t, err)
		records = append(records, record)
	}
	return records
}

func mustEncodeTableRow(t *testing.T, database, name string, fileID int64, columns []parser.ColumnDefinition) []byte {
	t.Helper()
	row, err := encodeTableRow(database, name, fileID, columns)
	require.NoError(t, err)
	return row
}

func mustSchema(t *testing.T, columns []parser.ColumnDefinition) string {
	t.Helper()
	s, err := EncodeSchema(columns)
	require.NoError(t, err)
	return s
}
```

- [ ] **Step 2: Write the failing store tests**

`catalogstore_test.go`:

```go
package engine

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func TestCatalogStore_DatabaseRows(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.addDatabase("shop"))
	require.NoError(t, store.addDatabase("blog"))
	assert.Equal(t, []string{"shop", "blog"}, sysDatabaseNames(t, store))

	require.NoError(t, store.removeDatabase("shop"))
	assert.Equal(t, []string{"blog"}, sysDatabaseNames(t, store))
}

func TestCatalogStore_TableRows(t *testing.T) {
	store := newTestStore(t)
	schema := mustSchema(t, idColumn)

	require.NoError(t, store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 7, idColumn)))
	assert.Equal(t, []tableRecord{{"shop", "users", 7, schema}}, sysTableRecords(t, store))

	t.Run("replace with a new name keeps one row", func(t *testing.T) {
		require.NoError(t, store.replaceTable("shop", "users", "people", mustEncodeTableRow(t, "shop", "people", 7, idColumn)))
		assert.Equal(t, []tableRecord{{"shop", "people", 7, schema}}, sysTableRecords(t, store))
	})

	t.Run("replace with a new file id keeps one row", func(t *testing.T) {
		require.NoError(t, store.replaceTable("shop", "people", "people", mustEncodeTableRow(t, "shop", "people", 9, idColumn)))
		assert.Equal(t, []tableRecord{{"shop", "people", 9, schema}}, sysTableRecords(t, store))
	})

	t.Run("remove leaves nothing", func(t *testing.T) {
		require.NoError(t, store.removeTable("shop", "people"))
		assert.Empty(t, sysTableRecords(t, store))
	})
}

func TestCatalogStore_RemoveDatabaseTablesOnlyTouchesThatDatabase(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.addTable("shop", "a", mustEncodeTableRow(t, "shop", "a", 1, idColumn)))
	require.NoError(t, store.addTable("blog", "b", mustEncodeTableRow(t, "blog", "b", 2, idColumn)))

	require.NoError(t, store.removeDatabaseTables("shop"))

	records := sysTableRecords(t, store)
	require.Len(t, records, 1)
	assert.Equal(t, "blog", records[0].database)
}

func TestCatalogStore_FailedWritesChangeNothing(t *testing.T) {
	t.Run("a failed insert leaves no row and no index entry", func(t *testing.T) {
		store := newTestStore(t)
		ff := failSysTables(t, store)
		ff.failInserts = true

		err := store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn))

		require.Error(t, err)
		assert.Empty(t, sysTableRecords(t, store))
		assert.Empty(t, store.tableRows)
	})

	t.Run("a failed sync removes the row it inserted", func(t *testing.T) {
		store := newTestStore(t)
		ff := failSysTables(t, store)
		ff.failSyncs = true

		err := store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn))

		require.Error(t, err)
		ff.failSyncs = false
		assert.Empty(t, sysTableRecords(t, store))
	})

	t.Run("replace: a failed tombstone removes the new row and keeps the old", func(t *testing.T) {
		store := newTestStore(t)
		require.NoError(t, store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn)))
		ff := failSysTables(t, store)
		ff.failDeletes = 1 // the old row's tombstone fails; the clean-up of the new row works

		err := store.replaceTable("shop", "users", "people", mustEncodeTableRow(t, "shop", "people", 1, idColumn))

		require.Error(t, err)
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, "users", records[0].name)
		require.NoError(t, store.removeTable("shop", "users"), "the index still points at the old row")
	})
}

func TestCatalogStore_AddDatabaseSweepsStaleTableRows(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.addDatabase("shop"))
	require.NoError(t, store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, idColumn)))
	require.NoError(t, store.removeDatabase("shop"))
	ff := failSysTables(t, store)
	ff.failDeletes = 1
	require.Error(t, store.removeDatabaseTables("shop"), "the table row's tombstone fails")
	require.Len(t, sysTableRecords(t, store), 1, "the stale row is still on disk")

	require.NoError(t, store.addDatabase("shop"))

	assert.Empty(t, sysTableRecords(t, store), "a new database of that name must not inherit the old tables")
}

func TestCatalogStore_WideSchemaIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	columns := make([]parser.ColumnDefinition, 600)
	for i := range columns {
		columns[i] = parser.ColumnDefinition{Name: fmt.Sprintf("column_%03d", i), DataType: parser.VarCharDataType{Size: 255}}
	}

	_, err := encodeTableRow("shop", "wide", 1, columns)

	assert.ErrorContains(t, err, `schema of table "wide" is too large to store`)
}

func TestCatalogStore_ReopeningKeepsTheRows(t *testing.T) {
	dir := t.TempDir()
	first := newTestStoreAt(t, dir)
	require.NoError(t, first.addDatabase("shop"))
	require.NoError(t, first.Close())
	require.NoError(t, first.Close(), "Close is idempotent")

	second := newTestStoreAt(t, dir)

	assert.Equal(t, []string{"shop"}, sysDatabaseNames(t, second))
}
```

- [ ] **Step 3: Run, expect FAIL**

Run: `go test ./internal/engine/ 2>&1 | head`
Expected: `undefined: catalogStore` / `openCatalogStore`.

- [ ] **Step 4: Implement**

`catalogstore.go`:

```go
package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

const (
	sysDirName       = "sys"
	sysDatabasesFile = "sys_databases.tbl"
	sysTablesFile    = "sys_tables.tbl"
)

// The catalog's own tables have hard-coded schemas: they are the bootstrap
// that every other schema is read through.
var (
	sysDatabasesColumns = []parser.ColumnDefinition{
		{Name: "name", DataType: parser.TextDataType{}},
	}
	sysTablesColumns = []parser.ColumnDefinition{
		{Name: "db", DataType: parser.TextDataType{}},
		{Name: "name", DataType: parser.TextDataType{}},
		{Name: "file_id", DataType: parser.BigIntDataType{}},
		{Name: "schema", DataType: parser.TextDataType{}},
	}
)

type tableKey struct{ database, name string }

// tableRecord is one decoded sys_tables row.
type tableRecord struct {
	database string
	name     string
	fileID   int64
	schema   string
}

// catalogStore owns the two system files and remembers where each live row
// is, so a row can be tombstoned without scanning. mu is the last lock in the
// engine's lock order: it is held only around one catalog write, and nothing
// in here calls back into a database or table.
type catalogStore struct {
	mu        sync.Mutex
	files     *fileAllocator
	databases storage.File
	tables    storage.File
	dbRows    map[string]storage.RowID
	tableRows map[tableKey]storage.RowID
	closed    bool
}

func newCatalogStore(files *fileAllocator, databases, tables storage.File) *catalogStore {
	return &catalogStore{
		files:     files,
		databases: databases,
		tables:    tables,
		dbRows:    make(map[string]storage.RowID),
		tableRows: make(map[tableKey]storage.RowID),
	}
}

// openCatalogStore opens <data_dir>/sys/sys_databases.tbl and sys_tables.tbl,
// creating whichever is missing. It does not read their rows.
func openCatalogStore(files *fileAllocator) (*catalogStore, error) {
	dir := filepath.Join(string(files.dataDir), sysDirName)
	if err := files.makeDir(dir); err != nil {
		return nil, fmt.Errorf("creating catalog directory: %w", err)
	}
	databases, err := openOrCreate(filepath.Join(dir, sysDatabasesFile))
	if err != nil {
		return nil, err
	}
	tables, err := openOrCreate(filepath.Join(dir, sysTablesFile))
	if err != nil {
		_ = databases.Close()
		return nil, err
	}
	return newCatalogStore(files, databases, tables), nil
}

func openOrCreate(path string) (storage.File, error) {
	if _, err := os.Stat(path); err == nil {
		return storage.OpenFile(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return storage.CreateFile(path)
}

// Close closes both files. It is idempotent.
func (s *catalogStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.databases.Close(), s.tables.Close())
}

// encodeTableRow builds a sys_tables row. Callers do this before creating any
// file, so a schema too large for a page fails before anything exists on disk.
func encodeTableRow(database, name string, fileID int64, columns []parser.ColumnDefinition) ([]byte, error) {
	schema, err := EncodeSchema(columns)
	if err != nil {
		return nil, fmt.Errorf("table %q: %w", name, err)
	}
	row, err := encodeRowChecked(sysTablesColumns, []any{database, name, fileID, schema})
	if err != nil {
		return nil, fmt.Errorf("schema of table %q is too large to store: %w", name, err)
	}
	return row, nil
}

func decodeDatabaseName(b []byte) (string, error) {
	values, err := DecodeRow(sysDatabasesColumns, b)
	if err != nil {
		return "", fmt.Errorf("decoding sys_databases row: %w", err)
	}
	name, ok := values[0].(string)
	if !ok {
		return "", errors.New("decoding sys_databases row: database name is NULL")
	}
	return name, nil
}

func decodeTableRecord(b []byte) (tableRecord, error) {
	values, err := DecodeRow(sysTablesColumns, b)
	if err != nil {
		return tableRecord{}, fmt.Errorf("decoding sys_tables row: %w", err)
	}
	database, ok1 := values[0].(string)
	name, ok2 := values[1].(string)
	fileID, ok3 := values[2].(int64)
	schema, ok4 := values[3].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return tableRecord{}, errors.New("decoding sys_tables row: a column is NULL")
	}
	return tableRecord{database: database, name: name, fileID: fileID, schema: schema}, nil
}

// insertSynced makes row durable. If the sync fails the row is tombstoned on a
// best-effort basis, so a change the caller was told failed is not left behind.
func insertSynced(file storage.File, row []byte) (storage.RowID, error) {
	id, err := file.Insert(row)
	if err != nil {
		return storage.RowID{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Delete(id)
		return storage.RowID{}, err
	}
	return id, nil
}

func deleteSynced(file storage.File, id storage.RowID) error {
	if err := file.Delete(id); err != nil {
		return err
	}
	return file.Sync()
}

// addDatabase records a new database. Any stale sys_tables rows still under
// this name (left by a DROP DATABASE whose table-row tombstones failed) are
// tombstoned first, so the new database never inherits the old one's tables.
func (s *catalogStore) addDatabase(name string) error {
	row, err := encodeRowChecked(sysDatabasesColumns, []any{name})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.tombstoneTablesLocked(name); err != nil {
		return err
	}
	id, err := insertSynced(s.databases, row)
	if err != nil {
		return err
	}
	s.dbRows[name] = id
	return nil
}

// removeDatabase tombstones the database's row. It is the commit point of
// DROP DATABASE: once it returns nil the database is gone after a restart.
func (s *catalogStore) removeDatabase(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.dbRows[name]
	if !ok {
		return fmt.Errorf("database %q has no catalog row", name)
	}
	if err := deleteSynced(s.databases, id); err != nil {
		return err
	}
	delete(s.dbRows, name)
	return nil
}

// removeDatabaseTables tombstones every table row of database. Rows whose
// tombstone fails stay in the index so addDatabase can sweep them later.
func (s *catalogStore) removeDatabaseTables(database string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tombstoneTablesLocked(database)
}

func (s *catalogStore) tombstoneTablesLocked(database string) error {
	var errs []error
	removed := false
	for key, id := range s.tableRows {
		if key.database != database {
			continue
		}
		if err := s.tables.Delete(id); err != nil {
			errs = append(errs, fmt.Errorf("table %q: %w", key.name, err))
			continue
		}
		delete(s.tableRows, key)
		removed = true
	}
	if removed {
		if err := s.tables.Sync(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *catalogStore) addTable(database, name string, row []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := insertSynced(s.tables, row)
	if err != nil {
		return err
	}
	s.tableRows[tableKey{database, name}] = id
	return nil
}

func (s *catalogStore) removeTable(database, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := tableKey{database, name}
	id, ok := s.tableRows[key]
	if !ok {
		return fmt.Errorf("table %q has no catalog row", name)
	}
	if err := deleteSynced(s.tables, id); err != nil {
		return err
	}
	delete(s.tableRows, key)
	return nil
}

// replaceTable swaps a table's row for newRow (a rename, or a new file id and
// schema). The new row is made durable before the old one is tombstoned: with
// one sync a power loss could keep the tombstone but not the insert, and the
// table would vanish from the catalog. If the tombstone fails, the new row is
// tombstoned best-effort so the catalog does not apply a change the caller
// was told failed.
func (s *catalogStore) replaceTable(database, oldName, newName string, newRow []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldKey := tableKey{database, oldName}
	oldID, ok := s.tableRows[oldKey]
	if !ok {
		return fmt.Errorf("table %q has no catalog row", oldName)
	}
	newID, err := insertSynced(s.tables, newRow)
	if err != nil {
		return err
	}
	if err := deleteSynced(s.tables, oldID); err != nil {
		_ = deleteSynced(s.tables, newID)
		return err
	}
	delete(s.tableRows, oldKey)
	s.tableRows[tableKey{database, newName}] = newID
	return nil
}
```

If `go vet` flags the `else if` after `return` in `openOrCreate`, restructure with a plain `if/return`.

- [ ] **Step 5: Run, mutation-check, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./internal/engine/ -run TestCatalogStore -v`
Expected: PASS. Mutation checks (revert each): drop the sweep call in `addDatabase`; make `replaceTable` use one sync (skip the first `Sync`); skip the best-effort `deleteSynced(newID)`; in `tombstoneTablesLocked` delete the index entry before `Delete` succeeds. Each must fail a test above (the one-sync mutation is unobservable by tests: ledger it as such).

```bash
git add internal/engine
git commit -m "feat(engine): add the catalog store for system-table rows

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 5: DDL writes the catalog

**Files:**
- Modify: `internal/engine/catalog.go`, `database.go`, `table.go`, `testhelpers_test.go`, `table_file_test.go`, `table_test.go` (`TestTable_Rename`), `database_test.go`, `lifecycle_test.go` (compile fixes only)
- Create: `internal/engine/catalog_persist_test.go`

**Interfaces:**
- Consumes: Task 4 store; Task 2 `tableFile`.
- Produces:
  - `NewDatabase(name string, store *catalogStore) (Database, error)`
  - `NewTable(name string, columns []parser.ColumnDefinition, store *catalogStore, database string) (*SqlTable, error)`
  - `Table.Rename(name string) error` (was no return)
  - `SqlTable.store *catalogStore` replaces `SqlTable.files`; `SqlDatabase.store` replaces `SqlDatabase.files`; `SqlCatalog.store`.
  - `(*SqlTable).rebuildAndRecord(columns, transform) (tableFile, error)`.

- [ ] **Step 1: Update helpers so existing tests keep compiling**

In `testhelpers_test.go`: `newTestDatabase` builds `store := newTestStore(t)` and calls `NewDatabase("testdb", store)`; `newTempTable` builds `store := newTestStore(t)` and calls `NewTable(name, columns, store, "testdb")`. Fix `table_file_test.go` (it builds a table over a given dir: use `newTestStoreAt(t, dir)`), `files_test.go` is unaffected. `TestTable_Rename` becomes `require.NoError(t, table.Rename("people"))`.

- [ ] **Step 2: Write the failing tests**

`catalog_persist_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func storeOf(c Catalog) *catalogStore { return c.(*SqlCatalog).store }

func usersColumns() []parser.ColumnDefinition {
	return []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 20}},
	}
}

func TestPersist_DDLWritesCatalogRows(t *testing.T) {
	c := newTestCatalog(t)
	store := storeOf(c)

	t.Run("create database", func(t *testing.T) {
		require.NoError(t, c.CreateDatabase("shop"))
		assert.Equal(t, []string{"shop"}, sysDatabaseNames(t, store))
	})

	db, err := c.GetDatabase("shop")
	require.NoError(t, err)

	t.Run("create table", func(t *testing.T) {
		require.NoError(t, db.CreateTable("users", usersColumns(), false))
		table, _ := db.GetTable("users")
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, tableRecord{"shop", "users", table.(*SqlTable).fileID, mustSchema(t, usersColumns())}, records[0])
	})

	t.Run("rename table keeps the file id", func(t *testing.T) {
		table, _ := db.GetTable("users")
		id := table.(*SqlTable).fileID
		require.NoError(t, db.RenameTable("users", "people"))
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, "people", records[0].name)
		assert.Equal(t, id, records[0].fileID)
	})

	t.Run("alter add and drop column write a new file id and schema", func(t *testing.T) {
		table, _ := db.GetTable("people")
		before := table.(*SqlTable).fileID
		require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}}))
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Greater(t, records[0].fileID, before)
		assert.Contains(t, records[0].schema, `"name":"age"`)

		require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "age"}))
		records = sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.NotContains(t, records[0].schema, `"name":"age"`)
	})

	t.Run("rename column rewrites the schema only", func(t *testing.T) {
		table, _ := db.GetTable("people")
		id := table.(*SqlTable).fileID
		require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "title"}))
		records := sysTableRecords(t, store)
		require.Len(t, records, 1)
		assert.Equal(t, id, records[0].fileID)
		assert.Contains(t, records[0].schema, `"name":"title"`)
	})

	t.Run("drop table", func(t *testing.T) {
		require.NoError(t, db.DropTable("people", false))
		assert.Empty(t, sysTableRecords(t, store))
	})

	t.Run("drop database", func(t *testing.T) {
		require.NoError(t, db.CreateTable("t", idColumn, false))
		require.NoError(t, c.DropDatabase("shop"))
		assert.Empty(t, sysDatabaseNames(t, store))
		assert.Empty(t, sysTableRecords(t, store))
	})
}

func TestPersist_FailedCatalogWriteChangesNothing(t *testing.T) {
	setup := func(t *testing.T) (Catalog, Database, *catalogStore) {
		c := newTestCatalog(t)
		db := createDB(t, c, "shop")
		require.NoError(t, db.CreateTable("users", usersColumns(), false))
		return c, db, storeOf(c)
	}

	t.Run("create table leaves no table and no file", func(t *testing.T) {
		c, db, store := setup(t)
		failSysTables(t, store).failInserts = true

		err := db.CreateTable("orders", idColumn, false)

		require.Error(t, err)
		_, exists := db.GetTable("orders")
		assert.False(t, exists)
		assert.Len(t, tableFiles(t, string(c.(*SqlCatalog).files.dataDir), "shop"), 1, "only users' file")
	})

	t.Run("a wide schema fails before any file exists", func(t *testing.T) {
		c, db, _ := setup(t)
		columns := make([]parser.ColumnDefinition, 600)
		for i := range columns {
			columns[i] = parser.ColumnDefinition{Name: "column_" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)), DataType: parser.VarCharDataType{Size: 255}}
		}

		err := db.CreateTable("wide", columns, false)

		assert.ErrorContains(t, err, "too large to store")
		assert.Len(t, tableFiles(t, string(c.(*SqlCatalog).files.dataDir), "shop"), 1)
	})

	t.Run("drop table keeps the table and its rows", func(t *testing.T) {
		_, db, store := setup(t)
		table, _ := db.GetTable("users")
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{{{Type: parser.NUMBER, Value: "1"}, {Type: parser.STRING, Value: "ann"}}}))
		failSysTables(t, store).failDeletes = 1

		require.Error(t, db.DropTable("users", false))

		again, exists := db.GetTable("users")
		require.True(t, exists)
		resp, err := again.Select([]string{"*"}, nil)
		require.NoError(t, err)
		assert.Equal(t, [][]string{{"1", "ann"}}, resp.Rows)
	})

	t.Run("rename table keeps the old name", func(t *testing.T) {
		_, db, store := setup(t)
		failSysTables(t, store).failInserts = true

		require.Error(t, db.RenameTable("users", "people"))

		_, oldExists := db.GetTable("users")
		_, newExists := db.GetTable("people")
		assert.True(t, oldExists)
		assert.False(t, newExists)
	})

	t.Run("alter add column keeps the schema and discards the new file", func(t *testing.T) {
		c, db, store := setup(t)
		table, _ := db.GetTable("users")
		sqlTable := table.(*SqlTable)
		before := sqlTable.fileID
		failSysTables(t, store).failInserts = true

		err := table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}})

		require.Error(t, err)
		assert.Len(t, sqlTable.Columns, 2)
		assert.Equal(t, before, sqlTable.fileID)
		assert.Len(t, tableFiles(t, string(c.(*SqlCatalog).files.dataDir), "shop"), 1)
	})

	t.Run("rename column keeps the name and does not edit the caller's slice", func(t *testing.T) {
		_, db, store := setup(t)
		table, _ := db.GetTable("users")
		failSysTables(t, store).failInserts = true

		require.Error(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "title"}))

		assert.Equal(t, "name", table.(*SqlTable).Columns[1].Name)
	})

	t.Run("drop database that cannot commit keeps the database usable", func(t *testing.T) {
		c, _, store := setup(t)
		failSysDatabases(t, store).failDeletes = 1

		require.Error(t, c.DropDatabase("shop"))

		db, err := c.GetDatabase("shop")
		require.NoError(t, err)
		_, exists := db.GetTable("users")
		assert.True(t, exists)
	})
}

func TestPersist_DropDatabaseWithStaleTableRowsIsNotResurrected(t *testing.T) {
	c := newTestCatalog(t)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", usersColumns(), false))
	store := storeOf(c)
	failSysTables(t, store).failDeletes = 1

	err := c.DropDatabase("shop")

	assert.ErrorContains(t, err, "dropped, but")
	_, getErr := c.GetDatabase("shop")
	require.Error(t, getErr)
	require.Len(t, sysTableRecords(t, store), 1, "the stale row is still there")

	require.NoError(t, c.CreateDatabase("shop"))

	assert.Empty(t, sysTableRecords(t, store))
	fresh, err := c.GetDatabase("shop")
	require.NoError(t, err)
	_, exists := fresh.GetTable("users")
	assert.False(t, exists)
}
```

(The wide-schema column names must be distinct and valid; the generator above yields 600 distinct names. Replace with `fmt.Sprintf("column_%03d", i)` if you prefer, adding the `fmt` import.)

- [ ] **Step 3: Run, expect FAIL**

Run: `go test ./internal/engine/ 2>&1 | head`
Expected: compile errors (`NewDatabase` arguments, `store` fields, `Rename` returns nothing).

- [ ] **Step 4: Implement**

`catalog.go`: `SqlCatalog` gets `store *catalogStore` (keep `files` as `store.files` access: replace the field with `store`; update `Close`). `NewCatalog`:

```go
func NewCatalog(dataDir DataDir) (Catalog, error) {
	files, err := newFileAllocator(dataDir)
	if err != nil {
		return nil, err
	}
	store, err := openCatalogStore(files)
	if err != nil {
		return nil, err
	}
	return &SqlCatalog{Databases: make(map[string]Database), store: store}, nil
}
```

`CreateDatabase` calls `NewDatabase(name, c.store)`. `DropDatabase`:

```go
	db, exists := c.Databases[name]
	if !exists {
		return fmt.Errorf("database %q does not exist", name)
	}
	// The database row is the commit point. If it cannot be tombstoned the
	// database is untouched.
	if err := c.store.removeDatabase(name); err != nil {
		return fmt.Errorf("dropping database %q: %w", name, err)
	}
	delete(c.Databases, name)

	// From here the database is gone; what is left is clean-up.
	if err := errors.Join(c.store.removeDatabaseTables(name), db.Drop()); err != nil {
		return fmt.Errorf("database %q dropped, but cleaning up after it failed: %w", name, err)
	}
	return nil
```

`Close` closes databases, then `c.store.Close()`, joining errors.

`database.go`: `SqlDatabase.files` becomes `store *catalogStore`. `NewDatabase(name, store)` validates the name, `store.files.makeDir(...)`, then `store.addDatabase(name)` (return its error) before returning. `CreateTable` calls `NewTable(name, columns, d.store, d.Name)`. `Drop` uses `d.store.files.databaseDir`. `RenameTable`: after the two existence checks, `if err := table.Rename(newName); err != nil { return err }` then re-key the map. `DropTable`:

```go
	if err := d.store.removeTable(d.Name, name); err != nil {
		return fmt.Errorf("dropping table %q: %w", name, err)
	}
	delete(d.Table, name)
	if err := table.Drop(); err != nil { ...same message as today... }
```

`table.go`: `Table` interface `Rename(name string) error`. `SqlTable.files` is replaced by `store *catalogStore` (use `t.store.files` where `t.files` was). `NewTable`:

```go
	id := store.files.nextFileID()
	row, err := encodeTableRow(database, name, id, validated)
	if err != nil {
		return nil, err
	}
	tf, err := createTableFile(store.files, database, id)
	if err != nil {
		return nil, err
	}
	if err := store.addTable(database, name, row); err != nil {
		discardFile(tf)
		return nil, err
	}
	return &SqlTable{Name: name, Columns: columns, path: tf.path, file: tf.file, fileID: tf.id, store: store, database: database}, nil
```

`Rename`:

```go
func (t *SqlTable) Rename(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	row, err := encodeTableRow(t.database, name, t.fileID, t.Columns)
	if err != nil {
		return err
	}
	if err := t.store.replaceTable(t.database, t.Name, name, row); err != nil {
		return err
	}
	t.Name = name
	return nil
}
```

Add `rebuildAndRecord`:

```go
// rebuildAndRecord streams the table into a new file in the new schema and
// records that file and schema in the catalog. Nothing about the table
// changes: callers commit t.Columns and adopt the file only after this
// returns, and on any failure the new file is gone.
func (t *SqlTable) rebuildAndRecord(columns []parser.ColumnDefinition, transform func(row []any) []any) (tableFile, error) {
	id := t.store.files.nextFileID()
	row, err := encodeTableRow(t.database, t.Name, id, columns)
	if err != nil {
		return tableFile{}, err
	}
	tf, err := t.rebuildFile(id, columns, transform)
	if err != nil {
		return tableFile{}, err
	}
	if err := t.store.replaceTable(t.database, t.Name, t.Name, row); err != nil {
		discardFile(tf)
		return tableFile{}, err
	}
	return tf, nil
}
```

`addColumn` and `dropColumn` call `rebuildAndRecord(newColumns, ...)` instead of `rebuildFile`. `renameColumn` clones: `newColumns := slices.Clone(t.Columns); newColumns[idx].Name = newName`, encodes `encodeTableRow(t.database, t.Name, t.fileID, newColumns)`, calls `t.store.replaceTable(t.database, t.Name, t.Name, row)`, and only then `t.Columns = newColumns`.

- [ ] **Step 5: Run everything, mutation-check, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: PASS. Mutation checks (revert each): in `DropTable` delete from the map before `removeTable`; in `renameColumn` mutate `t.Columns[idx]` before the store call; in `DropDatabase` skip `removeDatabaseTables`; in `NewTable` call `addTable` before `createTableFile`. Each must fail a test above.

```bash
git add internal/engine
git commit -m "feat(engine): write the catalog on every DDL statement

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

**C2 done.** Run the whole suite, then `git switch -c feat/catalog-startup-load`.

---

# PR C3: startup load, recovery, shutdown

### Task 6: Load the catalog at startup

**Files:**
- Create: `internal/engine/catalogload.go`, `internal/engine/restart_test.go`
- Modify: `internal/engine/catalog.go`, `database.go`, `table.go` (constructors for loaded objects), `testhelpers_test.go`

**Interfaces:**
- Consumes: Task 4 store (`tableRecord`, `decodeDatabaseName`), `DecodeSchema`, `storage.OpenFile`.
- Produces:
  - `type loadedTable struct { name string; fileID int64; columns []parser.ColumnDefinition; path string; file storage.File }`
  - `type loadedCatalog struct { databases map[string][]loadedTable }` (every database, including those with no tables)
  - `(*catalogStore).load() (*loadedCatalog, error)`: resolves rows (spec 6.3 steps 3-5), tombstones losers, primes `dbRows`/`tableRows`, opens survivors' files; on any error closes what it opened.
  - `newLoadedDatabase(name string, store *catalogStore, tables []loadedTable) Database`
  - `NewCatalog` now loads.

- [ ] **Step 1: Add test helpers and write the failing restart tests**

Append to `testhelpers_test.go`:

```go
func mustExec(t *testing.T, e Engine, database, sql string) Response {
	t.Helper()
	statement, err := parser.NewParser(parser.NewLexer()).Parse(sql)
	require.NoError(t, err, sql)
	resp, err := e.Execute(statement, database)
	require.NoError(t, err, sql)
	return resp
}

func execError(t *testing.T, e Engine, database, sql string) error {
	t.Helper()
	statement, err := parser.NewParser(parser.NewLexer()).Parse(sql)
	require.NoError(t, err, sql)
	_, err = e.Execute(statement, database)
	return err
}

// restart closes old and opens a new catalog over the same data directory,
// the way a server restart does.
func restart(t *testing.T, dir string, old Catalog) (Catalog, Engine) {
	t.Helper()
	require.NoError(t, old.Close())
	c := newTestCatalogAt(t, dir)
	return c, NewEngine(c)
}
```

`restart_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const usersDDL = "CREATE TABLE users (id INT PRIMARY KEY, name VARCHAR(20) NOT NULL DEFAULT 'anon', active BOOLEAN)"

func TestRestart_KeepsDatabasesSchemasAndRows(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "", "CREATE DATABASE empty")
	mustExec(t, e, "shop", usersDDL)
	mustExec(t, e, "shop", "INSERT INTO users (id, name, active) VALUES (1, 'ann', 1), (2, 'bob', 0)")
	mustExec(t, e, "shop", "INSERT INTO users (id) VALUES (3)")

	c, e = restart(t, dir, c)

	assert.Equal(t, []string{"empty", "shop"}, c.ListDatabases())
	resp := mustExec(t, e, "shop", "SELECT * FROM users")
	assert.Equal(t, []string{"id", "name", "active"}, resp.Columns)
	assert.Equal(t, [][]string{{"1", "ann", "true"}, {"2", "bob", "false"}, {"3", "anon", "NULL"}}, resp.Rows)

	t.Run("constraints survive: the primary key still rejects a duplicate", func(t *testing.T) {
		err := execError(t, e, "shop", "INSERT INTO users (id) VALUES (1)")
		assert.ErrorContains(t, err, "duplicate")
	})
	t.Run("NOT NULL with a default survives", func(t *testing.T) {
		mustExec(t, e, "shop", "INSERT INTO users (id) VALUES (4)")
		resp := mustExec(t, e, "shop", "SELECT name FROM users WHERE id = 4")
		assert.Equal(t, [][]string{{"anon"}}, resp.Rows)
	})
}

func TestRestart_KeepsDeletesAndUpdates(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", usersDDL)
	mustExec(t, e, "shop", "INSERT INTO users (id, name) VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	mustExec(t, e, "shop", "DELETE FROM users WHERE id = 2")
	mustExec(t, e, "shop", "UPDATE users SET name = 'z' WHERE id = 3")

	_, e = restart(t, dir, c)

	resp := mustExec(t, e, "shop", "SELECT id, name FROM users")
	assert.ElementsMatch(t, [][]string{{"1", "a"}, {"3", "z"}}, resp.Rows)
}

func TestRestart_AfterDDL(t *testing.T) {
	tests := map[string]struct {
		ddl    []string
		verify func(t *testing.T, e Engine)
	}{
		"drop table": {
			ddl: []string{"DROP TABLE users"},
			verify: func(t *testing.T, e Engine) {
				assert.ErrorContains(t, execError(t, e, "shop", "SELECT * FROM users"), `table "users" does not exist`)
			},
		},
		"rename table": {
			ddl: []string{"ALTER TABLE users RENAME TO people"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT id FROM people")
				assert.Equal(t, [][]string{{"1"}}, resp.Rows)
				assert.Error(t, execError(t, e, "shop", "SELECT * FROM users"))
			},
		},
		"add column": {
			ddl: []string{"ALTER TABLE users ADD COLUMN age INT DEFAULT 7"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT id, age FROM users")
				assert.Equal(t, [][]string{{"1", "7"}}, resp.Rows)
			},
		},
		"drop column": {
			ddl: []string{"ALTER TABLE users DROP COLUMN active"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT * FROM users")
				assert.Equal(t, []string{"id", "name"}, resp.Columns)
			},
		},
		"rename column": {
			ddl: []string{"ALTER TABLE users RENAME COLUMN name TO title"},
			verify: func(t *testing.T, e Engine) {
				resp := mustExec(t, e, "shop", "SELECT title FROM users")
				assert.Equal(t, [][]string{{"x"}}, resp.Rows)
			},
		},
		"drop database": {
			ddl: []string{"DROP DATABASE shop"},
			verify: func(t *testing.T, e Engine) {
				assert.Equal(t, [][]string{}, mustExec(t, e, "", "SHOW DATABASES").Rows)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := newTestCatalogAt(t, dir)
			e := NewEngine(c)
			mustExec(t, e, "", "CREATE DATABASE shop")
			mustExec(t, e, "shop", usersDDL)
			mustExec(t, e, "shop", "INSERT INTO users (id, name, active) VALUES (1, 'x', 1)")
			for _, sql := range tt.ddl {
				mustExec(t, e, "shop", sql)
			}

			_, e = restart(t, dir, c)

			tt.verify(t, e)
		})
	}
}

func TestRestart_IsStableAndNeverReusesFileIDs(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", usersDDL)
	db, _ := c.GetDatabase("shop")
	table, _ := db.GetTable("users")
	first := table.(*SqlTable).fileID

	c, e = restart(t, dir, c)
	c, e = restart(t, dir, c)

	mustExec(t, e, "shop", "CREATE TABLE orders (id INT)")
	db, _ = c.GetDatabase("shop")
	users, _ := db.GetTable("users")
	orders, _ := db.GetTable("orders")
	assert.Equal(t, first, users.(*SqlTable).fileID, "a table keeps its file id across restarts")
	assert.Greater(t, orders.(*SqlTable).fileID, first)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/engine/ -run TestRestart -v`
Expected: FAIL (databases empty after restart; `SHOW DATABASES` shows nothing).

- [ ] **Step 3: Implement the loader**

`catalogload.go`:

```go
package engine

import (
	"errors"
	"fmt"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// loadedTable is one table read back from sys_tables, with its file open.
type loadedTable struct {
	name    string
	fileID  int64
	columns []parser.ColumnDefinition
	path    string
	file    storage.File
}

type loadedCatalog struct {
	// databases maps every live database to its tables (possibly none).
	databases map[string][]loadedTable
}

// survivor is a sys_tables row that is still live while the scan resolves
// duplicates.
type survivor struct {
	record tableRecord
	id     storage.RowID
	dead   bool
}

// load reads the system tables (spec 6.3 steps 3 to 5) and opens every
// surviving table's file. It never deletes a table file: that is cleanup's
// job, once every survivor is known. On any error every file it opened is
// closed again.
func (s *catalogStore) load() (*loadedCatalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Databases. A repeated name keeps the first row.
	var staleDatabases []storage.RowID
	for row, err := range s.databases.Scan() {
		if err != nil {
			return nil, fmt.Errorf("reading sys_databases: %w", err)
		}
		name, err := decodeDatabaseName(row.Bytes)
		if err != nil {
			return nil, err
		}
		if _, dup := s.dbRows[name]; dup {
			staleDatabases = append(staleDatabases, row.ID)
			continue
		}
		s.dbRows[name] = row.ID
	}

	// Tables. A later row displaces any current survivor that shares its
	// (db, name) or its file_id. Displacement is checked against survivors,
	// not against every earlier row: rows [a,f1] [b,f1] [a,f2] leave b on f1
	// and a on f2, and f1 must stay because b uses it.
	byName := map[tableKey]*survivor{}
	byID := map[int64]*survivor{}
	var all []*survivor
	var staleTables []storage.RowID
	displace := func(old *survivor) {
		if old == nil || old.dead {
			return
		}
		old.dead = true
		staleTables = append(staleTables, old.id)
		if byName[tableKey{old.record.database, old.record.name}] == old {
			delete(byName, tableKey{old.record.database, old.record.name})
		}
		if byID[old.record.fileID] == old {
			delete(byID, old.record.fileID)
		}
	}
	for row, err := range s.tables.Scan() {
		if err != nil {
			return nil, fmt.Errorf("reading sys_tables: %w", err)
		}
		record, err := decodeTableRecord(row.Bytes)
		if err != nil {
			return nil, err
		}
		if _, ok := s.dbRows[record.database]; !ok {
			staleTables = append(staleTables, row.ID)
			continue
		}
		key := tableKey{record.database, record.name}
		displace(byName[key])
		displace(byID[record.fileID])
		sv := &survivor{record: record, id: row.ID}
		byName[key], byID[record.fileID] = sv, sv
		all = append(all, sv)
	}

	// Tombstone the losers, after the scans so no page is rewritten while it
	// is being read.
	if err := tombstoneAll(s.databases, staleDatabases); err != nil {
		return nil, err
	}
	if err := tombstoneAll(s.tables, staleTables); err != nil {
		return nil, err
	}

	loaded := &loadedCatalog{databases: make(map[string][]loadedTable, len(s.dbRows))}
	for name := range s.dbRows {
		loaded.databases[name] = nil
	}
	closeAll := func() {
		for _, tables := range loaded.databases {
			for _, table := range tables {
				_ = table.file.Close()
			}
		}
	}
	for _, sv := range all {
		if sv.dead {
			continue
		}
		record := sv.record
		columns, err := DecodeSchema(record.schema)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("table %q in database %q: %w", record.name, record.database, err)
		}
		path := s.files.tablePath(record.database, record.fileID)
		file, err := storage.OpenFile(path)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("table %q in database %q: its file is missing or damaged: %w", record.name, record.database, err)
		}
		s.tableRows[tableKey{record.database, record.name}] = sv.id
		loaded.databases[record.database] = append(loaded.databases[record.database], loadedTable{
			name: record.name, fileID: record.fileID, columns: columns, path: path, file: file,
		})
	}
	return loaded, nil
}

func tombstoneAll(file storage.File, ids []storage.RowID) error {
	if len(ids) == 0 {
		return nil
	}
	var errs []error
	for _, id := range ids {
		if err := file.Delete(id); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("tombstoning stale catalog rows: %w", err)
	}
	return file.Sync()
}
```

Note: `s.tableRows` priming happens only for survivors; when `load` fails halfway the store is discarded by `NewCatalog` (it calls `store.Close()`), so partially primed maps do not matter.

In `database.go` add:

```go
// newLoadedDatabase rebuilds a database from its catalog rows. Its directory
// and files already exist, so nothing is created or written.
func newLoadedDatabase(name string, store *catalogStore, tables []loadedTable) Database {
	d := &SqlDatabase{Name: name, Table: make(map[string]Table, len(tables)), store: store}
	for _, lt := range tables {
		d.Table[lt.name] = &SqlTable{
			Name: lt.name, Columns: lt.columns,
			path: lt.path, file: lt.file, fileID: lt.fileID,
			store: store, database: name,
		}
	}
	return d
}
```

In `catalog.go`, `NewCatalog` after `openCatalogStore`:

```go
	loaded, err := store.load()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("loading catalog: %w", err)
	}
	c := &SqlCatalog{Databases: make(map[string]Database, len(loaded.databases)), store: store}
	for name, tables := range loaded.databases {
		c.Databases[name] = newLoadedDatabase(name, store, tables)
	}
	return c, nil
```

- [ ] **Step 4: Run, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: PASS, including every earlier test (they now run through `NewCatalog` over fresh temp dirs).

```bash
git add internal/engine
git commit -m "feat(engine): load databases and tables from the catalog at startup

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 7: Crash recovery and orphan cleanup

**Files:**
- Create: `internal/engine/catalogcleanup.go`, `internal/engine/crash_test.go`
- Modify: `internal/engine/catalog.go` (call cleanup), `internal/engine/files.go` if a helper fits there

**Interfaces:**
- Produces: `removeOrphans(files *fileAllocator, databases map[string]bool, referenced map[int64]bool)`: removes `<number>.tbl` files in any directory under `data/` whose id is not referenced, then removes empty directories under `data/` whose name is not a database. Best effort: errors are ignored (an orphan is harmless). Never recursive.

- [ ] **Step 1: Write the failing crash-state tests**

`crash_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

// crashFixture builds a data directory by hand, the way a crash would leave
// it: catalog rows and files written independently of each other.
type crashFixture struct {
	t     *testing.T
	dir   string
	store *catalogStore
}

func newCrashFixture(t *testing.T) *crashFixture {
	dir := t.TempDir()
	return &crashFixture{t: t, dir: dir, store: newTestStoreAt(t, dir)}
}

func (f *crashFixture) database(name string) {
	require.NoError(f.t, f.store.files.makeDir(f.store.files.databaseDir(name)))
	require.NoError(f.t, f.store.addDatabase(name))
}

// row writes a sys_tables row without creating its file.
func (f *crashFixture) row(db, name string, id int64) {
	require.NoError(f.t, f.store.addTable(db, name, mustEncodeTableRow(f.t, db, name, id, idColumn)))
}

// file creates an empty table file with the given id.
func (f *crashFixture) file(db string, id int64) string {
	require.NoError(f.t, f.store.files.makeDir(f.store.files.databaseDir(db)))
	path := f.store.files.tablePath(db, id)
	file, err := storage.CreateFile(path)
	require.NoError(f.t, err)
	require.NoError(f.t, file.Close())
	return path
}

func (f *crashFixture) open() (Catalog, error) {
	require.NoError(f.t, f.store.Close())
	return NewCatalog(DataDir(f.dir))
}

func (f *crashFixture) mustOpen() Catalog {
	c, err := f.open()
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func hasTable(t *testing.T, c Catalog, db, name string) bool {
	t.Helper()
	d, err := c.GetDatabase(db)
	require.NoError(t, err)
	_, ok := d.GetTable(name)
	return ok
}

func TestStartup_RemovesAnOrphanFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	live := f.file("shop", 1)
	orphan := f.file("shop", 2) // created, but the catalog row was never written

	f.mustOpen()

	assert.True(t, exists(t, live))
	assert.False(t, exists(t, orphan))
}

func TestStartup_RemovesAnOrphanDirectoryAndItsFiles(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	stray := f.file("ghost", 5)
	require.NoError(t, os.MkdirAll(filepath.Join(f.dir, "data", "empty_ghost"), 0o755))

	f.mustOpen()

	assert.False(t, exists(t, stray))
	assert.False(t, exists(t, filepath.Dir(stray)))
	assert.False(t, exists(t, filepath.Join(f.dir, "data", "empty_ghost")))
}

func TestStartup_NeverRemovesWhatItDidNotCreate(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	note := filepath.Join(f.dir, "data", "shop", "notes.txt")
	require.NoError(t, os.WriteFile(note, []byte("mine"), 0o644))
	stray := filepath.Join(f.dir, "data", "README")
	require.NoError(t, os.WriteFile(stray, []byte("mine"), 0o644))

	f.mustOpen()

	assert.True(t, exists(t, note), "a file that is not <id>.tbl is left alone")
	assert.True(t, exists(t, stray), "files directly under data/ are left alone")
}

func TestStartup_KeepsAnEmptyDatabaseDirectory(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")

	c := f.mustOpen()

	assert.Equal(t, []string{"shop"}, c.ListDatabases())
}

func TestStartup_DropsATableRowWhoseDatabaseIsGone(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("gone", "users", 3)
	orphan := f.file("gone", 3)

	c := f.mustOpen()

	assert.Equal(t, []string{"shop"}, c.ListDatabases())
	assert.False(t, exists(t, orphan))
	// the row is tombstoned for good: a database of that name starts empty
	require.NoError(t, c.CreateDatabase("gone"))
	assert.False(t, hasTable(t, c, "gone", "users"))
}

func TestStartup_DuplicateRowsAfterAnAlterKeepTheLaterFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	f.row("shop", "users", 2) // the ALTER's new row; the old row was never tombstoned
	old := f.file("shop", 1)
	fresh := f.file("shop", 2)

	c := f.mustOpen()

	assert.True(t, hasTable(t, c, "shop", "users"))
	db, _ := c.GetDatabase("shop")
	table, _ := db.GetTable("users")
	assert.Equal(t, int64(2), table.(*SqlTable).fileID)
	assert.False(t, exists(t, old))
	assert.True(t, exists(t, fresh))
}

func TestStartup_DuplicateRowsAfterARenameKeepTheNewNameAndTheSharedFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	f.row("shop", "people", 1) // the rename's new row; the old row was never tombstoned
	shared := f.file("shop", 1)

	c := f.mustOpen()

	assert.False(t, hasTable(t, c, "shop", "users"))
	assert.True(t, hasTable(t, c, "shop", "people"))
	assert.True(t, exists(t, shared), "the surviving table still uses this file")
}

func TestStartup_RenameThenAlterChainKeepsSharedFile(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "a", 1)
	f.row("shop", "b", 1) // rename a -> b, old row lost
	f.row("shop", "a", 2) // a new table named a
	f1 := f.file("shop", 1)
	f2 := f.file("shop", 2)

	c := f.mustOpen()

	assert.True(t, hasTable(t, c, "shop", "a"))
	assert.True(t, hasTable(t, c, "shop", "b"))
	assert.True(t, exists(t, f1), "b still uses f1")
	assert.True(t, exists(t, f2))
	db, _ := c.GetDatabase("shop")
	a, _ := db.GetTable("a")
	b, _ := db.GetTable("b")
	assert.Equal(t, int64(2), a.(*SqlTable).fileID)
	assert.Equal(t, int64(1), b.(*SqlTable).fileID)
}

func TestStartup_LiveRowWithAMissingFileFailsAndClosesWhatItOpened(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.row("shop", "users", 1)
	f.row("shop", "orders", 2)
	f.file("shop", 1) // orders' file is missing

	c, err := f.open()

	require.Error(t, err)
	assert.Nil(t, c)
	assert.ErrorContains(t, err, `table "orders"`)
	assert.ErrorContains(t, err, "missing or damaged")
}

func TestStartup_TornSystemFileFailsStartup(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	require.NoError(t, f.store.Close())
	path := filepath.Join(f.dir, "sys", sysDatabasesFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.Write([]byte("torn"))
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = NewCatalog(DataDir(f.dir))

	assert.Error(t, err)
}

func TestStartup_NeverReusesAnIDEvenOfAnOrphan(t *testing.T) {
	f := newCrashFixture(t)
	f.database("shop")
	f.file("shop", 40) // orphan with the highest id

	c := f.mustOpen()
	require.NoError(t, c.Close())
	c2 := newTestCatalogAt(t, f.dir)

	db, _ := c2.GetDatabase("shop")
	require.NoError(t, db.CreateTable("t", idColumn, false))
	table, _ := db.GetTable("t")
	assert.Greater(t, table.(*SqlTable).fileID, int64(40))
}

func TestStartup_CaseTwinDatabasesKeepTheirFiles(t *testing.T) {
	probe := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(probe, "a"), 0o755))
	if _, err := os.Stat(filepath.Join(probe, "A")); err != nil {
		t.Skip("filesystem is case-sensitive; Shop and shop do not share a directory here")
	}

	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE Shop")
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "Shop", "CREATE TABLE a (id INT)")
	mustExec(t, e, "Shop", "INSERT INTO a (id) VALUES (1)")
	mustExec(t, e, "shop", "CREATE TABLE b (id INT)")
	mustExec(t, e, "shop", "INSERT INTO b (id) VALUES (2)")

	_, e = restart(t, dir, c)

	assert.Equal(t, [][]string{{"1"}}, mustExec(t, e, "Shop", "SELECT id FROM a").Rows)
	assert.Equal(t, [][]string{{"2"}}, mustExec(t, e, "shop", "SELECT id FROM b").Rows)
}

func TestRestart_DroppedDatabaseWithStaleRowsIsNotResurrected(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE users (id INT)")
	failSysTables(t, storeOf(c)).failDeletes = 1
	require.Error(t, execError(t, e, "", "DROP DATABASE shop"))
	mustExec(t, e, "", "CREATE DATABASE shop")

	_, e = restart(t, dir, c)

	assert.ErrorContains(t, execError(t, e, "shop", "SELECT * FROM users"), `table "users" does not exist`)
}
```

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/engine/ -run 'TestStartup|TestRestart_Dropped' -v`
Expected: orphan tests FAIL (files still there); the duplicate-row and chain tests may already pass because `load` resolves them (that is fine: they pin the behavior).

- [ ] **Step 3: Implement cleanup**

`catalogcleanup.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// removeOrphans deletes what no surviving catalog row refers to. Ids are
// global and never reused, so a table file is an orphan exactly when no
// survivor has its id, whatever directory it sits in; a directory name is
// never trusted, because on a case-insensitive filesystem two databases can
// share one. Only files named <number>.tbl are removed, and a directory is
// removed only if it is empty and no database has its exact name, with a
// non-recursive os.Remove: it can never take a file it did not create. A
// removed directory that a database still needs is recreated by its next
// CREATE TABLE.
//
// Failures are ignored: an orphan wastes space and nothing else.
func removeOrphans(files *fileAllocator, databases map[string]bool, referenced map[int64]bool) {
	root := filepath.Join(string(files.dataDir), "data")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		children, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		remaining := 0
		for _, child := range children {
			if child.IsDir() {
				remaining++
				continue
			}
			idText, isTable := strings.CutSuffix(child.Name(), tableFileExt)
			id, parseErr := strconv.ParseInt(idText, 10, 64)
			if !isTable || parseErr != nil || referenced[id] {
				remaining++
				continue
			}
			if os.Remove(filepath.Join(dir, child.Name())) != nil {
				remaining++
			}
		}
		if remaining == 0 && !databases[entry.Name()] {
			_ = os.Remove(dir)
		}
	}
}
```

In `NewCatalog`, after building `c` (all survivors are open) and before returning:

```go
	databases := make(map[string]bool, len(loaded.databases))
	referenced := make(map[int64]bool)
	for name, tables := range loaded.databases {
		databases[name] = true
		for _, table := range tables {
			referenced[table.fileID] = true
		}
	}
	removeOrphans(files, databases, referenced)
```

(`files` is the allocator created earlier in `NewCatalog`; it scanned ids before this cleanup, so ids of removed orphans are never reused.)

- [ ] **Step 4: Run, mutation-check, commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: PASS. Mutation checks (revert each): make `load`'s `displace(byID[...])` a no-op (the rename-chain and rename tests must fail); make `displace` delete files (not possible by construction: instead check the chain test by comparing against displacing from *all* earlier rows: replace `byName/byID` lookups by a scan over `all` and confirm `TestStartup_RenameThenAlterChainKeepsSharedFile` fails); change orphan cleanup to key on directory name; use `os.RemoveAll` in place of `os.Remove`; skip the `.tbl` name check. Each must fail a test above.

```bash
git add internal/engine
git commit -m "feat(engine): recover from a crash and remove orphan files at startup

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

### Task 8: Close on shutdown, docs

**Files:**
- Modify: `internal/wiring/providers.go`, `wire.go`, `wire_gen.go` (regenerate), `wiring_test.go`, `internal/engine/wireset.go`, `cmd/server/main.go`, `CLAUDE.md`, `plan/on-disk-persistence-design.md` (spec wording)

**Interfaces:**
- Produces: `wiring.ProvideCatalog(dataDir engine.DataDir) (engine.Catalog, func(), error)`; `InitializeServer(cfg) (*grpcserver.Server, func(), error)`.

- [ ] **Step 1: Update and extend the wiring tests**

`wiring_test.go`: both existing subtests take three results (`server, cleanup, err := InitializeServer(cfg)`), call `cleanup()` (via `t.Cleanup`) in the success case, and the failure case ignores it. Add:

```go
func TestInitializeServer_DataSurvivesARestart(t *testing.T) {
	cfg := &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}}
	query := func(server *grpcserver.Server, database, sql string) *sqlpb.QueryResponse {
		resp, err := server.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: sql, Database: database})
		require.NoError(t, err)
		require.Equal(t, sqlpb.StatusCode_OK, resp.Code, resp.ErrorMessage)
		return resp
	}

	server, cleanup, err := InitializeServer(cfg)
	require.NoError(t, err)
	query(server, "", "CREATE DATABASE shop")
	query(server, "shop", "CREATE TABLE users (id INT PRIMARY KEY)")
	query(server, "shop", "INSERT INTO users (id) VALUES (1)")
	cleanup()

	server, cleanup, err = InitializeServer(cfg)
	require.NoError(t, err)
	defer cleanup()

	resp := query(server, "shop", "SELECT id FROM users")
	require.Len(t, resp.Rows, 1)
	assert.Equal(t, []string{"1"}, resp.Rows[0].Values)
}

func TestProvideCatalog_CleanupClosesTheCatalog(t *testing.T) {
	catalog, cleanup, err := ProvideCatalog(engine.DataDir(t.TempDir()))
	require.NoError(t, err)
	require.NoError(t, catalog.CreateDatabase("shop"))
	db, err := catalog.GetDatabase("shop")
	require.NoError(t, err)
	require.NoError(t, db.CreateTable("users", []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}, false))
	table, _ := db.GetTable("users")

	cleanup()

	_, err = table.Select([]string{"*"}, nil)
	assert.ErrorContains(t, err, "is closed")
}
```

(imports: `context`, `engine`, `grpcserver`, `parser`, `sqlpb`.)

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/wiring/`
Expected: build failures (`ProvideCatalog` undefined, `InitializeServer` returns two values).

- [ ] **Step 3: Implement**

`providers.go`:

```go
// ProvideCatalog opens the catalog and returns the function that closes it, so
// the server releases its file handles on shutdown.
func ProvideCatalog(dataDir engine.DataDir) (engine.Catalog, func(), error) {
	catalog, err := engine.NewCatalog(dataDir)
	if err != nil {
		return nil, nil, err
	}
	return catalog, func() {
		if err := catalog.Close(); err != nil {
			log.Printf("closing catalog: %v", err)
		}
	}, nil
}
```

`engine/wireset.go`: remove `NewCatalog` from the set (keep `NewEngine`). `wiring/wire.go`: add `ProvideCatalog` to `WireSet`; the injector becomes `func InitializeServer(cfg *config.Config) (*grpcserver.Server, func(), error) { wire.Build(WireSet); return nil, nil, nil }`. Regenerate: `PATH="$(go env GOPATH)/bin:$PATH" make generate`.

`cmd/server/main.go`: `server, cleanup, err := wiring.InitializeServer(cfg)`. After that point every exit closes the catalog: replace the two later `log.Fatalf` calls with a call to `cleanup()` followed by `log.Fatalf`, and call `cleanup()` after `grpcServer.GracefulStop()` in the shutdown branch (before logging "server stopped").

- [ ] **Step 4: Update docs**

`CLAUDE.md`: in Project overview replace "in-memory execution engine" with "disk-backed execution engine". Replace the Storage paragraph's catalog-related sentences with: the catalog lives in two system heap files under `<storage.data_dir>/sys/` (`sys_databases`, `sys_tables`; schemas are JSON, see `schemacodec.go`); every DDL statement writes its catalog row and fsyncs before changing memory, so the catalog row is the commit point; at startup `catalogStore.load` resolves duplicate rows left by a crash against current survivors, opens every table's file, and `removeOrphans` deletes `<id>.tbl` files no survivor references (by id, never by directory name, never recursively); a missing or torn file refuses startup; `Catalog.Close()` runs on shutdown through the Wire cleanup function; a schema too large for one page cannot be created. Update the engine bullet about locks to the full order `Catalog.mu → Database.mu → Table.mu → store.mu`.

Spec `plan/on-disk-persistence-design.md` section 6.3 step 5: change "a corrupt page or a torn file is a startup error" to "a missing or torn table file, or any damaged page in a system file, is a startup error (a damaged page inside a table file shows up when that table is first scanned)".

- [ ] **Step 5: Verify everything and commit**

Run: `gofmt -l . ; go vet ./... && go build ./... && go test -race ./...` and, for the engine and storage packages, three repeated runs with `-count=3`.
Then a manual smoke (no network needed): `go run ./cmd/server -config internal/config/config.yml` in a scratch dir with a temp `data_dir`, send SIGTERM, confirm "server stopped" and no error, then remove the temp dir. Skip if the config's port is busy; the wiring test already covers restart end to end.

```bash
git add -A cmd internal CLAUDE.md
git commit -m "feat(engine): persist the catalog and close it on shutdown

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

(`plan/` is gitignored; do not add it.)

**C3 done.** Fresh whole-branch review of each PR range (fable model), fix Critical/Important findings with RED-first tests, ledger minors, update `memory/project_on_disk_persistence.md`. Stop before pushing; push and open the three PRs only when asked, in stack order (C1 based on `main`, C2 on C1's branch, C3 on C2's branch), with PR bodies in the usual one-line-per-bullet Summary + Test plan format.

## Self-review notes

- **Spec coverage:** 6.1 system tables (Task 4), 6.2 codec (3), 6.3 startup (6, 7), 6.4 ordering incl. two-sync replace, stale-row sweep, encode-before-create (4, 5), 6.5 directory durability (1), 6.6 locking (store never calls back; Global Constraints), 6.7 wiring and shutdown (8), 6.8 tests (each task), 7 limitations (docs in 8).
- **Known unobservable behaviours (ledger, not tests):** directory fsync in `CreateFile`/`makeDir`; the first of replace's two syncs.
- **Residual risk (accepted, ledger as ruling):** if `Sync` itself fails after `tombstoneTablesLocked` removed index entries, a stale `sys_tables` row may survive without an index entry, so a same-name `CREATE DATABASE` in the same process would not sweep it; startup removes it as long as the database row stays gone, but recreating the database before restart could resurrect it. Requires an fsync failure on top of a failed DROP; not handled.
