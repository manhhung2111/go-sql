# PR A: Row Codec and Per-Table Heap Files Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the row codec and give every `SqlTable` its own empty heap file under a configured data directory, with a correct create/drop lifecycle, while all statements keep running against in-memory `Rows`.

**Architecture:** `internal/engine` gains `rowcodec.go` (rows to bytes and back, using the per-column offset table) and `files.go` (a `fileAllocator` that hands out `<data_dir>/data/<db>/<file_id>.tbl` paths). `NewCatalog(DataDir)` builds the allocator; `Database` owns its directory and `Table` owns its file. Nothing writes rows to the files yet (that is PR B), so behaviour visible to SQL clients does not change.

**Tech Stack:** Go 1.26.4, `internal/storage` (`CreateFile`, merged in PR #4), testify, Google Wire.

**Spec:** `plan/on-disk-persistence-design.md` (sections 3, 5.1, 5.2).

## Prerequisite

PR #4 (storage) is merged. Start from fresh `main`:

```bash
git checkout main && git pull --ff-only origin main
git checkout -b feat/engine-table-files
```

## Global Constraints

- Go 1.26.4; module path `manhhung2111/go-sql`.
- `internal/engine` must never import `proto/sqlpb`; `internal/storage` must never import `internal/parser`.
- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers.
- Tests use testify (`assert`/`require`), table-driven where there is a matrix of cases, and `t.TempDir()` for anything on disk.
- `gofmt -l .` must print nothing; `go vet ./...` and `go build ./...` must pass; run `go test -race ./...` before finishing (the engine is concurrent). The code blocks in this plan are not guaranteed to be gofmt-aligned (for example trailing comments in literals), so run `gofmt -w` on each file you create or edit before the `gofmt -l` checks.
- Commits are Conventional Commits (`<type>(scope): description`) ending with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Project rule: only commit when the user asks, so each "Commit" step below marks an intended commit boundary, not permission to commit unprompted.
- `plan/` is gitignored; never commit it.
- Row codec layout (from the spec): per column an `(offset uint16, length uint16)` entry, little-endian, then the payloads in column order; offsets are measured from the start of the row; offset `0xFFFF` means NULL; int64 is 8 bytes little-endian, bool is 1 byte, strings are raw bytes; `numCols` is not stored.
- File ids: files are named `<file_id>.tbl`, never by table name; ids are never reused; at startup the next id is `1 + max id found under <data_dir>/data/`.

## Review Focus

Failure modes the spec implies but that are easy to miss; each has a test in the task that owns the code:

1. A database name with a path separator or `..` must be rejected and must never create anything outside `<data_dir>/data` (Task 2 `TestValidateDatabaseName`, Task 4 `TestLifecycle_CreateDatabaseRejectsPathLikeNames`).
2. `CREATE DATABASE` when its directory cannot be created must return an error and leave no phantom database in `SHOW DATABASES` (Task 4 `TestLifecycle_CreateDatabaseFailureLeavesNoPhantomDatabase`).
3. A `CREATE TABLE` rejected by schema validation must not leave an orphan file behind (Task 4 `TestLifecycle_CreateTableWithInvalidSchemaLeavesNoFile`).
4. A server restarted on a populated data directory must never hand out an id that collides with a leftover file (Task 2 `continues after the highest id already on disk`, Task 4 `TestLifecycle_RestartContinuesFileIDs`).
5. Concurrent `CREATE TABLE`s must get distinct files, and `DROP DATABASE` must never delete a file it did not create, because database names that differ only by case can share a directory on case-insensitive filesystems (Task 4 `TestLifecycle_ConcurrentCreateTableGetsDistinctFiles`, `TestLifecycle_DropDatabaseLeavesForeignFilesAlone`).

Codec robustness (corrupt bytes must be an error, never a panic; `""` and NULL must differ; zero columns must work) is covered inside Task 1.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/engine/table_test.go` | Modify (Task 1) | rename helper `binary` to `binaryExpr` so `encoding/binary` can be imported |
| `internal/engine/rowcodec.go` | Create | `EncodeRow` / `DecodeRow` |
| `internal/engine/rowcodec_test.go` | Create | codec tests |
| `internal/engine/files.go` | Create | `DataDir`, `fileAllocator`, `validateDatabaseName` |
| `internal/engine/files_test.go` | Create | allocator and name-validation tests |
| `internal/config/config.go` | Modify | `Storage.DataDir` setting with default |
| `internal/config/config.yml` | Modify | add `storage.data_dir` |
| `internal/config/config_test.go` | Create | config loading tests |
| `internal/engine/table.go` | Modify | table owns a `storage.File`; `Close`, `Drop` |
| `internal/engine/database.go` | Modify | database owns its directory; `Close`, `Drop`; allocator use |
| `internal/engine/catalog.go` | Modify | `NewCatalog(DataDir)`; `Close`; drop removes files |
| `internal/engine/testhelpers_test.go` | Create | `newTestCatalog`, `newTestDatabase`, `newTempTable`, `tableFiles` |
| `internal/engine/lifecycle_test.go` | Create | create/drop/restart/concurrency tests |
| `internal/engine/{engine,catalog,database,table}_test.go` | Modify | mechanical constructor swaps |
| `internal/grpcserver/server_test.go` | Modify | build the catalog with a temp data dir |
| `internal/wiring/providers.go` | Create | `ProvideDataDir` |
| `internal/wiring/wire.go`, `wire_gen.go` | Modify | provide `DataDir`, propagate `NewCatalog`'s error |
| `internal/wiring/wiring_test.go` | Create | `InitializeServer` tests |
| `.gitignore`, `CLAUDE.md` | Modify | ignore `/data`; document the storage layout |

---

### Task 1: Row codec

**Files:**
- Create: `internal/engine/rowcodec.go`
- Test: `internal/engine/rowcodec_test.go`

**Interfaces:**
- Consumes: `parser.ColumnDefinition`, `parser.DataType` variants (`CharDataType`, `VarCharDataType`, `TextDataType`, `BooleanDataType`, `SmallIntDataType`, `MediumIntDataType`, `IntDataType`, `BigIntDataType`). Row values are what `coerceValue` returns: `string`, `int64`, `bool`, or `nil` for NULL.
- Produces:
  - `func EncodeRow(columns []parser.ColumnDefinition, row []any) ([]byte, error)`
  - `func DecodeRow(columns []parser.ColumnDefinition, b []byte) ([]any, error)`

- [ ] **Step 0: Rename the test helper that collides with `encoding/binary`**

`internal/engine/table_test.go` declares a package-level helper `func binary(...)`. Go does not allow an import name (`encoding/binary`, which this task and later PRs import in the engine package) to share a name with a package-level identifier, so rename the helper first:

```bash
sed -i '' 's/binary(/binaryExpr(/g' internal/engine/table_test.go
grep -n 'binaryExpr(' internal/engine/table_test.go   # expect the declaration plus 2 call sites
go vet ./internal/engine
```

Expected: `go vet` passes.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/rowcodec_test.go`:

```go
package engine

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func codecColumns() []parser.ColumnDefinition {
	return []parser.ColumnDefinition{
		{Name: "id", DataType: parser.BigIntDataType{}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
		{Name: "active", DataType: parser.BooleanDataType{}},
	}
}

func TestRowCodec_RoundTrip(t *testing.T) {
	columns := codecColumns()
	tests := []struct {
		name string
		row  []any
	}{
		{"all values", []any{int64(7), "bob", true}},
		{"false boolean", []any{int64(1), "x", false}},
		{"negative int", []any{int64(-42), "neg", true}},
		{"min int64", []any{int64(math.MinInt64), "min", false}},
		{"max int64", []any{int64(math.MaxInt64), "max", true}},
		{"empty string", []any{int64(1), "", true}},
		{"unicode string", []any{int64(1), "héllo wörld 日本語", true}},
		{"NULL first", []any{nil, "bob", true}},
		{"NULL middle", []any{int64(1), nil, true}},
		{"NULL last", []any{int64(1), "bob", nil}},
		{"all NULL", []any{nil, nil, nil}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeRow(columns, tc.row)
			require.NoError(t, err)

			decoded, err := DecodeRow(columns, encoded)

			require.NoError(t, err)
			assert.Equal(t, tc.row, decoded)
		})
	}
}

func TestRowCodec_EveryDataType(t *testing.T) {
	columns := []parser.ColumnDefinition{
		{Name: "c", DataType: parser.CharDataType{Size: 10}},
		{Name: "v", DataType: parser.VarCharDataType{Size: 10}},
		{Name: "t", DataType: parser.TextDataType{}},
		{Name: "b", DataType: parser.BooleanDataType{}},
		{Name: "s", DataType: parser.SmallIntDataType{}},
		{Name: "m", DataType: parser.MediumIntDataType{}},
		{Name: "i", DataType: parser.IntDataType{}},
		{Name: "g", DataType: parser.BigIntDataType{}},
	}
	row := []any{"c", "v", "t", true, int64(-1), int64(2), int64(3), int64(4)}

	encoded, err := EncodeRow(columns, row)
	require.NoError(t, err)
	decoded, err := DecodeRow(columns, encoded)

	require.NoError(t, err)
	assert.Equal(t, row, decoded)
}

func TestEncodeRow_Layout(t *testing.T) {
	encoded, err := EncodeRow(codecColumns(), []any{int64(7), "bob", nil})
	require.NoError(t, err)

	want := []byte{
		12, 0, 8, 0, // id: starts at byte 12, 8 bytes long
		20, 0, 3, 0, // name: starts at byte 20, 3 bytes long
		0xFF, 0xFF, 0, 0, // active: NULL
		7, 0, 0, 0, 0, 0, 0, 0, // id payload, little-endian
		'b', 'o', 'b', // name payload
	}
	assert.Equal(t, want, encoded)
}

func TestEncodeRow_EmptyStringIsNotNull(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "s", DataType: parser.TextDataType{}}}

	empty, err := EncodeRow(columns, []any{""})
	require.NoError(t, err)
	null, err := EncodeRow(columns, []any{nil})
	require.NoError(t, err)

	assert.Equal(t, []byte{4, 0, 0, 0}, empty, "a real offset with length 0")
	assert.Equal(t, []byte{0xFF, 0xFF, 0, 0}, null, "the NULL sentinel")
}

func TestRowCodec_ZeroColumns(t *testing.T) {
	encoded, err := EncodeRow(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, encoded)

	decoded, err := DecodeRow(nil, encoded)
	require.NoError(t, err)
	assert.Empty(t, decoded)
}

func TestEncodeRow_Errors(t *testing.T) {
	tests := []struct {
		name    string
		columns []parser.ColumnDefinition
		row     []any
		wantErr string
	}{
		{"too few values", codecColumns(), []any{int64(1)}, "row has 1 values, table has 3 columns"},
		{"too many values", codecColumns(), []any{int64(1), "a", true, "extra"}, "row has 4 values, table has 3 columns"},
		{"string in an int column", codecColumns(), []any{"seven", "bob", true}, `column "id": expected int64, got string`},
		{"plain int instead of int64", codecColumns(), []any{7, "bob", true}, `column "id": expected int64, got int`},
		{"int in a string column", codecColumns(), []any{int64(1), int64(2), true}, `column "name": expected string, got int64`},
		{"string in a bool column", codecColumns(), []any{int64(1), "bob", "true"}, `column "active": expected bool, got string`},
		{
			"unsupported data type, even for NULL",
			[]parser.ColumnDefinition{{Name: "x", DataType: 42}},
			[]any{nil},
			`column "x": unsupported data type int`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EncodeRow(tc.columns, tc.row)

			assert.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestEncodeRow_SizeLimit(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "s", DataType: parser.TextDataType{}}}
	atLimit := strings.Repeat("a", maxCodecRowLen-rowEntrySize)

	encoded, err := EncodeRow(columns, []any{atLimit})
	require.NoError(t, err)
	assert.Len(t, encoded, maxCodecRowLen)
	decoded, err := DecodeRow(columns, encoded)
	require.NoError(t, err)
	assert.Equal(t, []any{atLimit}, decoded)

	_, err = EncodeRow(columns, []any{atLimit + "a"})
	assert.ErrorContains(t, err, "row too large")
}

func TestDecodeRow_RejectsCorruptBytes(t *testing.T) {
	columns := codecColumns()
	// valid is 24 bytes: a 12-byte offset table, then id at 12 (8 bytes),
	// name at 20 (3 bytes) and active at 23 (1 byte).
	valid, err := EncodeRow(columns, []any{int64(7), "bob", true})
	require.NoError(t, err)
	require.Len(t, valid, 24)

	tests := []struct {
		name    string
		mutate  func(b []byte) []byte
		wantErr string
	}{
		{"shorter than its offset table", func(b []byte) []byte { return b[:11] }, "shorter than its"},
		{"payload cut short", func(b []byte) []byte { return b[:len(b)-1] }, `column "active"`},
		{"offset points into the offset table", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[0:], 4)
			return b
		}, `column "id"`},
		{"offset beyond the row", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[4:], 1000)
			return b
		}, `column "name"`},
		{"int with the wrong length", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[2:], 4)
			return b
		}, `column "id": expected 8 bytes`},
		{"bool with the wrong length", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[10:], 0)
			return b
		}, `column "active": expected 1 byte`},
		{"bool that is neither 0 nor 1", func(b []byte) []byte {
			b[23] = 2
			return b
		}, `column "active": invalid boolean byte 2`},
		{"NULL entry that has a length", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[8:], nullOffset)
			binary.LittleEndian.PutUint16(b[10:], 1)
			return b
		}, `column "active": NULL entry has length 1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.mutate(append([]byte(nil), valid...))

			_, err := DecodeRow(columns, b)

			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestDecodeRow_UnsupportedDataType(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "x", DataType: 42}}

	_, err := DecodeRow(columns, []byte{0xFF, 0xFF, 0, 0})

	assert.EqualError(t, err, `column "x": unsupported data type int`)
}

func TestDecodeRow_StringsDoNotAliasTheInput(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "s", DataType: parser.TextDataType{}}}
	encoded, err := EncodeRow(columns, []any{"hello"})
	require.NoError(t, err)

	decoded, err := DecodeRow(columns, encoded)
	require.NoError(t, err)
	for i := range encoded {
		encoded[i] = 'X' // a scan's row bytes are only valid until the next row
	}

	assert.Equal(t, []any{"hello"}, decoded)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine -run 'RowCodec|EncodeRow|DecodeRow' -v`
Expected: build failure, `undefined: EncodeRow` (and `maxCodecRowLen`, `rowEntrySize`, `nullOffset`), and nothing about `binary` once Step 0 is done.

- [ ] **Step 3: Write the implementation**

Create `internal/engine/rowcodec.go`:

```go
package engine

import (
	"encoding/binary"
	"fmt"

	"manhhung2111/go-sql/internal/parser"
)

const (
	// rowEntrySize is one column's slot in the row's offset table:
	// offset uint16 + length uint16.
	rowEntrySize = 2 + 2
	// nullOffset marks a NULL column. A real payload can never start there
	// because maxCodecRowLen keeps every offset below it.
	nullOffset = 0xFFFF
	// maxCodecRowLen is the largest encoded row: offsets and lengths are
	// uint16, and 0xFFFF is reserved for NULL.
	maxCodecRowLen = nullOffset - 1
)

type valueKind int

const (
	kindString valueKind = iota
	kindInt
	kindBool
)

// kindOf maps a column's DataType to how its values are stored. Every integer
// type is an int64 in memory (coerceValue range-checks each against its own
// SQL range), so they all share one 8-byte encoding.
func kindOf(dt parser.DataType) (valueKind, error) {
	switch dt.(type) {
	case parser.CharDataType, parser.VarCharDataType, parser.TextDataType:
		return kindString, nil
	case parser.BooleanDataType:
		return kindBool, nil
	case parser.SmallIntDataType, parser.MediumIntDataType, parser.IntDataType, parser.BigIntDataType:
		return kindInt, nil
	default:
		return 0, fmt.Errorf("unsupported data type %T", dt)
	}
}

// EncodeRow turns a row of coerced values (string, int64, bool or nil for
// NULL, in column order) into bytes: an (offset, length) entry per column,
// then the payloads. Offsets are measured from the start of the returned
// slice. The column count is not stored; DecodeRow is given the schema.
func EncodeRow(columns []parser.ColumnDefinition, row []any) ([]byte, error) {
	if len(row) != len(columns) {
		return nil, fmt.Errorf("row has %d values, table has %d columns", len(row), len(columns))
	}

	headerSize := len(columns) * rowEntrySize
	if headerSize > maxCodecRowLen {
		return nil, fmt.Errorf("row too large: %d columns need a %d-byte offset table, limit is %d", len(columns), headerSize, maxCodecRowLen)
	}

	// The offset table is zero-filled up front and filled in as each payload
	// is appended, so every column's offset is the running length at that
	// moment.
	out := make([]byte, headerSize)
	for i, col := range columns {
		kind, err := kindOf(col.DataType)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", col.Name, err)
		}

		entry := i * rowEntrySize
		if row[i] == nil {
			binary.LittleEndian.PutUint16(out[entry:], nullOffset)
			continue
		}

		start := len(out)
		switch kind {
		case kindString:
			s, ok := row[i].(string)
			if !ok {
				return nil, fmt.Errorf("column %q: expected string, got %T", col.Name, row[i])
			}
			out = append(out, s...)
		case kindInt:
			n, ok := row[i].(int64)
			if !ok {
				return nil, fmt.Errorf("column %q: expected int64, got %T", col.Name, row[i])
			}
			out = binary.LittleEndian.AppendUint64(out, uint64(n))
		case kindBool:
			b, ok := row[i].(bool)
			if !ok {
				return nil, fmt.Errorf("column %q: expected bool, got %T", col.Name, row[i])
			}
			if b {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		}
		if len(out) > maxCodecRowLen {
			return nil, fmt.Errorf("row too large: %d bytes exceeds the limit of %d", len(out), maxCodecRowLen)
		}

		binary.LittleEndian.PutUint16(out[entry:], uint16(start))
		binary.LittleEndian.PutUint16(out[entry+2:], uint16(len(out)-start))
	}

	return out, nil
}

// DecodeRow is EncodeRow's inverse. The bytes come from disk, so every offset
// and length is validated against the buffer: corrupt input is an error,
// never a panic or an out-of-range read. Strings are copied, so the result
// stays valid after b is reused.
func DecodeRow(columns []parser.ColumnDefinition, b []byte) ([]any, error) {
	headerSize := len(columns) * rowEntrySize
	if len(b) < headerSize {
		return nil, fmt.Errorf("row is %d bytes, shorter than its %d-byte offset table", len(b), headerSize)
	}

	row := make([]any, len(columns))
	for i, col := range columns {
		kind, err := kindOf(col.DataType)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", col.Name, err)
		}

		entry := i * rowEntrySize
		offset := int(binary.LittleEndian.Uint16(b[entry:]))
		length := int(binary.LittleEndian.Uint16(b[entry+2:]))

		if offset == nullOffset {
			if length != 0 {
				return nil, fmt.Errorf("column %q: NULL entry has length %d", col.Name, length)
			}
			continue // row[i] stays nil
		}
		if offset < headerSize || offset+length > len(b) {
			return nil, fmt.Errorf("column %q: bytes [%d, %d) outside row payload [%d, %d)", col.Name, offset, offset+length, headerSize, len(b))
		}

		payload := b[offset : offset+length]
		switch kind {
		case kindString:
			row[i] = string(payload)
		case kindInt:
			if length != 8 {
				return nil, fmt.Errorf("column %q: expected 8 bytes for an integer, got %d", col.Name, length)
			}
			row[i] = int64(binary.LittleEndian.Uint64(payload))
		case kindBool:
			if length != 1 {
				return nil, fmt.Errorf("column %q: expected 1 byte for a boolean, got %d", col.Name, length)
			}
			if payload[0] > 1 {
				return nil, fmt.Errorf("column %q: invalid boolean byte %d", col.Name, payload[0])
			}
			row[i] = payload[0] == 1
		}
	}

	return row, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/engine; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'RowCodec|EncodeRow|DecodeRow' -v`
Expected: all PASS, `gofmt` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/rowcodec.go internal/engine/rowcodec_test.go internal/engine/table_test.go
git commit -m "feat(engine): add row codec with per-column offset table

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: File id allocator and database name validation

**Files:**
- Create: `internal/engine/files.go`
- Test: `internal/engine/files_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type DataDir string`
  - `func newFileAllocator(dataDir DataDir) (*fileAllocator, error)`
  - `func (a *fileAllocator) databaseDir(database string) string` returns `<dataDir>/data/<database>`
  - `func (a *fileAllocator) newTablePath(database string) string` returns `<dataDir>/data/<database>/<id>.tbl` with a fresh id each call
  - `func validateDatabaseName(name string) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/files_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFileAllocator(t *testing.T) {
	t.Run("a data directory with nothing in it starts at id 1", func(t *testing.T) {
		dir := t.TempDir()
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		assert.Equal(t, filepath.Join(dir, "data", "shop", "1.tbl"), a.newTablePath("shop"))
	})

	t.Run("ids are shared across databases and never repeat", func(t *testing.T) {
		dir := t.TempDir()
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		first := a.newTablePath("a")
		second := a.newTablePath("b")
		third := a.newTablePath("a")

		assert.Equal(t, filepath.Join(dir, "data", "a", "1.tbl"), first)
		assert.Equal(t, filepath.Join(dir, "data", "b", "2.tbl"), second)
		assert.Equal(t, filepath.Join(dir, "data", "a", "3.tbl"), third)
	})

	t.Run("continues after the highest id already on disk", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "data", "a"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "data", "b"), 0o755))
		for _, name := range []string{
			"a/5.tbl", "b/9.tbl", "a/7.tbl",
			"a/notes.txt",  // not a table file
			"a/x.tbl",      // not a numeric id
			"a/12.tbl.tmp", // a temp file is not a table file
			"readme",       // a plain file directly under data/ is not a database
		} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "data", name), nil, 0o644))
		}

		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		assert.Equal(t, filepath.Join(dir, "data", "a", "10.tbl"), a.newTablePath("a"))
	})

	t.Run("an empty data directory path is rejected", func(t *testing.T) {
		_, err := newFileAllocator("")

		assert.EqualError(t, err, "data directory is required")
	})

	t.Run("a data directory that cannot be scanned is an error", func(t *testing.T) {
		dir := t.TempDir()
		// "data" is a regular file, so it cannot be read as a directory.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), nil, 0o644))

		_, err := newFileAllocator(DataDir(dir))

		assert.Error(t, err)
	})

	t.Run("concurrent callers get distinct paths", func(t *testing.T) {
		a, err := newFileAllocator(DataDir(t.TempDir()))
		require.NoError(t, err)

		const n = 100
		paths := make(chan string, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				paths <- a.newTablePath("shop")
			}()
		}
		wg.Wait()
		close(paths)

		seen := make(map[string]bool, n)
		for p := range paths {
			seen[p] = true
		}
		assert.Len(t, seen, n)
	})
}

func TestValidateDatabaseName(t *testing.T) {
	for _, name := range []string{"shop", "Shop_2", "日本"} {
		assert.NoError(t, validateDatabaseName(name), name)
	}

	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "../x", "a\x00b"} {
		assert.ErrorContains(t, validateDatabaseName(name), "invalid database name", "%q", name)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine -run 'FileAllocator|ValidateDatabaseName' -v`
Expected: build failure, `undefined: newFileAllocator` (and `DataDir`, `validateDatabaseName`).

- [ ] **Step 3: Write the implementation**

Create `internal/engine/files.go`:

```go
package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// DataDir is the root directory all table files live under:
// <DataDir>/data/<database>/<file_id>.tbl.
type DataDir string

const tableFileExt = ".tbl"

// fileAllocator hands out table file paths. A table file is named by a
// numeric id, never by the table's name, so renaming a table never touches
// disk. Ids are never reused, and are shared across every database.
type fileAllocator struct {
	dataDir DataDir
	nextID  atomic.Int64
}

// newFileAllocator continues numbering after the highest id already on disk,
// so a server restarted on a populated data directory can never pick an id
// whose file already exists.
func newFileAllocator(dataDir DataDir) (*fileAllocator, error) {
	if dataDir == "" {
		return nil, errors.New("data directory is required")
	}

	maxID, err := maxTableFileID(filepath.Join(string(dataDir), "data"))
	if err != nil {
		return nil, err
	}

	a := &fileAllocator{dataDir: dataDir}
	a.nextID.Store(maxID + 1)
	return a, nil
}

// maxTableFileID returns the highest <id>.tbl id found in any database
// directory under root, or 0 if there are none. Anything that does not look
// like a table file is ignored.
func maxTableFileID(root string) (int64, error) {
	databases, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("scanning data directory %q: %w", root, err)
	}

	var maxID int64
	for _, database := range databases {
		if !database.IsDir() {
			continue
		}
		dir := filepath.Join(root, database.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return 0, fmt.Errorf("scanning database directory %q: %w", dir, err)
		}
		for _, file := range files {
			name, isTableFile := strings.CutSuffix(file.Name(), tableFileExt)
			if !isTableFile {
				continue
			}
			if id, err := strconv.ParseInt(name, 10, 64); err == nil && id > maxID {
				maxID = id
			}
		}
	}
	return maxID, nil
}

func (a *fileAllocator) databaseDir(database string) string {
	return filepath.Join(string(a.dataDir), "data", database)
}

// newTablePath returns a path for a new table file in database's directory,
// with an id no earlier call has returned.
func (a *fileAllocator) newTablePath(database string) string {
	id := a.nextID.Add(1) - 1
	return filepath.Join(a.databaseDir(database), strconv.FormatInt(id, 10)+tableFileExt)
}

// validateDatabaseName rejects names that are not a single, plain directory
// name. The SQL lexer already limits identifiers to letters, digits and
// underscores, but a database name becomes a directory name, so the engine
// does not rely on its caller for that.
func validateDatabaseName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("invalid database name %q", name)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/engine; go vet ./internal/engine && go test -race -count=1 ./internal/engine -run 'FileAllocator|ValidateDatabaseName' -v`
Expected: all PASS. (The package compiles because nothing else changed yet.)

- [ ] **Step 5: Commit**

```bash
git add internal/engine/files.go internal/engine/files_test.go
git commit -m "feat(engine): add table file id allocator and database name validation

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `storage.data_dir` config setting

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config.yml`
- Test: `internal/config/config_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type StorageConfig struct { DataDir string }` (yaml key `data_dir`)
  - `Config.Storage StorageConfig` (yaml key `storage`)
  - `const DefaultDataDir = "data"`, applied by `Load` when the setting is missing or blank

- [ ] **Step 1: Write the failing tests**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
	return path
}

func TestLoad(t *testing.T) {
	t.Run("reads the storage data directory", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "storage:\n  data_dir: /var/lib/go-sql\n"))

		require.NoError(t, err)
		assert.Equal(t, "/var/lib/go-sql", cfg.Storage.DataDir)
	})

	t.Run("defaults the data directory when the storage section is missing", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "server:\n  port: 1\n"))

		require.NoError(t, err)
		assert.Equal(t, DefaultDataDir, cfg.Storage.DataDir)
	})

	t.Run("defaults the data directory when it is blank", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "storage:\n  data_dir: \"\"\n"))

		require.NoError(t, err)
		assert.Equal(t, DefaultDataDir, cfg.Storage.DataDir)
	})

	t.Run("keeps the server settings", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "server:\n  host: 127.0.0.1\n  port: 50051\n"))

		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:50051", cfg.Server.Addr())
	})

	t.Run("a missing file is an error", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "nope.yml"))

		assert.ErrorContains(t, err, "read config file")
	})

	t.Run("invalid yaml is an error", func(t *testing.T) {
		_, err := Load(writeConfig(t, "server: [unclosed"))

		assert.ErrorContains(t, err, "parse config file")
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config -v`
Expected: build failure, `cfg.Storage undefined` and `undefined: DefaultDataDir`.

- [ ] **Step 3: Write the implementation**

In `internal/config/config.go`, replace the `Config` struct and the end of `Load`:

```go
type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
}

// DefaultDataDir is used when the config does not set storage.data_dir. It is
// relative to the server's working directory.
const DefaultDataDir = "data"
```

and, in `Load`, just before `return &cfg, nil`:

```go
	if cfg.Storage.DataDir == "" {
		cfg.Storage.DataDir = DefaultDataDir
	}

	return &cfg, nil
```

Replace the contents of `internal/config/config.yml` with:

```yaml
server:
  port: 50051
  host: 127.0.0.1

storage:
  data_dir: ./data
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `gofmt -l internal/config; go vet ./internal/config && go test -race -count=1 ./internal/config -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config.yml internal/config/config_test.go
git commit -m "feat(config): add storage data_dir setting

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Give each table a heap file and thread the data directory through

This is one task because the pieces are compile-coupled: changing `NewCatalog`'s signature breaks `wire_gen.go` and every test constructor at once, and the repo must build again at the end of the task.

**Files:**
- Modify: `internal/engine/table.go`, `internal/engine/database.go`, `internal/engine/catalog.go`
- Create: `internal/engine/testhelpers_test.go`, `internal/engine/lifecycle_test.go`
- Modify: `internal/engine/{engine,catalog,database,table}_test.go`, `internal/grpcserver/server_test.go`
- Create: `internal/wiring/providers.go`, `internal/wiring/wiring_test.go`
- Modify: `internal/wiring/wire.go`, `internal/wiring/wire_gen.go`
- Modify: `.gitignore`, `CLAUDE.md`

**Interfaces:**
- Consumes (Task 2): `DataDir`, `newFileAllocator`, `(*fileAllocator).databaseDir`, `(*fileAllocator).newTablePath`, `validateDatabaseName`. (Task 3): `config.Config.Storage.DataDir`. From `internal/storage`: `storage.CreateFile(path string) (storage.File, error)`.
- Produces:
  - `func NewCatalog(dataDir DataDir) (Catalog, error)`
  - `func NewDatabase(name string, files *fileAllocator) (Database, error)`
  - `func NewTable(name string, columns []parser.ColumnDefinition, path string) (*SqlTable, error)`
  - `Close() error` on `Catalog`, `Database`, `Table`; `Drop() error` on `Database`, `Table`
  - `wiring.ProvideDataDir(cfg *config.Config) engine.DataDir`
  - test helpers: `newTestCatalog(t)`, `newTestCatalogAt(t, dir)`, `newTestDatabase(t)`, `newTempTable(t, name, columns)`, `tableFiles(t, dir, database)`

- [ ] **Step 1: Write the test helpers**

Create `internal/engine/testhelpers_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// newTestCatalogAt builds a catalog over dir and closes it when the test
// ends, so open file handles never pile up across the suite.
func newTestCatalogAt(t *testing.T, dir string) Catalog {
	t.Helper()
	catalog, err := NewCatalog(DataDir(dir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	return catalog
}

func newTestCatalog(t *testing.T) Catalog {
	t.Helper()
	return newTestCatalogAt(t, t.TempDir())
}

func newTestDatabase(t *testing.T) Database {
	t.Helper()
	files, err := newFileAllocator(DataDir(t.TempDir()))
	require.NoError(t, err)
	db, err := NewDatabase("testdb", files)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTempTable is NewTable over a temp-dir file, with the same (table, err)
// result so a call site only changes the function name. The table's file is
// closed when the test ends.
func newTempTable(t *testing.T, name string, columns []parser.ColumnDefinition) (*SqlTable, error) {
	t.Helper()
	table, err := NewTable(name, columns, filepath.Join(t.TempDir(), "1.tbl"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = table.Close() })
	return table, nil
}

// tableFiles lists the file names in a database's directory, sorted.
func tableFiles(t *testing.T, dir, database string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "data", database))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}
```

- [ ] **Step 2: Write the lifecycle tests**

Create `internal/engine/lifecycle_test.go`:

```go
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

var idColumn = []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}

func createDB(t *testing.T, c Catalog, name string) Database {
	t.Helper()
	require.NoError(t, c.CreateDatabase(name))
	db, err := c.GetDatabase(name)
	require.NoError(t, err)
	return db
}

func TestLifecycle_CreateDatabaseMakesItsDirectory(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)

	createDB(t, c, "shop")

	info, err := os.Stat(filepath.Join(dir, "data", "shop"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestLifecycle_CreateDatabaseFailureLeavesNoPhantomDatabase(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	// "data" is a regular file, so no database directory can be made under it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), nil, 0o644))

	err := c.CreateDatabase("shop")

	require.Error(t, err)
	assert.Empty(t, c.ListDatabases())
}

func TestLifecycle_CreateDatabaseRejectsPathLikeNames(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)

	for _, name := range []string{"../escape", "a/b", ".."} {
		assert.ErrorContains(t, c.CreateDatabase(name), "invalid database name", name)
	}

	assert.Empty(t, c.ListDatabases())
	_, err := os.Stat(filepath.Join(dir, "escape"))
	assert.True(t, os.IsNotExist(err), "nothing is created outside <data_dir>/data")
}

func TestLifecycle_CreateTableMakesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")

	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
	info, err := os.Stat(filepath.Join(dir, "data", "shop", "1.tbl"))
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestLifecycle_CreateTableWithInvalidSchemaLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")

	err := db.CreateTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "id", DataType: parser.IntDataType{}},
	}, false)

	require.Error(t, err)
	assert.Empty(t, tableFiles(t, dir, "shop"))
	_, exists := db.GetTable("users")
	assert.False(t, exists)
}

func TestLifecycle_CreateTableThatAlreadyExistsMakesNoSecondFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Error(t, db.CreateTable("users", idColumn, false))
	assert.NoError(t, db.CreateTable("users", idColumn, true), "IF NOT EXISTS")

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_DropTableRemovesOnlyItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))  // 1.tbl
	require.NoError(t, db.CreateTable("orders", idColumn, false)) // 2.tbl

	require.NoError(t, db.DropTable("users", false))

	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"))
	assert.NoError(t, db.DropTable("users", true), "IF EXISTS on a missing table is a no-op")
	assert.EqualError(t, db.DropTable("users", false), `table "users" does not exist`)
	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_RecreatingADroppedTableGetsANewFileID(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false)) // 1.tbl
	require.NoError(t, db.DropTable("users", false))

	require.NoError(t, db.CreateTable("users", idColumn, false))

	assert.Equal(t, []string{"2.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_RenameTableKeepsItsFile(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))

	require.NoError(t, db.RenameTable("users", "customers"))

	assert.Equal(t, []string{"1.tbl"}, tableFiles(t, dir, "shop"))
	_, exists := db.GetTable("customers")
	assert.True(t, exists)
}

func TestLifecycle_DropDatabaseRemovesItsFilesAndDirectory(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))
	require.NoError(t, db.CreateTable("orders", idColumn, false))

	require.NoError(t, c.DropDatabase("shop"))

	_, err := os.Stat(filepath.Join(dir, "data", "shop"))
	assert.True(t, os.IsNotExist(err))
	assert.Empty(t, c.ListDatabases())
}

// Two database names that differ only by case (or Unicode form) share one
// directory on a case-insensitive filesystem, so dropping one must not delete
// anything it did not create.
func TestLifecycle_DropDatabaseLeavesForeignFilesAlone(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))
	foreign := filepath.Join(dir, "data", "shop", "foreign.dat")
	require.NoError(t, os.WriteFile(foreign, []byte("not ours"), 0o644))

	require.NoError(t, c.DropDatabase("shop"))

	assert.Equal(t, []string{"foreign.dat"}, tableFiles(t, dir, "shop"), "its own table file is gone, the foreign file and directory remain")
	got, err := os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, []byte("not ours"), got)
}

func TestLifecycle_RestartContinuesFileIDs(t *testing.T) {
	dir := t.TempDir()
	first := newTestCatalogAt(t, dir)
	db := createDB(t, first, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))  // 1.tbl
	require.NoError(t, db.CreateTable("orders", idColumn, false)) // 2.tbl
	require.NoError(t, first.Close())

	// The schemas are not persisted yet, so the new catalog knows nothing of
	// "shop"; the files are still on disk and must not be overwritten.
	second := newTestCatalogAt(t, dir)
	db2 := createDB(t, second, "shop")
	require.NoError(t, db2.CreateTable("invoices", idColumn, false))

	assert.Equal(t, []string{"1.tbl", "2.tbl", "3.tbl"}, tableFiles(t, dir, "shop"))
}

func TestLifecycle_ConcurrentCreateTableGetsDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	db := createDB(t, newTestCatalogAt(t, dir), "shop")

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assert.NoError(t, db.CreateTable(fmt.Sprintf("t%d", i), idColumn, false))
		}(i)
	}
	wg.Wait()

	assert.Len(t, tableFiles(t, dir, "shop"), n)
}

func TestLifecycle_CloseIsIdempotentAndReleasesTheFile(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	db := createDB(t, c, "shop")
	require.NoError(t, db.CreateTable("users", idColumn, false))
	table, _ := db.GetTable("users")

	require.NoError(t, c.Close())
	require.NoError(t, c.Close())

	assert.Nil(t, table.(*SqlTable).file)
}

func TestTable_DropRemovesItsFile(t *testing.T) {
	table, err := newTempTable(t, "users", idColumn)
	require.NoError(t, err)

	require.NoError(t, table.Drop())
	require.NoError(t, table.Drop(), "dropping twice is harmless")

	_, statErr := os.Stat(table.path)
	assert.True(t, os.IsNotExist(statErr))
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go vet ./internal/engine`
Expected: compile errors, `NewCatalog` / `NewDatabase` / `NewTable` called with the wrong arguments, and `table.path`, `Close`, `Drop` undefined. This is the red state; the old test files still use the old signatures and are fixed in Step 5.

- [ ] **Step 4: Implement the engine changes**

**`internal/engine/table.go`.** Add imports `errors`, `io/fs`, `os`, and `manhhung2111/go-sql/internal/storage`. Extend the `Table` interface and struct, and replace `NewTable`:

```go
type Table interface {
	InsertValues(columns []string, values [][]parser.Token) error
	Select(columns []string, where parser.Expression) (Response, error)
	AlterColumns(action parser.AlterAction) error
	Delete(where parser.Expression) error
	Update(assignments []parser.Assignment, where parser.Expression) error
	Rename(name string)
	// Close releases the table's file. It is idempotent.
	Close() error
	// Drop closes the table's file and deletes it. It is idempotent.
	Drop() error
}

type SqlTable struct {
	mu      sync.RWMutex
	Name    string
	Columns []parser.ColumnDefinition
	Rows    [][]any

	// path and file are the table's heap file. Statements still run against
	// Rows; the file exists now so that creating, dropping and numbering
	// files is settled before the data moves onto it.
	path string
	file storage.File
}
```

```go
// Rules when for a table schema:
//   - Not yet supported the multi-column primary key
//   - There is only 1 primary key column in a table, and the table can have multiple columns with constrants (NOT NULL & UNIQUE)
//   - Columns do not specify with contraint NOT NULL will default be NULLABLE
//   - A table can not have duplicated columns
//
// The schema is validated before the file at path is created, so a rejected
// schema never leaves a file behind. Creating the file fails if path already
// exists.
func NewTable(name string, columns []parser.ColumnDefinition, path string) (*SqlTable, error) {
	validated := make([]parser.ColumnDefinition, 0, len(columns))
	for _, column := range columns {
		if err := validateNewColumn(name, validated, column); err != nil {
			return nil, err
		}
		validated = append(validated, column)
	}

	file, err := storage.CreateFile(path)
	if err != nil {
		return nil, err
	}

	return &SqlTable{Name: name, Columns: columns, Rows: make([][]any, 0), path: path, file: file}, nil
}
```

Append at the end of the file:

```go
func (t *SqlTable) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeLocked()
}

func (t *SqlTable) closeLocked() error {
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

func (t *SqlTable) Drop() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	closeErr := t.closeLocked()
	if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("removing table file %q: %w", t.path, err))
	}
	return closeErr
}
```

**`internal/engine/database.go`.** Add imports `errors`, `io/fs`, `os`, `syscall`. Replace the interface, struct, `NewDatabase`, and `DropTable`, and add `Close` and `Drop`:

```go
type Database interface {
	GetTable(name string) (Table, bool)
	CreateTable(name string, columns []parser.ColumnDefinition, ifNotExists bool) error
	RenameTable(oldName, newName string) error
	DropTable(name string, ifExists bool) error
	// Close closes every table's file.
	Close() error
	// Drop drops every table, then removes the database's directory if
	// nothing else is left in it.
	Drop() error
}

type SqlDatabase struct {
	mu    sync.RWMutex
	Name  string
	Table map[string]Table
	files *fileAllocator
}

// NewDatabase validates name (it becomes a directory name) and creates the
// database's directory.
func NewDatabase(name string, files *fileAllocator) (Database, error) {
	if err := validateDatabaseName(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(files.databaseDir(name), 0o755); err != nil {
		return nil, fmt.Errorf("creating directory for database %q: %w", name, err)
	}

	return &SqlDatabase{
		Name:  name,
		Table: make(map[string]Table),
		files: files,
	}, nil
}
```

In `CreateTable`, change the `NewTable` call:

```go
	table, err := NewTable(name, columns, d.files.newTablePath(d.Name))
```

Replace `DropTable`:

```go
// DropTable removes the table from the database first, then deletes its file:
// if the file cannot be removed the table is still gone and the file is a
// harmless orphan, never a table pointing at a missing file.
func (d *SqlDatabase) DropTable(name string, ifExists bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	table, exists := d.Table[name]
	if !exists {
		if ifExists {
			return nil
		}
		return fmt.Errorf("table %q does not exist", name)
	}

	delete(d.Table, name)
	if err := table.Drop(); err != nil {
		return fmt.Errorf("table %q dropped, but removing its file failed: %w", name, err)
	}
	return nil
}

func (d *SqlDatabase) Close() error {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var errs []error
	for name, table := range d.Table {
		if err := table.Close(); err != nil {
			errs = append(errs, fmt.Errorf("table %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (d *SqlDatabase) Drop() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var errs []error
	for name, table := range d.Table {
		if err := table.Drop(); err != nil {
			errs = append(errs, fmt.Errorf("table %q: %w", name, err))
		}
	}
	d.Table = make(map[string]Table)

	// A non-recursive remove: it refuses a directory that still has files in
	// it. Database names that differ only by case or Unicode form can share a
	// directory on some filesystems, and this must never delete a file it did
	// not create.
	if err := os.Remove(d.files.databaseDir(d.Name)); err != nil &&
		!errors.Is(err, fs.ErrNotExist) &&
		!errors.Is(err, syscall.ENOTEMPTY) &&
		!errors.Is(err, syscall.EEXIST) {
		errs = append(errs, fmt.Errorf("removing directory of database %q: %w", d.Name, err))
	}
	return errors.Join(errs...)
}
```

**`internal/engine/catalog.go`.** Add import `errors`. Replace the interface, struct, constructor, `CreateDatabase`, `DropDatabase`, and add `Close`:

```go
type Catalog interface {
	GetDatabase(name string) (Database, error)
	ListDatabases() []string
	CreateDatabase(name string) error
	DropDatabase(name string) error
	// Close closes every database's table files.
	Close() error
}

type SqlCatalog struct {
	mu        sync.RWMutex
	Databases map[string]Database
	files     *fileAllocator
}

func NewCatalog(dataDir DataDir) (Catalog, error) {
	files, err := newFileAllocator(dataDir)
	if err != nil {
		return nil, err
	}
	return &SqlCatalog{
		Databases: make(map[string]Database),
		files:     files,
	}, nil
}
```

```go
func (c *SqlCatalog) CreateDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.Databases[name]; exists {
		return fmt.Errorf("database %q already exists", name)
	}

	db, err := NewDatabase(name, c.files)
	if err != nil {
		return err
	}

	c.Databases[name] = db
	return nil
}

// DropDatabase removes the database from the catalog first, then deletes its
// files: a failure removing them leaves orphan files, never a database that
// points at missing ones.
func (c *SqlCatalog) DropDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	db, exists := c.Databases[name]
	if !exists {
		return fmt.Errorf("database %q does not exist", name)
	}

	delete(c.Databases, name)
	if err := db.Drop(); err != nil {
		return fmt.Errorf("database %q dropped, but removing its files failed: %w", name, err)
	}
	return nil
}

func (c *SqlCatalog) Close() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var errs []error
	for name, db := range c.Databases {
		if err := db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("database %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
```

Lock order is `Catalog.mu` then `Database.mu` then `Table.mu`; nothing in this task takes them in another order.

- [ ] **Step 5: Update the existing tests mechanically**

```bash
sed -i '' 's/NewCatalog()/newTestCatalog(t)/g' internal/engine/engine_test.go internal/engine/catalog_test.go
sed -i '' 's/NewDatabase("testdb")/newTestDatabase(t)/g' internal/engine/database_test.go
sed -i '' 's/NewTable("users", /newTempTable(t, "users", /g' internal/engine/table_test.go
```

Then run `go vet ./internal/engine`. If a call site sits in a helper that has no `t`, thread `t *testing.T` through that helper. In `internal/grpcserver/server_test.go`, replace

```go
	sqlEngine := engine.NewEngine(engine.NewCatalog())
```

with

```go
	catalog, err := engine.NewCatalog(engine.DataDir(t.TempDir()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	sqlEngine := engine.NewEngine(catalog)
```

(if the function already declares `err` further down with `:=`, change that later line to `=` or rename; the compiler will say which).

- [ ] **Step 6: Run the engine tests**

Run: `gofmt -l internal; go vet ./internal/engine && go test -race -count=1 ./internal/engine ./internal/grpcserver -v 2>&1 | tail -40`
Expected: all PASS, including every existing engine test, unchanged except for constructors. `go build ./...` still fails at `internal/wiring/wire_gen.go` until Step 7.

- [ ] **Step 7: Wire the data directory in**

Create `internal/wiring/providers.go` (no build tag, so both the injector and the generated file can see it):

```go
package wiring

import (
	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
)

// ProvideDataDir hands the configured data directory to the engine.
func ProvideDataDir(cfg *config.Config) engine.DataDir {
	return engine.DataDir(cfg.Storage.DataDir)
}
```

In `internal/wiring/wire.go`, add `ProvideDataDir` to the set:

```go
var WireSet = wire.NewSet(
	parser.WireSet,
	grpcserver.WireSet,
	engine.WireSet,
	ProvideDataDir,
)
```

Regenerate:

```bash
PATH="$(go env GOPATH)/bin:$PATH" make generate
```

Expected `internal/wiring/wire_gen.go` `InitializeServer` (if `wire` is unavailable, write it by hand to exactly this, and make the `WireSet` line at the bottom end with `ProvideDataDir)`):

```go
func InitializeServer(cfg *config.Config) (*grpcserver.Server, error) {
	lexer := parser.NewLexer()
	parserParser := parser.NewParser(lexer)
	dataDir := ProvideDataDir(cfg)
	catalog, err := engine.NewCatalog(dataDir)
	if err != nil {
		return nil, err
	}
	engineEngine := engine.NewEngine(catalog)
	server := grpcserver.NewServer(cfg, parserParser, engineEngine)
	return server, nil
}
```

Create `internal/wiring/wiring_test.go`:

```go
package wiring

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/config"
)

func TestInitializeServer(t *testing.T) {
	t.Run("builds a server over the configured data directory", func(t *testing.T) {
		cfg := &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}}

		server, err := InitializeServer(cfg)

		require.NoError(t, err)
		assert.NotNil(t, server)
	})

	t.Run("fails when no data directory is configured", func(t *testing.T) {
		_, err := InitializeServer(&config.Config{})

		assert.ErrorContains(t, err, "data directory is required")
	})
}
```

- [ ] **Step 8: Housekeeping docs**

Append to `.gitignore` (next to `/plan`):

```
/data
```

In `CLAUDE.md`, insert this paragraph immediately before `## Conventions`:

```markdown
**Storage** (`internal/storage`, standalone): slotted-page heap files of 16KiB checksummed pages, with tombstone deletes and a checksum-verifying `Scan`. It deals only in `[]byte` rows and never imports `internal/parser`. The engine owns one file per `SqlTable` at `<storage.data_dir>/data/<db>/<file_id>.tbl` (default `data`, gitignored). Files are named by numeric id, never by table name, so renaming a table never touches disk; ids come from `fileAllocator` and are never reused. `Catalog`/`Database`/`Table` all have `Close()`; `Database`/`Table` have `Drop()`. For now the files are created and removed with their tables but stay empty: statements still run against in-memory `Rows`.
```

- [ ] **Step 9: Whole-repo verification**

Run: `gofmt -l . ; go build ./... && go vet ./... && go test -race -count=1 ./...`
Expected: gofmt prints nothing; every package passes.

Manual check that the file lifecycle works end to end through the real server wiring:

```bash
go test -race -count=1 ./internal/wiring ./internal/engine -run 'InitializeServer|Lifecycle' -v
```
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal .gitignore CLAUDE.md
git status --short   # only the files listed in this task; plan/ must not appear
git commit -m "feat(engine): give each table a heap file under the data directory

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

## Pull request

When the user asks for the PR: title `feat(engine): add row codec and per-table heap files`. The body needs **Summary** and **Test plan** sections with every paragraph and bullet on a single unwrapped line (GitHub renders manual line breaks), ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. The Summary should say that no SQL-visible behaviour changes and that the files stay empty until PR B; the Test plan should list the codec boundary cases, the lifecycle tests (including the five review-focus ones), and `go test -race ./...`.

## Self-review (done while writing)

- **Spec coverage:** 5.1 codec is Task 1; 5.2 file naming, allocator, `Close`/`Drop`, `NewCatalog(DataDir)`, `data_dir`, directory ownership, commit-then-remove drop order and the case-insensitive-filesystem caveat are Tasks 2 to 4. `ALTER` rewrite, the statement flows and removing `Rows` are 5.3 to 5.5 and belong to PR B. Directory fsync is deliberately left for PR C (spec section 7).
- **Placeholders:** none; every code step has full code.
- **Type consistency:** `DataDir`, `newFileAllocator`, `databaseDir`, `newTablePath`, `validateDatabaseName` (Task 2) are used with the same signatures in Task 4; `NewTable(name, columns, path)`, `NewDatabase(name, files)` and `NewCatalog(DataDir)` match the helpers and the generated wiring; `maxCodecRowLen`, `rowEntrySize`, `nullOffset` (Task 1) are the names the tests use.
- **Known limitation to carry into PR B/C:** the server never calls `Catalog.Close()` on shutdown (`InitializeServer` returns only the gRPC server). Harmless in PR A because the files are empty; PR B writes through and fsyncs per statement so nothing is lost either, but a clean shutdown path should be added when the catalog gains startup/shutdown work in PR C.
