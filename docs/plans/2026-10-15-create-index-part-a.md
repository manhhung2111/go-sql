# CREATE INDEX part A (syntax, key encoding, durable definitions) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `CREATE INDEX name ON table (column)` and `DROP INDEX name ON table` parse, are stored durably in a new `sys_indexes` system file, survive restarts, renames and drops, and the secondary key encoding exists. The index does nothing yet (part B builds and uses the trees).

**Architecture:** Parser gets two keywords and two statements. `internal/index` gets a prefix-free, order-preserving secondary key encoding. The catalog store gets a third heap file whose rows are validated against the surviving tables at load, so renames and drops can be ordered (insert new rows, replace the table row, tombstone old rows) for crash safety. `SqlTable` holds the definitions and gains `CreateIndex`/`DropIndex`.

**Tech Stack:** Go 1.26, testify, `internal/parser`, `internal/index`, `internal/engine`.

**Spec:** `docs/specs/2026-10-15-create-index-design.md` (the "Syntax", "Engine statements", "Durability" and "Secondary key encoding" sections). Part B (trees, maintenance, planner) is a separate plan written after this merges.

## Global Constraints

- `INDEX` and `ON` become reserved words. Stored schemas (JSON) are unaffected.
- One column only; a column list of two or more, `UNIQUE INDEX`, and trailing tokens are parse errors. Peek and check a token's type before advancing past it.
- `sys_indexes` row: `(db, tbl, name, col)`, all TEXT; the row is the commit point of `CREATE INDEX`, its tombstone of `DROP INDEX`; every write is fsynced (`insertSynced`/`deleteSynced`).
- At load an index row is kept only if its table survives and that table's schema has the column; every other row is tombstoned; a repeated `(db, tbl, name)` among the valid rows keeps the first.
- Ordering for crash safety: `RENAME TO` and `RENAME COLUMN` insert the new index rows first, then replace the table row, then tombstone the old index rows. `DROP TABLE` tombstones the table row, then its index rows. A new table or database sweeps leftover index rows under its name.
- Lock order is unchanged: `Catalog.mu` → `Database.mu` → `Table.mu` → `tableIndexes.mu` → `catalogStore.mu`.
- Error messages: lowercase, no trailing punctuation, `%q` for identifiers, "expected X, got Y".
- Engine package non-test code must not import stdlib `cmp`. Commits: Conventional Commits ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Run `gofmt -w` on touched files; `go test -race ./...` before the last commit of each task.

## Review Focus

- A crash between any two steps of `RENAME TO`, `RENAME COLUMN` and `DROP TABLE` must load with the right definitions and no dangling rows (tested by writing each intermediate state by hand).
- A dropped table's leftover index rows must not attach to a new table of the same name.
- `index` and `on` as identifiers must now fail to parse, not silently misparse.
- `CREATE INDEX` on a closed (dropped) table must fail without touching the catalog.
- Strings containing zero bytes, one string a prefix of another, and negative integers must sort correctly in the secondary key encoding.

## File Structure

- `internal/parser/token.go`, `parser.go`, `parser_ddl.go`, `ast_ddl.go`: tokens, dispatch, parsing, AST.
- `internal/parser/index_test.go` (new): parser tests.
- `internal/index/secondarykey.go` (new), `secondarykey_test.go` (new): encoding.
- `internal/engine/catalogstore.go`: `sys_indexes` file, row codec, `addIndex`, `removeIndex`, staging for renames, sweeps; `catalogload.go`: load-time validation.
- `internal/engine/indexdef.go` (new): `indexDef` and the table's `CreateIndex`/`DropIndex`.
- `internal/engine/table.go`, `database.go`, `catalog.go`, `engine.go`: wiring.
- `internal/engine/indexcatalog_test.go` (new): store, load, crash-state and statement tests.

---

### Task 1: Parser

**Files:**
- Modify: `internal/parser/token.go` (after `TO` in the const list; `tokenTypeMapping`), `internal/parser/parser.go` (`CREATE` and `DROP` cases), `internal/parser/parser_ddl.go`, `internal/parser/ast_ddl.go`
- Test: `internal/parser/index_test.go` (create)

**Interfaces:**
- Produces: `parser.CreateIndexStatement{Name, Table, Column string; IfNotExists bool}`, `parser.DropIndexStatement{Name, Table string; IfExists bool}`.

- [ ] **Step 1: Write the failing tests**

Create `internal/parser/index_test.go`:

```go
package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse_CreateIndex(t *testing.T) {
	tests := []struct {
		sql  string
		want CreateIndexStatement
	}{
		{"CREATE INDEX idx_name ON users (name)", CreateIndexStatement{Name: "idx_name", Table: "users", Column: "name"}},
		{"create index idx_name on users (name);", CreateIndexStatement{Name: "idx_name", Table: "users", Column: "name"}},
		{"CREATE INDEX IF NOT EXISTS i ON t (c)", CreateIndexStatement{Name: "i", Table: "t", Column: "c", IfNotExists: true}},
	}
	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			stmt, err := parse(t, tc.sql)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, stmt)
		})
	}
}

func TestParse_DropIndex(t *testing.T) {
	tests := []struct {
		sql  string
		want DropIndexStatement
	}{
		{"DROP INDEX idx_name ON users", DropIndexStatement{Name: "idx_name", Table: "users"}},
		{"drop index if exists i on t;", DropIndexStatement{Name: "i", Table: "t", IfExists: true}},
	}
	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			stmt, err := parse(t, tc.sql)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, stmt)
		})
	}
}

func TestParse_IndexErrors(t *testing.T) {
	tests := []struct {
		sql     string
		wantErr string
	}{
		{"CREATE INDEX", "expected index name, got "},
		{"CREATE INDEX 1 ON t (c)", "expected index name, got 1"},
		{"CREATE INDEX i t (c)", "expected ON after index name, got t"},
		{"CREATE INDEX i ON", "expected table name, got "},
		{"CREATE INDEX i ON t c", "expected ( after table name, got c"},
		{"CREATE INDEX i ON t ()", "expected column name, got )"},
		{"CREATE INDEX i ON t (a, b)", "composite indexes are not supported"},
		{"CREATE INDEX i ON t (a", "expected ) after column name, got "},
		{"CREATE INDEX i ON t (a) extra", "unexpected token after statement, got extra"},
		{"CREATE INDEX IF i ON t (a)", "expected NOT after IF, got i"},
		{"CREATE INDEX IF NOT i ON t (a)", "expected EXISTS after IF NOT, got i"},
		{"CREATE UNIQUE INDEX i ON t (a)", "DATABASE, TABLE or INDEX keyword must be expected after CREATE, got UNIQUE"},
		{"DROP INDEX", "expected index name, got "},
		{"DROP INDEX i", "expected ON after index name, got "},
		{"DROP INDEX i ON", "expected table name, got "},
		{"DROP INDEX IF i ON t", "expected EXISTS after IF, got i"},
		{"DROP INDEX i ON t extra", "unexpected token after statement, got extra"},
	}
	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			_, err := parse(t, tc.sql)

			assert.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestParse_IndexAndOnAreReservedWords(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE t (index INT)",
		"CREATE TABLE t (on INT)",
		"SELECT index FROM t",
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := parse(t, sql)

			assert.Error(t, err)
		})
	}
}
```

- [ ] **Step 2: Add the types so the tests compile, and watch them fail**

In `internal/parser/ast_ddl.go` add:

```go
type CreateIndexStatement struct {
	Name        string
	Table       string
	Column      string
	IfNotExists bool
}

type DropIndexStatement struct {
	Name     string
	Table    string
	IfExists bool
}
```

Run: `go test ./internal/parser/ -run 'TestParse_(CreateIndex|DropIndex|IndexErrors|IndexAndOn)' 2>&1 | tail -20`
Expected: FAIL on assertions (`CREATE INDEX` is "DATABASE keyword must be expected after CREATE…", `index` parses as an identifier). It must compile.

- [ ] **Step 3: Implement**

`token.go`: add `INDEX` and `ON` after `TO` in the const block, and `"INDEX": INDEX, "ON": ON,` to `tokenTypeMapping` (same line as `"ALTER": ALTER, ...`).

`parser.go`, replace the `CREATE` and `DROP` cases:

```go
	case CREATE:
		if s.expect(DATABASE) {
			stmt, err = s.parseCreateDatabaseStatement()
		} else if s.expect(TABLE) {
			stmt, err = s.parseCreateTableStatement()
		} else if s.expect(INDEX) {
			stmt, err = s.parseCreateIndexStatement()
		} else {
			return nil, fmt.Errorf("DATABASE, TABLE or INDEX keyword must be expected after CREATE, got %s", s.peek().Value)
		}
	case DROP:
		if s.expect(DATABASE) {
			stmt, err = s.parseDropDatabaseStatement()
		} else if s.expect(TABLE) {
			stmt, err = s.parseDropTableStatement()
		} else if s.expect(INDEX) {
			stmt, err = s.parseDropIndexStatement()
		} else {
			return nil, fmt.Errorf("DATABASE, TABLE or INDEX keyword must be expected after DROP, got %s", s.peek().Value)
		}
```

`parser_ddl.go`, append:

```go
// "CREATE" "INDEX" ("IF" "NOT" "EXISTS")? index_name "ON" table_name "(" column_name ")"
func (s *sqlParser) parseCreateIndexStatement() (CreateIndexStatement, error) {
	stmt := CreateIndexStatement{}

	if s.expect(IF) {
		if !s.expect(NOT) {
			return CreateIndexStatement{}, fmt.Errorf("expected NOT after IF, got %s", s.peek().Value)
		}
		if !s.expect(EXISTS) {
			return CreateIndexStatement{}, fmt.Errorf("expected EXISTS after IF NOT, got %s", s.peek().Value)
		}
		stmt.IfNotExists = true
	}

	name := s.advance()
	if name.Type != IDENT {
		return CreateIndexStatement{}, fmt.Errorf("expected index name, got %s", name.Value)
	}
	stmt.Name = name.Value

	if !s.expect(ON) {
		return CreateIndexStatement{}, fmt.Errorf("expected ON after index name, got %s", s.peek().Value)
	}

	table := s.advance()
	if table.Type != IDENT {
		return CreateIndexStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}
	stmt.Table = table.Value

	if !s.expect(LPAREN) {
		return CreateIndexStatement{}, fmt.Errorf("expected ( after table name, got %s", s.peek().Value)
	}
	column := s.advance()
	if column.Type != IDENT {
		return CreateIndexStatement{}, fmt.Errorf("expected column name, got %s", column.Value)
	}
	stmt.Column = column.Value

	if s.peek().Type == COMMA {
		return CreateIndexStatement{}, fmt.Errorf("composite indexes are not supported")
	}
	if !s.expect(RPAREN) {
		return CreateIndexStatement{}, fmt.Errorf("expected ) after column name, got %s", s.peek().Value)
	}
	return stmt, nil
}

// "DROP" "INDEX" ("IF" "EXISTS")? index_name "ON" table_name
func (s *sqlParser) parseDropIndexStatement() (DropIndexStatement, error) {
	stmt := DropIndexStatement{}

	if s.expect(IF) {
		if !s.expect(EXISTS) {
			return DropIndexStatement{}, fmt.Errorf("expected EXISTS after IF, got %s", s.peek().Value)
		}
		stmt.IfExists = true
	}

	name := s.advance()
	if name.Type != IDENT {
		return DropIndexStatement{}, fmt.Errorf("expected index name, got %s", name.Value)
	}
	stmt.Name = name.Value

	if !s.expect(ON) {
		return DropIndexStatement{}, fmt.Errorf("expected ON after index name, got %s", s.peek().Value)
	}

	table := s.advance()
	if table.Type != IDENT {
		return DropIndexStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}
	stmt.Table = table.Value
	return stmt, nil
}
```

Note: the existing error `DATABASE or TABLE keyword must be expected after DROP` and `DATABASE keyword must be expected after CREATE` change text; update any existing parser test that asserts them (`grep -rn "keyword must be expected after" internal/`).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/parser/ 2>&1 | tail -20`
Expected: PASS. Where the empty-EOF token's `Value` is not `""`, adjust the expected `got ` suffixes in `TestParse_IndexErrors` to what the lexer reports for end of input (look at an existing test such as `TestParse_DropTableNonIdentTable` neighbours for an EOF case) and note it.

- [ ] **Step 5: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean, all ok.

```bash
git add internal/parser
git commit -m "feat(parser): parse CREATE INDEX and DROP INDEX

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Secondary key encoding

**Files:**
- Create: `internal/index/secondarykey.go`
- Test: `internal/index/secondarykey_test.go` (create)

**Interfaces:**
- Produces: `index.EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error)`; `index.SecondaryEqualRange(v any) (lo, hi []byte, err error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/index/secondarykey_test.go`:

```go
package index

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

func secKey(t *testing.T, v any, id storage.RowID) []byte {
	t.Helper()
	key, err := EncodeSecondaryKey(v, id)
	require.NoError(t, err)
	return key
}

func TestEncodeSecondaryKey_OrdersByValueThenRowID(t *testing.T) {
	// Already in the order they must sort in.
	ordered := []struct {
		v  any
		id storage.RowID
	}{
		{int64(-9223372036854775808), storage.RowID{Page: 5, Slot: 5}},
		{int64(-1), storage.RowID{Page: 0, Slot: 0}},
		{int64(0), storage.RowID{Page: 0, Slot: 1}},
		{int64(0), storage.RowID{Page: 0, Slot: 2}},
		{int64(0), storage.RowID{Page: 1, Slot: 0}},
		{int64(0), storage.RowID{Page: 2147483647, Slot: 0}},
		{int64(1), storage.RowID{Page: 0, Slot: 0}},
		{int64(9223372036854775807), storage.RowID{Page: 0, Slot: 0}},
	}
	for i := 1; i < len(ordered); i++ {
		a := secKey(t, ordered[i-1].v, ordered[i-1].id)
		b := secKey(t, ordered[i].v, ordered[i].id)
		assert.Less(t, bytes.Compare(a, b), 0, "entry %d sorts before entry %d", i-1, i)
	}

	strs := []string{"", "\x00", "\x00\x00", "a", "a\x00", "a\x00b", "a\x01", "ab", "b"}
	for i := 1; i < len(strs); i++ {
		a := secKey(t, strs[i-1], storage.RowID{Page: 9, Slot: 9})
		b := secKey(t, strs[i], storage.RowID{Page: 0, Slot: 0})
		assert.Less(t, bytes.Compare(a, b), 0, "%q sorts before %q whatever their row ids", strs[i-1], strs[i])
	}

	f := secKey(t, false, storage.RowID{Page: 9, Slot: 9})
	tr := secKey(t, true, storage.RowID{Page: 0, Slot: 0})
	assert.Less(t, bytes.Compare(f, tr), 0)
}

func TestEncodeSecondaryKey_Errors(t *testing.T) {
	_, err := EncodeSecondaryKey(nil, storage.RowID{})
	assert.Error(t, err)
	_, err = EncodeSecondaryKey(3.5, storage.RowID{})
	assert.Error(t, err)
}

// For a random set of entries, the entries in [lo, hi) are exactly the ones of
// the value, and none of its neighbours.
func TestSecondaryEqualRange_HoldsExactlyTheEntriesOfAValue(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	alphabet := []string{"", "\x00", "a", "a\x00", "ab", "b", "\xff", "\xff\xff"}
	type entry struct {
		v   any
		key []byte
	}
	var entries []entry
	for i := 0; i < 400; i++ {
		var v any
		if i%2 == 0 {
			v = alphabet[rng.Intn(len(alphabet))]
		} else {
			v = int64(rng.Intn(5) - 2)
		}
		id := storage.RowID{Page: int32(rng.Intn(1000)), Slot: rng.Intn(50)}
		entries = append(entries, entry{v, secKey(t, v, id)})
	}

	for _, probe := range []any{"", "a", "ab", "\x00", "\xff", "zz", int64(-2), int64(0), int64(2), int64(7)} {
		lo, hi, err := SecondaryEqualRange(probe)
		require.NoError(t, err)
		for _, e := range entries {
			inRange := bytes.Compare(e.key, lo) >= 0 && bytes.Compare(e.key, hi) < 0
			assert.Equal(t, e.v == probe, inRange, "probe %q entry %q", probe, e.v)
		}
	}
}

func TestSecondaryEntries_SortIntoGroups(t *testing.T) {
	var keys [][]byte
	for _, v := range []string{"b", "a", "ab", "a"} {
		for s := 0; s < 3; s++ {
			keys = append(keys, secKey(t, v, storage.RowID{Page: 0, Slot: s}))
		}
	}
	slices.SortFunc(keys, bytes.Compare)

	lo, hi, err := SecondaryEqualRange("a")
	require.NoError(t, err)
	first := slices.IndexFunc(keys, func(k []byte) bool { return bytes.Compare(k, lo) >= 0 })
	last := slices.IndexFunc(keys, func(k []byte) bool { return bytes.Compare(k, hi) >= 0 })
	assert.Equal(t, 6, last-first, "both inserts of a, three slots each, contiguous")
}
```

- [ ] **Step 2: Add stubs and watch the tests fail**

Create `internal/index/secondarykey.go` with stubs:

```go
package index

import "manhhung2111/go-sql/internal/storage"

func EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error) { return nil, nil }

func SecondaryEqualRange(v any) (lo, hi []byte, err error) { return nil, nil, nil }
```

Run: `go test ./internal/index/ -run 'Secondary' 2>&1 | tail -15`
Expected: FAIL on assertions.

- [ ] **Step 3: Implement**

Replace `internal/index/secondarykey.go`:

```go
package index

import (
	"encoding/binary"
	"errors"
	"fmt"

	"manhhung2111/go-sql/internal/storage"
)

// secondaryRowIDSize is the page and slot that end every secondary key.
const secondaryRowIDSize = 8

// valuePart is the order-preserving, prefix-free encoding of v: no value's
// encoding is a prefix of another's, so appending anything to one never sorts
// among the entries of another value.
func valuePart(v any) ([]byte, error) {
	switch x := v.(type) {
	case int64:
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(x)^(1<<63))
		return key, nil
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case string:
		// 0x00 is written 0x00 0xFF and the value ends with 0x00 0x01: the
		// terminator sorts below every escaped byte pair and every other byte
		// after a 0x00, so a prefix sorts before its extensions.
		out := make([]byte, 0, len(x)+2)
		for i := 0; i < len(x); i++ {
			if x[i] == 0 {
				out = append(out, 0x00, 0xFF)
				continue
			}
			out = append(out, x[i])
		}
		return append(out, 0x00, 0x01), nil
	case nil:
		return nil, errors.New("cannot index a NULL value")
	default:
		return nil, fmt.Errorf("unsupported key type %T", v)
	}
}

// EncodeSecondaryKey returns the tree key for the entry of value v at row id:
// the value part, then the page and slot as 4 bytes each, big-endian. Entries of
// one value are adjacent and ordered by RowID, which is file order. Page and slot
// must be non-negative and fit in 32 bits.
func EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error) {
	part, err := valuePart(v)
	if err != nil {
		return nil, err
	}
	key := make([]byte, len(part)+secondaryRowIDSize)
	copy(key, part)
	binary.BigEndian.PutUint32(key[len(part):], uint32(id.Page))
	binary.BigEndian.PutUint32(key[len(part)+4:], uint32(id.Slot))
	return key, nil
}

// SecondaryEqualRange returns the half-open key range [lo, hi) that holds every
// entry of v and no other. For a predicate: col = v is [lo, hi); col >= v and
// col < v use lo as their bound; col > v and col <= v use hi.
func SecondaryEqualRange(v any) (lo, hi []byte, err error) {
	part, err := valuePart(v)
	if err != nil {
		return nil, nil, err
	}
	hi = make([]byte, 0, len(part)+secondaryRowIDSize+1)
	hi = append(hi, part...)
	for i := 0; i < secondaryRowIDSize; i++ {
		hi = append(hi, 0xFF)
	}
	hi = append(hi, 0x00)
	return part, hi, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/index/ 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Mutation checks and commit**

Confirm each fails a test, then restore: (a) write strings as raw bytes without escape or terminator (`out = append(out, x...)`, no terminator) — the ordering and equal-range tests must fail; (b) `hi` as `part‖0x00` — the equal-range test must fail; (c) drop the slot from the key — the ordering test must fail.

Run: `gofmt -l . ; go vet ./... && go test -race ./...`

```bash
git add internal/index
git commit -m "feat(index): add the secondary key encoding for non-unique indexes

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `sys_indexes` store and load

**Files:**
- Modify: `internal/engine/catalogstore.go`, `internal/engine/catalogload.go`
- Test: `internal/engine/indexcatalog_test.go` (create)

**Interfaces:**
- Consumes: `catalogStore`, `insertSynced`, `deleteSynced`, `encodeRowChecked`, `DecodeRow`, `tableKey`, `survivor`, test helpers `newTestStore`, `newTestStoreAt`, `newCrashFixture`, `mustEncodeTableRow`, `idColumn`.
- Produces:
  - `type indexDef struct{ name, column string }` (in `indexdef.go`, created here)
  - `sysIndexesColumns`, `sysIndexesFile`, `indexKey{database, table, name}`
  - `(*catalogStore).addIndex(database, table, name, column string) error`
  - `(*catalogStore).removeIndex(database, table, name string) error`
  - `(*catalogStore).stageIndexRows(database, oldTable, newTable string, renames map[string]string) (*stagedIndexRows, error)`; `(*catalogStore).commitStaged(*stagedIndexRows)`; `(*catalogStore).abortStaged(*stagedIndexRows)`
  - `(*catalogStore).removeTableIndexes(database, table string) error`
  - `loadedTable.indexes []indexDef`

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/indexcatalog_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// sysIndexRecords reads every live sys_indexes row as "table.name(column)".
func sysIndexRecords(t *testing.T, store *catalogStore) []string {
	t.Helper()
	var out []string
	for row, err := range store.indexes.Scan() {
		require.NoError(t, err)
		values, err := DecodeRow(sysIndexesColumns, row.Bytes)
		require.NoError(t, err)
		out = append(out, values[1].(string)+"."+values[2].(string)+"("+values[3].(string)+")")
	}
	return out
}

func TestCatalogStore_IndexRows(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.addIndex("shop", "users", "by_name", "name"))
	require.NoError(t, store.addIndex("shop", "users", "by_age", "age"))
	assert.Equal(t, []string{"users.by_name(name)", "users.by_age(age)"}, sysIndexRecords(t, store))

	require.NoError(t, store.removeIndex("shop", "users", "by_name"))
	assert.Equal(t, []string{"users.by_age(age)"}, sysIndexRecords(t, store))

	assert.Error(t, store.removeIndex("shop", "users", "by_name"), "already gone")
}

func TestCatalogStore_StagedIndexRows(t *testing.T) {
	setup := func(t *testing.T) *catalogStore {
		store := newTestStore(t)
		require.NoError(t, store.addIndex("shop", "users", "i1", "name"))
		require.NoError(t, store.addIndex("shop", "users", "i2", "age"))
		require.NoError(t, store.addIndex("shop", "other", "o1", "x"))
		return store
	}

	t.Run("a table rename: the copies are visible before the commit, the old rows after", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "users", "people", nil)
		require.NoError(t, err)
		assert.Len(t, sysIndexRecords(t, store), 5, "old and new rows coexist")

		store.commitStaged(staged)
		assert.ElementsMatch(t, []string{"other.o1(x)", "people.i1(name)", "people.i2(age)"}, sysIndexRecords(t, store))
		require.NoError(t, store.removeIndex("shop", "people", "i1"), "the map follows the new rows")
	})

	t.Run("a column rename changes only that column", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "users", "users", map[string]string{"name": "full_name"})
		require.NoError(t, err)
		store.commitStaged(staged)

		assert.ElementsMatch(t, []string{"other.o1(x)", "users.i1(full_name)", "users.i2(age)"}, sysIndexRecords(t, store))
	})

	t.Run("abort removes the copies and keeps the originals", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "users", "people", nil)
		require.NoError(t, err)
		store.abortStaged(staged)

		assert.ElementsMatch(t, []string{"other.o1(x)", "users.i1(name)", "users.i2(age)"}, sysIndexRecords(t, store))
	})

	t.Run("a table with no indexes stages nothing", func(t *testing.T) {
		store := setup(t)

		staged, err := store.stageIndexRows("shop", "none", "still_none", nil)

		require.NoError(t, err)
		store.commitStaged(staged)
		assert.Len(t, sysIndexRecords(t, store), 3)
	})
}

func TestCatalogStore_RemoveTableIndexesAndSweeps(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.addDatabase("shop"))
	require.NoError(t, store.addIndex("shop", "users", "i1", "name"))
	require.NoError(t, store.addIndex("shop", "other", "o1", "x"))

	require.NoError(t, store.removeTableIndexes("shop", "users"))
	assert.Equal(t, []string{"other.o1(x)"}, sysIndexRecords(t, store))

	t.Run("adding a table sweeps leftover rows under its name", func(t *testing.T) {
		require.NoError(t, store.addIndex("shop", "ghost", "g1", "id"))
		require.NoError(t, store.addTable("shop", "ghost", mustEncodeTableRow(t, "shop", "ghost", 3, idColumn)))
		assert.Equal(t, []string{"other.o1(x)"}, sysIndexRecords(t, store))
	})

	t.Run("removing the database's tables removes its index rows", func(t *testing.T) {
		require.NoError(t, store.removeDatabaseTables("shop"))
		assert.Empty(t, sysIndexRecords(t, store))
	})
}

// A crash leaves any of these hand-made states. Loading must keep exactly the
// definitions that name a surviving table and an existing column.
func TestStartup_IndexRowsAreResolvedAgainstTheTables(t *testing.T) {
	named := func(name string) parser.ColumnDefinition {
		return parser.ColumnDefinition{Name: name, DataType: parser.IntDataType{}}
	}
	setup := func(t *testing.T) *crashFixture {
		f := newCrashFixture(t)
		f.database("shop")
		require.NoError(t, f.store.files.makeDir(f.store.files.databaseDir("shop")))
		f.file("shop", 1)
		require.NoError(t, f.store.addTable("shop", "users", mustEncodeTableRow(t, "shop", "users", 1, named("id"), named("age"))))
		return f
	}
	definitions := func(t *testing.T, c Catalog) []indexDef {
		db, err := c.GetDatabase("shop")
		require.NoError(t, err)
		table, ok := db.GetTable("users")
		require.True(t, ok)
		return table.(*SqlTable).indexDefs
	}

	t.Run("a valid row survives", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
	})
	t.Run("a row for a missing table is dropped", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "gone", "x", "age"))
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
		store := newTestStoreAt(t, f.dir) // reopen to read what is left on disk
		assert.Equal(t, []string{"users.by_age(age)"}, sysIndexRecords(t, store))
	})
	t.Run("a row naming a column the schema lacks is dropped", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "users", "stale", "old_name"))

		c := f.mustOpen()

		assert.Empty(t, definitions(t, c))
	})
	t.Run("a crash between the insert and the tombstone of a column rename keeps the new row", func(t *testing.T) {
		f := setup(t)
		// table row already says "age"; both the old ("years") and new ("age") index rows exist
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "years"))
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
	})
	t.Run("a crash before the table row of a column rename keeps the old row", func(t *testing.T) {
		f := setup(t)
		// table row still says "age"; the staged copy names a column that does not exist yet
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "age"))
		require.NoError(t, f.store.addIndex("shop", "users", "by_age", "years"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "by_age", column: "age"}}, definitions(t, c))
	})
	t.Run("two valid rows with one name keep the first", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, f.store.addIndex("shop", "users", "dup", "age"))
		require.NoError(t, f.store.addIndex("shop", "users", "dup", "id"))

		c := f.mustOpen()

		assert.Equal(t, []indexDef{{name: "dup", column: "age"}}, definitions(t, c))
	})
}
```

(`c.GetDatabase(name)` returns `(Database, error)`.)

- [ ] **Step 2: Add stubs and watch the tests fail**

Create `internal/engine/indexdef.go`:

```go
package engine

// indexDef is one CREATE INDEX definition: an index named name on a column,
// by name so a column rename only has to rewrite the definition.
type indexDef struct {
	name   string
	column string
}
```

Add `indexes storage.File` to `catalogStore`, `indexRows map[indexKey]storage.RowID`, and compile-only stubs for the methods and `loadedTable.indexes`, `SqlTable.indexDefs []indexDef` (a field on the struct in `table.go`), returning `nil`/zero. Add `sysIndexesColumns`/`sysIndexesFile`/`indexKey` (see Step 3 for their values).

Run: `go test ./internal/engine/ -run 'IndexRows|StagedIndexRows|RemoveTableIndexes|IndexRowsAreResolved' 2>&1 | tail -20`
Expected: FAIL on assertions or on nil-store panics; it must compile.

- [ ] **Step 3: Implement the store**

In `catalogstore.go`:

```go
const sysIndexesFile = "sys_indexes.tbl"

var sysIndexesColumns = []parser.ColumnDefinition{
	{Name: "db", DataType: parser.TextDataType{}},
	{Name: "tbl", DataType: parser.TextDataType{}},
	{Name: "name", DataType: parser.TextDataType{}},
	{Name: "col", DataType: parser.TextDataType{}},
}

type indexKey struct{ database, table, name string }

// indexRecord is one decoded sys_indexes row.
type indexRecord struct {
	database, table, name, column string
}
```

Struct: add `indexes storage.File` and `indexRows map[indexKey]storage.RowID` to `catalogStore`; `newCatalogStore(files, databases, tables, indexes storage.File)` (update its callers: `openCatalogStore`, tests' `newTestStoreAt` if it calls it — search with `grep -rn "newCatalogStore(" internal`). `openCatalogStore` opens `sys_indexes.tbl` with `openOrCreate` after `sys_tables`, closing the earlier files if it fails. `Close` closes all three (`errors.Join`).

Row codec and store methods:

```go
func encodeIndexRow(r indexRecord) ([]byte, error) {
	row, err := encodeRowChecked(sysIndexesColumns, []any{r.database, r.table, r.name, r.column})
	if err != nil {
		return nil, fmt.Errorf("index %q: %w", r.name, err)
	}
	return row, nil
}

func decodeIndexRecord(b []byte) (indexRecord, error) {
	values, err := DecodeRow(sysIndexesColumns, b)
	if err != nil {
		return indexRecord{}, fmt.Errorf("decoding sys_indexes row: %w", err)
	}
	database, ok1 := values[0].(string)
	table, ok2 := values[1].(string)
	name, ok3 := values[2].(string)
	column, ok4 := values[3].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return indexRecord{}, errors.New("decoding sys_indexes row: a column is NULL")
	}
	return indexRecord{database, table, name, column}, nil
}

// addIndex makes a definition durable; the row is the commit point.
func (s *catalogStore) addIndex(database, table, name, column string) error {
	row, err := encodeIndexRow(indexRecord{database, table, name, column})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := insertSynced(s.indexes, row)
	if err != nil {
		return err
	}
	s.indexRows[indexKey{database, table, name}] = id
	return nil
}

func (s *catalogStore) removeIndex(database, table, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := indexKey{database, table, name}
	id, ok := s.indexRows[key]
	if !ok {
		return fmt.Errorf("index %q has no catalog row", name)
	}
	if err := deleteSynced(s.indexes, id); err != nil {
		return err
	}
	delete(s.indexRows, key)
	return nil
}

// tombstoneIndexRowsLocked tombstones every known index row for which match is
// true. A row whose tombstone fails stays in the map so a later sweep retries it.
func (s *catalogStore) tombstoneIndexRowsLocked(match func(indexKey) bool) error {
	var errs []error
	removed := false
	for key, id := range s.indexRows {
		if !match(key) {
			continue
		}
		if err := s.indexes.Delete(id); err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", key.name, err))
			continue
		}
		delete(s.indexRows, key)
		removed = true
	}
	if removed {
		if err := s.indexes.Sync(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeTableIndexes tombstones every index row of one table.
func (s *catalogStore) removeTableIndexes(database, table string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tombstoneIndexRowsLocked(func(k indexKey) bool { return k.database == database && k.table == table })
}

// stagedIndexRows is the new copy of a table's index rows, written before the
// table row changes. old holds the rows to tombstone once it has.
type stagedIndexRows struct {
	database, table, newTable string
	old                       map[indexKey]storage.RowID
	added                     map[indexKey]storage.RowID
}

// stageIndexRows writes copies of table's index rows under newTable, with each
// column renamed by renames (a nil map renames none), and syncs them. The
// originals stay until commitStaged. Rows left under newTable by an earlier,
// dropped table are swept first, so they never attach to this one.
func (s *catalogStore) stageIndexRows(database, table, newTable string, renames map[string]string) (*stagedIndexRows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	staged := &stagedIndexRows{database: database, table: table, newTable: newTable,
		old: map[indexKey]storage.RowID{}, added: map[indexKey]storage.RowID{}}
	for key, id := range s.indexRows {
		if key.database == database && key.table == table {
			staged.old[key] = id
		}
	}
	if len(staged.old) == 0 {
		return staged, nil
	}
	if newTable != table {
		if err := s.tombstoneIndexRowsLocked(func(k indexKey) bool { return k.database == database && k.table == newTable }); err != nil {
			return nil, err
		}
	}
	cleanup := func() {
		for _, id := range staged.added {
			_ = s.indexes.Delete(id)
		}
		_ = s.indexes.Sync()
	}
	for key := range staged.old {
		column, err := s.columnOfLocked(key)
		if err != nil {
			cleanup()
			return nil, err
		}
		if renamed, ok := renames[column]; ok {
			column = renamed
		}
		row, err := encodeIndexRow(indexRecord{database, newTable, key.name, column})
		if err != nil {
			cleanup()
			return nil, err
		}
		id, err := s.indexes.Insert(row)
		if err != nil {
			cleanup()
			return nil, err
		}
		staged.added[indexKey{database, newTable, key.name}] = id
	}
	if err := s.indexes.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	return staged, nil
}
```

`columnOfLocked(key)` reads the row's column: `b, err := s.indexes.Get(s.indexRows[key])`, then `decodeIndexRecord`. (Rows are tiny; this is a few `Get`s per rename.)

```go
// commitStaged tombstones the originals and makes the copies the live rows. A
// failed tombstone is ignored: the load-time check drops the stale row, because
// the table row it was validated against has already changed.
func (s *catalogStore) commitStaged(staged *stagedIndexRows) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, id := range staged.old {
		_ = s.indexes.Delete(id)
		delete(s.indexRows, key)
	}
	_ = s.indexes.Sync()
	for key, id := range staged.added {
		s.indexRows[key] = id
	}
}

// abortStaged tombstones the copies; the originals are untouched.
func (s *catalogStore) abortStaged(staged *stagedIndexRows) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range staged.added {
		_ = s.indexes.Delete(id)
	}
	_ = s.indexes.Sync()
}
```

`commitStaged` deletes every old key from the map before setting every added key, so it handles a column rename (same key, new row) and a table rename (new key) uniformly.

Sweeps: in `addTable`, after the row is recorded, call `s.tombstoneIndexRowsLocked` for `(database, name)`; in `addDatabase`, for the whole database; in `tombstoneTablesLocked` (used by `removeDatabaseTables` and the sweep in `addDatabase`) also tombstone the database's index rows (call `tombstoneIndexRowsLocked` and join its error).

- [ ] **Step 4: Implement the load**

In `catalogload.go`: add `indexes []indexDef` to `loadedTable`. In `load`, after the `for _, sv := range all` loop has built `loaded` (and before returning), add the index pass:

```go
	// Index definitions. A row is valid if its table survived and has the
	// column; the others, and repeats of a valid (db, table, name), are stale.
	columnsOf := map[tableKey]map[string]bool{}
	for _, sv := range all {
		if sv.dead {
			continue
		}
		columns, _ := DecodeSchema(sv.record.schema) // already decoded successfully above
		names := map[string]bool{}
		for _, c := range columns {
			names[c.Name] = true
		}
		columnsOf[tableKey{sv.record.database, sv.record.name}] = names
	}
	defs := map[tableKey][]indexDef{}
	seen := map[indexKey]bool{}
	var staleIndexes []storage.RowID
	for row, err := range s.indexes.Scan() {
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("reading sys_indexes: %w", err)
		}
		record, err := decodeIndexRecord(row.Bytes)
		if err != nil {
			closeAll()
			return nil, err
		}
		tk := tableKey{record.database, record.table}
		key := indexKey{record.database, record.table, record.name}
		if !columnsOf[tk][record.column] || seen[key] {
			staleIndexes = append(staleIndexes, row.ID)
			continue
		}
		seen[key] = true
		s.indexRows[key] = row.ID
		defs[tk] = append(defs[tk], indexDef{name: record.name, column: record.column})
	}
	if err := tombstoneAll(s.indexes, staleIndexes); err != nil {
		closeAll()
		return nil, err
	}
	for db, tables := range loaded.databases {
		for i := range tables {
			tables[i].indexes = defs[tableKey{db, tables[i].name}]
		}
	}
```

(`columnsOf[tk]` on a missing table is a nil map, so the lookup is false.) In `newLoadedDatabase`, set `indexDefs: lt.indexes` on each `SqlTable`.

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/engine/ -run 'IndexRows|StagedIndexRows|RemoveTableIndexes|IndexRowsAreResolved|CatalogStore|Startup|Restart' 2>&1 | tail -20`
Expected: PASS, with every existing catalog and startup test still green.

- [ ] **Step 6: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...`

```bash
git add internal/engine
git commit -m "feat(engine): store index definitions in sys_indexes and resolve them at startup

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `CreateIndex`, `DropIndex`, and the statements around them

**Files:**
- Modify: `internal/engine/indexdef.go`, `internal/engine/table.go` (`Table` interface; `Rename`; `renameColumn`; `dropColumn`), `internal/engine/database.go` (`DropTable`), `internal/engine/engine.go`
- Test: `internal/engine/indexcatalog_test.go`

**Interfaces:**
- Consumes: Task 3's store methods and `indexDef`.
- Produces: `Table.CreateIndex(name, column string, ifNotExists bool) error`, `Table.DropIndex(name string, ifExists bool) error`; `Engine.Execute` cases for both statements.

- [ ] **Step 1: Write the failing tests**

Append to `internal/engine/indexcatalog_test.go`:

```go
const peopleDDL = "CREATE TABLE people (id INT PRIMARY KEY, name VARCHAR(20), age INT)"

func newIndexEngine(t *testing.T) (string, Catalog, Engine) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", peopleDDL)
	return dir, c, e
}

func defsOf(t *testing.T, c Catalog, table string) []indexDef {
	t.Helper()
	db, err := c.GetDatabase("shop")
	require.NoError(t, err)
	tbl, ok := db.GetTable(table)
	require.True(t, ok)
	return tbl.(*SqlTable).indexDefs
}

func TestCreateIndex_StatementAndErrors(t *testing.T) {
	_, c, e := newIndexEngine(t)

	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))

	t.Run("a repeated name is an error unless IF NOT EXISTS", func(t *testing.T) {
		assert.ErrorContains(t, execError(t, e, "shop", "CREATE INDEX by_name ON people (age)"),
			`index "by_name" already exists on table "people"`)
		mustExec(t, e, "shop", "CREATE INDEX IF NOT EXISTS by_name ON people (age)")
		assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	})
	t.Run("an unknown column or table", func(t *testing.T) {
		assert.ErrorContains(t, execError(t, e, "shop", "CREATE INDEX x ON people (nope)"), `unknown column "nope"`)
		assert.ErrorContains(t, execError(t, e, "shop", "CREATE INDEX x ON nope (id)"), `table "nope" does not exist`)
	})
	t.Run("a key column and several indexes on one column are fine", func(t *testing.T) {
		mustExec(t, e, "shop", "CREATE INDEX by_id ON people (id)")
		mustExec(t, e, "shop", "CREATE INDEX by_name2 ON people (name)")
		assert.Len(t, defsOf(t, c, "people"), 3)
	})
	t.Run("drop", func(t *testing.T) {
		mustExec(t, e, "shop", "DROP INDEX by_name2 ON people")
		assert.Len(t, defsOf(t, c, "people"), 2)
		assert.ErrorContains(t, execError(t, e, "shop", "DROP INDEX by_name2 ON people"),
			`index "by_name2" does not exist on table "people"`)
		mustExec(t, e, "shop", "DROP INDEX IF EXISTS by_name2 ON people")
	})
}

func TestCreateIndex_SurvivesARestart(t *testing.T) {
	dir, c, e := newIndexEngine(t)
	mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")
	mustExec(t, e, "shop", "CREATE INDEX by_age ON people (age)")
	mustExec(t, e, "shop", "DROP INDEX by_age ON people")

	c, e = restart(t, dir, c)

	assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	_ = e
}

func TestCreateIndex_FollowsRenamesAndDrops(t *testing.T) {
	t.Run("RENAME TO", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "shop", "ALTER TABLE people RENAME TO humans")
		c, _ = restart(t, dir, c)

		assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "humans"))
	})
	t.Run("RENAME COLUMN", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "shop", "ALTER TABLE people RENAME COLUMN name TO full_name")
		assert.Equal(t, []indexDef{{"by_name", "full_name"}}, defsOf(t, c, "people"))
		c, _ = restart(t, dir, c)

		assert.Equal(t, []indexDef{{"by_name", "full_name"}}, defsOf(t, c, "people"))
	})
	t.Run("DROP COLUMN of an indexed column is refused, of another is not", func(t *testing.T) {
		_, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		err := execError(t, e, "shop", "ALTER TABLE people DROP COLUMN name")
		assert.ErrorContains(t, err, `cannot drop column "name": index "by_name" uses it`)
		mustExec(t, e, "shop", "ALTER TABLE people DROP COLUMN age")

		assert.Equal(t, []indexDef{{"by_name", "name"}}, defsOf(t, c, "people"))
	})
	t.Run("DROP TABLE then CREATE TABLE of the same name starts with no indexes, also after a restart", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "shop", "DROP TABLE people")
		mustExec(t, e, "shop", peopleDDL)
		assert.Empty(t, defsOf(t, c, "people"))
		c, _ = restart(t, dir, c)

		assert.Empty(t, defsOf(t, c, "people"))
	})
	t.Run("DROP DATABASE removes the rows", func(t *testing.T) {
		dir, c, e := newIndexEngine(t)
		mustExec(t, e, "shop", "CREATE INDEX by_name ON people (name)")

		mustExec(t, e, "", "DROP DATABASE shop")
		mustExec(t, e, "", "CREATE DATABASE shop")
		mustExec(t, e, "shop", peopleDDL)
		c, _ = restart(t, dir, c)

		assert.Empty(t, defsOf(t, c, "people"))
	})
}

func TestCreateIndex_OnADroppedTableFailsWithoutTouchingTheCatalog(t *testing.T) {
	_, c, e := newIndexEngine(t)
	db, err := c.GetDatabase("shop")
		require.NoError(t, err)
	table, _ := db.GetTable("people")
	mustExec(t, e, "shop", "DROP TABLE people")

	err := table.CreateIndex("late", "name", false)

	assert.ErrorContains(t, err, "is closed")
}

// Crash states for RENAME TO, written by hand: the staged rows exist under the
// new name while the table row still has the old one, and the reverse.
func TestStartup_ARenameThatCrashedKeepsTheDefinitions(t *testing.T) {
	named := func(name string) parser.ColumnDefinition {
		return parser.ColumnDefinition{Name: name, DataType: parser.IntDataType{}}
	}
	for _, tc := range []struct {
		name, tableRow string
	}{
		{"before the table row was replaced", "users"},
		{"after the table row was replaced, before the old rows were tombstoned", "people"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrashFixture(t)
			f.database("shop")
			f.file("shop", 1)
			require.NoError(t, f.store.addTable("shop", tc.tableRow, mustEncodeTableRow(t, "shop", tc.tableRow, 1, named("id"))))
			require.NoError(t, f.store.addIndex("shop", "users", "i", "id"))
			require.NoError(t, f.store.addIndex("shop", "people", "i", "id"))

			c := f.mustOpen()

			db, err := c.GetDatabase("shop")
		require.NoError(t, err)
			table, ok := db.GetTable(tc.tableRow)
			require.True(t, ok)
			assert.Equal(t, []indexDef{{"i", "id"}}, table.(*SqlTable).indexDefs)
			store := newTestStoreAt(t, f.dir)
			assert.Len(t, sysIndexRecords(t, store), 1, "the other copy is gone")
		})
	}
}
```

`c.GetDatabase(name)` returns `(Database, error)`, as `hasTable` in `crash_test.go` uses it. Add the `parser` import to this test file. Where a snippet above declares `err` twice in one scope, the implementer renames one.

- [ ] **Step 2: Add stubs and watch the tests fail**

Add `CreateIndex` and `DropIndex` to the `Table` interface and as stubs on `*SqlTable` returning `nil`; add the two `Engine.Execute` cases calling them (Step 3 shows the cases). Run:

`go test ./internal/engine/ -run 'CreateIndex|ARenameThatCrashed' 2>&1 | tail -25`

Expected: FAIL on assertions (no definitions recorded). It must compile.

- [ ] **Step 3: Implement**

`indexdef.go`, append (add `"fmt"` and `"slices"`):

```go
// CreateIndex records an index definition. The catalog row is the commit point;
// the table's definitions change only after it is durable. The trees are not
// built here: the next statement that needs them builds them from one scan.
func (t *SqlTable) CreateIndex(name, column string, ifNotExists bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}
	if !slices.ContainsFunc(t.Columns, func(c parser.ColumnDefinition) bool { return c.Name == column }) {
		return fmt.Errorf("unknown column %q", column)
	}
	if slices.ContainsFunc(t.indexDefs, func(d indexDef) bool { return d.name == name }) {
		if ifNotExists {
			return nil
		}
		return fmt.Errorf("index %q already exists on table %q", name, t.Name)
	}
	if err := t.store.addIndex(t.database, t.Name, name, column); err != nil {
		return err
	}
	t.indexDefs = append(slices.Clone(t.indexDefs), indexDef{name: name, column: column})
	t.dropIndexes()
	return nil
}

// DropIndex removes an index definition; the tombstone is the commit point.
func (t *SqlTable) DropIndex(name string, ifExists bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}
	at := slices.IndexFunc(t.indexDefs, func(d indexDef) bool { return d.name == name })
	if at < 0 {
		if ifExists {
			return nil
		}
		return fmt.Errorf("index %q does not exist on table %q", name, t.Name)
	}
	if err := t.store.removeIndex(t.database, t.Name, name); err != nil {
		return err
	}
	t.indexDefs = slices.Delete(slices.Clone(t.indexDefs), at, at+1)
	t.dropIndexes()
	return nil
}
```

(import `parser` in `indexdef.go`.)

`table.go`: extend the `Table` interface with both methods and their one-line docs. Edit three methods:

- `Rename(name)`: after `encodeTableRow`, `staged, err := t.store.stageIndexRows(t.database, t.Name, name, nil)`; return on error. If `replaceTable` fails, `t.store.abortStaged(staged)` then return the error. After success, `t.store.commitStaged(staged)`, then `t.Name = name`.
- `renameColumn`: after `encodeTableRow`, `staged, err := t.store.stageIndexRows(t.database, t.Name, t.Name, map[string]string{oldName: newName})`; same abort/commit around `replaceTable`; after success also rewrite `t.indexDefs`: a clone where every `column == oldName` becomes `newName`.
- `dropColumn`: after the `idx == -1` check, `if at := slices.IndexFunc(t.indexDefs, func(d indexDef) bool { return d.column == name }); at >= 0 { return fmt.Errorf("cannot drop column %q: index %q uses it", name, t.indexDefs[at].name) }`.

`database.go`, `DropTable`: after `d.store.removeTable(d.Name, name)` succeeds and before `table.Drop()`, `_ = d.store.removeTableIndexes(d.Name, name)` — the table is gone either way, and a leftover row is swept by the next `addTable` or dropped at load.

`engine.go`, in the `Execute` type switch:

```go
	case parser.CreateIndexStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}
		table, err := resolveTable(database, stmt.Table)
		if err != nil {
			return Response{}, err
		}
		if err := table.CreateIndex(stmt.Name, stmt.Column, stmt.IfNotExists); err != nil {
			return Response{}, err
		}
		return Response{}, nil

	case parser.DropIndexStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}
		table, err := resolveTable(database, stmt.Table)
		if err != nil {
			return Response{}, err
		}
		if err := table.DropIndex(stmt.Name, stmt.IfExists); err != nil {
			return Response{}, err
		}
		return Response{}, nil
```

Also update `catalog.go`'s `DropDatabase` path if `removeDatabaseTables` is the only caller needing the index sweep (Task 3 already extended `tombstoneTablesLocked`).

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/engine/ 2>&1 | tail -20`
Expected: PASS. Anything that implements `Table` outside `SqlTable` (grep `Table interface` implementers in tests) needs the two methods; add no-op stubs there.

- [ ] **Step 5: Mutation checks**

Each must fail a test; restore with `git checkout internal` afterwards (commit first so that nothing is lost): (a) `CreateIndex` appends the definition to `t.indexDefs` before calling `addIndex` (a failing `addIndex`, injected with a closed store file, must leave no definition); (b) `Rename` replaces the table row before staging; (c) `commitStaged` skips tombstoning old rows; (d) `load` keeps a row whose column is missing; (e) `DropTable` skips `removeTableIndexes` and `addTable` skips its sweep (the "DROP TABLE then CREATE TABLE" test must fail); (f) `dropColumn` skips the indexed-column guard.

- [ ] **Step 6: Documentation, whole suite, commit**

In `CLAUDE.md`'s storage paragraph, add after the catalog sentence: "`CREATE INDEX name ON table (column)` and `DROP INDEX` record named secondary-index definitions in a third system file, `sys_indexes(db, tbl, name, col)`; a definition is kept at startup only if its table survives and still has the column, and renames write the new rows before replacing the table row. Definitions do nothing yet beyond being stored: building and using the entries is the next step." Remove the last clause when part B lands.

Run: `gofmt -l . ; go vet ./... && go test -race ./...`

```bash
git add internal/engine CLAUDE.md
git commit -m "feat(engine): add CREATE INDEX and DROP INDEX definitions

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
