# CREATE INDEX (non-unique, single column): design

Status: draft for review. Builds on the [engine index wiring](2026-10-12-engine-index-wiring-design.md), [equality lookups](2026-10-13-engine-equality-lookup-design.md), [range lookups](2026-10-13-range-lookup-design.md) and the [page-count rule](2026-10-14-page-count-range-rule-design.md) (PR #41, which this needs).

## Goal

Let a user name an index on any column: `CREATE INDEX name ON table (column)` and `DROP INDEX name ON table`. Equality and range `WHERE`s on that column are then served from the index, as they already are for PRIMARY KEY and UNIQUE columns, but the column may hold duplicates. The definition survives a restart; the entries are rebuilt from the heap, as today.

## Context

Today the only indexes are implicit: one tree per PRIMARY KEY/UNIQUE column, keyed by the column value, one `RowID` per key, built lazily from one scan and discarded whenever the file might disagree. A `WHERE` on any other column scans.

What a secondary index needs that the code does not have:

- **Syntax.** The parser has no `INDEX` or `ON` token and no `CREATE INDEX` or `DROP INDEX` statement. `CREATE` dispatches only to `DATABASE` and `TABLE`.
- **A durable definition.** The catalog holds `sys_databases` and `sys_tables` (the schema as JSON). Nothing records "this table has an index on that column".
- **Duplicate keys.** `index.Tree` holds one `RowID` per key, and the key encoding for strings is the raw bytes. That encoding is not safe to extend with a suffix: with entries `"a"‖rowid` and `"ab"‖rowid` sorted together, the entries of `"ab"` fall between those of `"a"` and any upper bound built from `"a"`. A secondary key needs an encoding where no value's encoding is a prefix of another's.
- **More trees to keep in step** on every write, and a planner that can choose them.

Index entries stay what they have been: a rebuildable, in-memory cache of the heap. Nothing about the data of an index is durable in this phase.

## Decisions made during design

- **Scope: non-unique, single column.** `CREATE UNIQUE INDEX`, composite keys and `SHOW INDEX` are later. PRIMARY KEY/UNIQUE columns keep their implicit indexes and their enforcement.
- **Definitions in a third system heap file, `sys_indexes`.** One row per index: `(db, table, name, column)`, with the column by name. It is written and fsynced like `sys_tables`: the row is the commit point of `CREATE INDEX`, and a tombstone is the commit point of `DROP INDEX`.
- **Lazy, like the implicit indexes.** `CREATE INDEX` writes its row and discards the table's trees; it does not scan the table. The first statement that can use the index builds it, in the same single scan that builds the others.
- **Names are per table.** `CREATE INDEX` on a column that is already a PRIMARY KEY/UNIQUE column, or that already has another index, is accepted (it is redundant, not wrong). Implicit indexes have no names and cannot be dropped.
- **One new key encoding for secondary entries**, order-preserving and prefix-free, ending in the `RowID`. Equal values sort in file order, which is the order a scan returns them.
- **A secondary index never degrades on duplicates.** Duplicates are legal. It can only degrade on a value too long to index (see "Long values").
- **`DROP COLUMN` of an indexed column is rejected**, with an error naming the index. The user drops the index first.
- **`INDEX` and `ON` become reserved words**, as in MySQL. A column or table named `index` or `on` can no longer be written without quoting, and the lexer has no quoting. Stored schemas are unaffected (they are JSON), so existing data still loads; only new SQL text using such names fails.

## Design

### Syntax and parser (`internal/parser`)

```
CREATE INDEX [IF NOT EXISTS] name ON table (column)
DROP INDEX [IF EXISTS] name ON table
```

- Tokens `INDEX` and `ON`, added to the keyword map. `parseCreate…` dispatches on the token after `CREATE`: `DATABASE`, `TABLE` or `INDEX`; `DROP` likewise.
- New AST nodes, plain structs like the others:

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

- A list of two or more columns, `UNIQUE` before `INDEX`, and anything after `)` except `;` are parse errors. The error for a column list says composite indexes are not supported. As everywhere in the parser, each token is peeked and its type checked before it is consumed.

### Engine statements (`internal/engine`)

`Engine.Execute` gains cases for both statements. They resolve the database and table as `ALTER TABLE` does and call new methods on the table interface:

```go
CreateIndex(name, column string, ifNotExists bool) error
DropIndex(name string, ifExists bool) error
```

`CreateIndex`, under the table's write lock: fail if the table is closed; look the column up (`column %q does not exist`); fail if the name exists (`index %q already exists on table %q`, or no-op with `IF NOT EXISTS`); write the `sys_indexes` row (insert, sync); append the definition to the table's list; discard the table's trees. `DropIndex` is the mirror: tombstone the row, remove the definition, discard the trees; a missing name is `index %q does not exist on table %q` or a no-op with `IF EXISTS`.

The table keeps its definitions as a small slice `[]indexDef{name, column string}`, guarded by `Table.mu` like the schema. Lock order is unchanged: `Table.mu` → `tableIndexes.mu` → `catalogStore.mu`.

### Durability (`catalogstore.go`, `catalogload.go`)

`sys_indexes` is created beside the other two (`openOrCreate`) with columns `db`, `table`, `name`, `column`. `catalogStore` gains `indexRows map[indexKey]storage.RowID` with `indexKey{database, table, name}`, and `addIndex`, `removeIndex`, `removeTableIndexes(database, table)`.

Loading reads `sys_indexes` after the tables are resolved, and keeps a row only if its table survives and that table's schema still has the column; every other row is tombstoned like the stale table rows are. A repeated `(db, table, name)` keeps the first. Each table is then opened with its definitions.

Because dangling rows are dropped at load, every statement that renames or removes something an index row names can be ordered so that a crash at any point leaves a state the load resolves:

- **`DROP TABLE`:** tombstone the table row (the commit point, as today), then tombstone its index rows. A crash between leaves index rows for a missing table, which load drops.
- **`DROP DATABASE`:** tombstone its index rows along with its table rows, in the same step.
- **`RENAME TO`:** insert the index rows under the new table name and sync, then replace the table row, then tombstone the old index rows. A crash before the replace leaves new-name rows whose table does not exist (dropped); a crash after leaves old-name rows for the same reason, and the new ones are valid.
- **`RENAME COLUMN`:** the same order with the column name: insert the renamed rows, replace the table row, tombstone the old rows. A row naming a column the schema lacks is dropped at load.
- **`ADD COLUMN`** rewrites the file and changes no definition. **`DROP COLUMN`** of an indexed column is refused before anything is written; of any other column it changes no definition.

A crash can only lose an index definition by dropping a row that was dangling; it never loses the table or its data.

### Secondary key encoding (`internal/index`)

```go
// EncodeSecondaryKey returns the tree key for the entry of value v at row id.
func EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error)

// SecondaryEqualRange returns the half-open key range that holds every entry of v.
func SecondaryEqualRange(v any) (lo, hi []byte, err error)
```

The value part is order-preserving and prefix-free:

| Type | Value part |
| --- | --- |
| `int64` | the 8-byte big-endian form with the sign bit flipped (as `EncodeKey`) |
| `bool` | one byte, 0 or 1 |
| `string` | the bytes with each `0x00` written as `0x00 0xFF`, then the terminator `0x00 0x01` |

followed by the `RowID` as 4 bytes of page and 4 bytes of slot, big-endian (both non-negative). No value part is a prefix of another of its type, and the escape keeps byte order equal to string order, so all entries of one value are adjacent, ordered by `RowID` (file order), and ordered between values by value.

Ranges on a secondary tree, with `V` the value part of the literal and `F` the eight bytes `0xFF`:

| Predicate | Lower bound | Upper bound |
| --- | --- | --- |
| `col = v` | `V` | `V‖F‖0x00` |
| `col >= v` | `V` | none |
| `col > v` | `V‖F‖0x00` | none |
| `col < v` | none | `V` |
| `col <= v` | none | `V‖F‖0x00` |

`V‖F‖0x00` is above every entry of `v` (a `RowID` is at most `0x7FFFFFFF_7FFFFFFF`) and below every entry of any greater value. These bounds are the unique-key ones with the zero-byte successor replaced by the entry-wide one; the planner's "combine to the tightest range" logic is unchanged.

### Long values

A secondary entry is `value part + 8` bytes and must not exceed the tree's `MaxKeyLen`. `INSERT`, `UPDATE` and `ADD COLUMN` on a table with an index on a column reject a value whose entry would be too long with `key too long for index %q: %d bytes exceeds the maximum of %d`, writing nothing, as they do for PRIMARY KEY/UNIQUE strings (the check is on the encoded size, which is up to twice the string for strings full of zero bytes).

A value already in the file when the index is created may be too long. The build then degrades that one index: its tree is nil and statements on that column scan, exactly like a degraded unique column. Nothing fails. Rows with `NULL` in the column are not indexed, as before.

### Index state and maintenance (`tableindex.go`)

`tableIndexes` gains `secondary map[string]*index.Tree`, keyed by index name; a nil tree means degraded. `buildIndexes` fills it in the same single scan: for each live row and each definition, insert `EncodeSecondaryKey(value, rowID)`.

The write paths already compute the key values of a row's PRIMARY KEY/UNIQUE columns before a write and update the trees after the heap write and `Sync` succeed (any error discards every tree). They are extended to carry the indexed columns' values too:

- **INSERT:** add `(value, newRowID)` for each indexed column of each new row.
- **DELETE:** remove `(value, oldRowID)`.
- **UPDATE:** remove `(oldValue, oldRowID)` and add `(newValue, newRowID)` for each updated row, whether or not the value changed, because the row moves to the end of the file. Removals come first.
- **ADD/DROP COLUMN, close, any write error:** the trees are discarded, as today.
- **RENAME COLUMN of an indexed column:** the definition's column name changes; the trees need not be discarded because column positions do not change.

A removal that finds nothing to remove (`Delete` reports absent) discards the trees, since it means the index disagreed with the heap.

### Planner (`lookup.go`)

`rangePlan` gains the source tree: a key column's unique tree, or a named secondary tree. `planLookup` already walks the AND chain and validates every comparison; the only change is which columns can carry a range: a PRIMARY KEY/UNIQUE column (unique tree), or a column with a secondary index.

Choosing among plans, first match wins: an equality on a PRIMARY KEY/UNIQUE column (one row, unconditional); then a range on a unique tree; then a range or equality on a secondary tree. An equality on a secondary index is a range `[V, V‖F‖0x00)` and goes through the same page-count rule as any range: the distinct pages of the matching rows are counted, and the lookup is used while `rangeCostFactor × pages ≤ filePages`. A secondary index on a column with few distinct values therefore costs nothing: the rule refuses it and the statement scans.

Everything else about a lookup is unchanged: it is used only for a `WHERE` that cannot fail at run time, the whole `WHERE` is re-evaluated on each fetched row, rows come back in file order (sorted by (page, slot), then fetched with `GetMany`), and a stale entry discards the indexes and falls back to the scan. `UPDATE` uses a lookup only if every unique column it assigns has a tree (secondary trees impose no conflicts).

### Documentation

`CLAUDE.md`'s storage paragraph gains `CREATE INDEX`/`DROP INDEX`, `sys_indexes` beside the other system files, the secondary key encoding in one sentence, and the updated lookup rule. The lock order sentence is unchanged.

## Error handling

- Parse errors follow the parser's conventions: lowercase, `%q`, "expected X, got Y".
- A failed catalog write leaves memory unchanged: the definition is added to the table only after the row is durable.
- A crash at any point resolves at load (see "Durability"); nothing user-visible is lost except a dangling definition.
- A secondary index can only make a statement faster or equal: every error a scan would report is still reported by the scan, because a lookup is used only for a `WHERE` that cannot fail.
- A stale or disagreeing entry costs a rebuild, never a wrong result.

## Out of scope (YAGNI)

- `CREATE UNIQUE INDEX`, composite and prefix indexes, functional indexes, `SHOW INDEX`, `EXPLAIN`.
- Using an index for `ORDER BY`, `LIMIT`, `OR`, `IN` or `!=`.
- The on-disk index format and index metadata beyond the definition row.
- Statistics: the page-count rule is the only cost model.
- Quoted identifiers, so that a column called `index` could still be written.

## Testing and verification

Pure Go with `testify`, `go test -race ./...`. Every test is seen failing first. The existing `countingFile`, `noIndexes` differential harness and `failingFile` are reused.

- **Parser:** both statements, with and without `IF [NOT] EXISTS`, in lower and upper case; errors for a missing name, `ON`, table, parenthesis or column, a two-column list, `UNIQUE INDEX`, and trailing tokens; `index` and `on` as column names are now parse errors.
- **Key encoding:** `EncodeSecondaryKey` orders by value then `RowID` across ints (negatives, extremes), strings (empty, embedded zero bytes, one a prefix of another) and bools; no value part is a prefix of another; `SecondaryEqualRange` holds exactly the entries of its value and none of its neighbours (for a seeded random set, compared with a brute-force filter).
- **Catalog and restart:** an index survives a restart; a dropped one does not; `DROP TABLE`, `DROP DATABASE`, `RENAME TO` and `RENAME COLUMN` carry or remove the rows; `DROP COLUMN` of an indexed column fails and writes nothing; fault injection at each step of the rename orderings and after the table row of a drop leaves a catalog that loads with the right definitions and no dangling rows.
- **Fewer reads:** with a duplicate-rich column indexed, an equality `SELECT`, `UPDATE` and `DELETE` do no `Scan` and read each page of the matches once; an equality on a column where the matches lie on more than half the pages scans; a second unindexed column still scans.
- **Same answers, errors and order as the scan:** the seeded differential test (indexed against `noIndexes`) gains a table with a secondary index on a duplicate-rich column and equality, range and mixed `WHERE`s on it, with INSERTs, UPDATEs that move rows and DELETEs, comparing results, errors and the file after every statement; also with the factor at 0 and at a huge value.
- **Maintenance:** after each kind of write every tree agrees with a fresh build (`assertIndexesMatchHeap` extended to secondary trees); an injected `Insert`, `Sync` or `Delete` failure discards them.
- **Long values:** an over-long value in an indexed column is rejected on `INSERT`/`UPDATE`/`ADD COLUMN`; one written before the index existed degrades only that index and results still match the scan.
- **Mutation checks**, each confirmed to fail a test: sort entries without the `RowID` suffix; use the raw string bytes as the value part; upper bound `V‖0x00` instead of `V‖F‖0x00`; skip the secondary update after an UPDATE (re-point only on key change); skip the removal on DELETE; keep the trees across `CREATE INDEX`; keep a dangling index row at load; drop the table row before the new index rows in `RENAME TO`.
- A one-off measurement on a 100,000-row table (not committed), quoted in the PR: an equality `SELECT` on a duplicate-rich non-key column, before and after the index, and one where the rule refuses.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order and PR split

1. **Parser:** tokens, AST nodes, parsing, tests.
2. **Key encoding:** `EncodeSecondaryKey`, `SecondaryEqualRange`, tests (pure).
3. **Catalog:** `sys_indexes`, load, the rename/drop orderings, and `CreateIndex`/`DropIndex` on the table, with restart and fault tests (the index does nothing yet).
4. **Maintenance and planner:** secondary trees built and kept in step, the planner, the differential test, `CLAUDE.md`, mutation checks and the measurement.

Steps 1 to 3 are one PR (the feature exists and is durable but inert), step 4 a second.

## Open questions

None blocking. The reserved-word cost (a column named `index` or `on`) is the only user-visible break.
