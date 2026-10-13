# Engine index wiring: design

Status: draft for review. Fourth and last sub-project of the in-memory B-tree index; it uses the [key codec](2026-10-12-index-key-codec-design.md), the [in-memory B+ tree](2026-10-12-in-memory-btree-design.md) and the [heap `Get`](2026-10-12-heap-get-design.md).

## Goal

Make PRIMARY KEY and UNIQUE checks, and equality lookups on those columns, use an in-memory index instead of streaming the whole table. Today every INSERT, UPDATE and equality `WHERE` reads every page. No statement's result changes, apart from one new error for over-long keys (see "Long keys").

## Context

The index is a rebuildable cache of the heap: the heap write is the commit point, there is no WAL, and a missing or stale index is rebuilt from the heap. In memory first; the on-disk format is a later phase.

What the engine does today, in `internal/engine/table.go`:

- `InsertValues` calls `checkUniqueness`, which collects the batch's key values and streams the file once (`existingKeys`) to find which already exist.
- `Update` scans the file once to find matching rows and, in the same pass, which unassigned rows already hold an assigned value.
- `Select` and `Delete` scan the file and evaluate the whole `WHERE` on every row.
- `ALTER ADD/DROP COLUMN` rewrites the table into a new file (new `file_id`), and `adoptFile` swaps it in. `RENAME COLUMN` and `RENAME TO` do not move rows.
- A statement holds `Table.mu` (read lock for `Select`, write lock for the rest). Lock order is `Catalog.mu` → `Database.mu` → `Table.mu` → `catalogStore.mu`.

Facts the design depends on:

- A `RowID` is unique for the life of one file: space is never reclaimed and a slot is never reused. After an `UPDATE` the old id is a tombstone. An `ALTER` rewrite packs rows from page 0 of a new file, so old ids point at other rows there. `File.Get` reports `ErrRowNotFound` only within one file, so the index must be discarded whenever the table's file changes.
- An interrupted `UPDATE` (new versions inserted, old ones not yet tombstoned) can leave two rows with the same key. The index maps a key to one `RowID`.
- Rows written before this change may hold UNIQUE string values longer than the tree's key limit (4084 bytes), because the row-size limit is about 16KiB.

## Decisions made during brainstorming

- **Long keys are rejected.** A PRIMARY KEY or UNIQUE string longer than the index's `MaxKeyLen` makes `INSERT`, `UPDATE` and `ADD COLUMN` fail with a "key too long" error, as MySQL does for over-long index keys. This is one small visible change. The index then covers every row written from now on.
- **Indexes are built lazily, on first use.** The first statement that needs a table's index builds it from one heap scan. Startup stays fast, untouched tables cost no memory, and a discarded index is rebuilt the same way.
- **One index per PRIMARY KEY or UNIQUE column**, keyed by the column's index in the schema. No composite keys and no `CREATE INDEX` syntax.
- **Writes update the index only after the heap write succeeded.** Any failure while writing discards the table's indexes, so the heap stays the single source of truth.
- **A column can be degraded to scanning.** If the build meets data the tree cannot hold (a duplicate key left by an interrupted `UPDATE`, or a value over the key limit written before this change), that column has no tree and every path for it falls back to today's scan code. Correctness never depends on the index.
- **The existing scan code stays.** It is the fallback for non-indexed predicates, degraded columns and a stale index.

## Design

### Index state (`internal/engine/tableindex.go`)

```go
// tableIndexes holds the in-memory indexes of one table.
type tableIndexes struct {
    mu     sync.Mutex          // guards building and resetting; a leaf lock
    fileID int64               // the file the trees were built from
    trees  map[int]*index.Tree // column index -> tree; nil map = not built; nil tree = column degraded
}
```

`SqlTable` gains `idx tableIndexes` and a `noIndexes bool` field that tests set to compare behaviour with and without indexes.

- `(t *SqlTable) indexes() (map[int]*index.Tree, error)`: callers hold `t.mu` (read or write). Under `idx.mu`, if the map is nil or `idx.fileID != t.fileID`, build it: one `Scan` of the file, decoding each row and, for each PRIMARY KEY/UNIQUE column, inserting `EncodeKey(value)` → `RowID` for every non-NULL value. A duplicate key or an over-long key degrades that column (nil tree) and the build continues for the others. A scan or decode error is returned and nothing is cached. If `noIndexes` is set it returns an empty map.
- `(t *SqlTable) dropIndexes()`: under `idx.mu`, forget the trees. Safe from a reader, because tree mutation only happens under the write lock.
- Lock order gains `Table.mu` → `tableIndexes.mu`, a leaf held only while building or resetting.
- Trees are mutated only by statements holding `Table.mu` for writing, and read by statements holding it for reading or writing, so a tree needs no lock of its own.
- Index keys are `index.EncodeKey` of the coerced value (`int64`, `string`, `bool`), the same values the engine already holds. NULL is never indexed, which keeps "a UNIQUE column may hold many NULLs".
- `index` gains `MaxKeyLenFor(nodeBytes int) int`, the formula `New` already uses, so the engine can check a key's length without building a tree. The engine always uses `index.DefaultNodeBytes`.

### Long keys

`INSERT`, `UPDATE` and `ADD COLUMN` check every PRIMARY KEY/UNIQUE value they are about to store. An `UPDATE` that matches no row stores nothing, so it is not checked; `ADD COLUMN` checks its default even on an empty table, because the default is what later rows receive. A string over `MaxKeyLenFor(DefaultNodeBytes)` fails with `key too long for column %q: %d bytes exceeds the maximum of %d`. For `INSERT` the check sits in `checkUniqueness`, in row order and column order, where a duplicate would be reported. Rows already on disk with over-long values keep working; the build degrades their column.

### Uniqueness checks (`checkUniqueness`, `Update`)

- `INSERT`: for each non-NULL value of each unique column, `tree.Get(key)` finds a duplicate in the table, and the existing batch-local `taken` map still finds duplicates within the batch. Error text and order are unchanged. A degraded column uses `existingKeys` as today.
- `UPDATE`: the file scan still finds the matching rows. A conflict on an assigned unique column is `tree.Get(key(assigned))` returning a `RowID` that is not among the matched rows. The rest of the logic (several matched rows receiving one value, which column is reported first) is unchanged.

### Keeping the index in step (under the write lock)

- `INSERT`: `appendToFile` collects the `RowID`s `Insert` returns. After the last `Insert` and the `Sync` succeed, it adds `key → RowID` to each tree. On any error it discards the indexes instead.
- `DELETE`: the scan that collects matching rows also collects their key values. After the tombstones and `Sync` succeed, it deletes those keys from the trees; on any error it discards the indexes.
- `UPDATE`: new versions are inserted at the end, so every updated row gets a new `RowID`. After the writes succeed, for each updated row it deletes the old key of every unique column and then inserts the new key with the new `RowID` (unchanged keys are re-pointed too). All deletes come first, so a row never collides with its own old entry. On any error it discards the indexes.
- `ALTER ADD/DROP COLUMN`: `adoptFile` discards the indexes along with the old file. `RENAME COLUMN` and `RENAME TO` change no `RowID` and no column index, so the indexes stay.
- `Close` and `Drop` discard the indexes with the file.
- A statement that fails validation before writing leaves the indexes as they were.

### Equality access path (second PR)

`where` qualifies when it is a `*ComparisonExpression` with operator `=` whose one side is an `IDENT` naming an indexed (non-degraded) column and whose other side is a literal, or an `AND` chain that contains one such comparison. `OR` and every other operator scan as before. The literal is coerced with `coerceValue` against the column's type; if that fails, or yields NULL, the statement takes the scan path so its error (or empty result) is exactly what it was.

For a qualifying `Select`, `Delete` or `Update`: `tree.Get(key)` → `RowID` → `File.Get` → `DecodeRow`, and then the **whole** `WHERE` is evaluated on that one row, so `AND`ed conditions and column-to-column comparisons keep their meaning. No qualifying row is the same as an empty scan.

`File.Get` returning `ErrRowNotFound` means the index disagrees with the heap. The statement discards the indexes and runs its scan path, so a bug can cost speed but not correctness.

### Memory and documentation

Once a table's index is built, its unique keys stay in memory until the indexes are discarded or the table closes. `CLAUDE.md`'s storage paragraph, which says uniqueness checks stream the file and that memory is proportional to the statement, is updated to say this.

## Error handling

- A build that fails on I/O or decoding returns the error to the statement that asked and caches nothing.
- Over-long keys, duplicates and disagreeing entries never reach the user as index errors: the first is the one new "key too long" error, and the others only degrade a column or force a scan.
- Statements that fail partway still follow the existing no-WAL rules; the only addition is that the indexes are discarded.

## Out of scope (YAGNI)

- Range predicates (`<`, `>`, `BETWEEN`) through `Range`, `OR` of equalities, composite keys, secondary non-unique indexes, `CREATE INDEX`, and `ORDER BY`.
- The on-disk index format, index metadata in the catalog, and the page cache.
- A cost-based planner: an applicable equality on a unique column is always used.

## Testing and verification

Pure Go with `testify`, in `internal/engine` (`go test -race ./...`, required for this package). A test seam: a `storage.File` wrapper that counts `Scan` and `Get` calls (the existing `failingFile` shows the pattern). Every test is seen failing first.

- **Fewer scans:**
  - After the first statement builds the index, a duplicate-key `INSERT` and an equality `SELECT`, `UPDATE` and `DELETE` do no `Scan`.
  - A non-equality `WHERE` still scans.
  - A degraded column still scans.
- **Differential:** a long seeded sequence of `INSERT`, `UPDATE`, `DELETE` and `SELECT` (including NULLs, duplicates, updates that move rows, `AND` and `OR`, reversed operands and mismatched literals) run against two tables, one with `noIndexes`. Results and errors must match step for step.
- **Maintenance:** after each kind of write the index agrees with a fresh build; an injected `Insert`, `Sync` or `Delete` failure discards the indexes and the next statement rebuilds and agrees with the heap.
- **ALTER and lifecycle:** after `ADD` and `DROP COLUMN` the indexes are rebuilt (old `RowID`s are not trusted); `RENAME COLUMN` keeps them; `Close` and `Drop` discard them; a reopened catalog builds lazily and gives the same results.
- **Degraded columns:** a table holding two rows with one key (as an interrupted `UPDATE` leaves) and a table holding a value over the key limit (written through the file directly) both keep working, with identical results to `noIndexes`.
- **Long keys:** `INSERT`, `UPDATE` and `ADD COLUMN` reject an over-long key with the new error and write nothing; a key of exactly the limit is accepted.
- **Disagreement:** an index entry pointing at a deleted slot makes the statement discard the indexes and still return the right answer.
- **Concurrency:** many concurrent `SELECT`s on a never-queried table build the index once and return correct results under `-race`.
- **Mutation checks**, each confirmed to fail a test: skip the index update after `INSERT`; skip re-pointing after `UPDATE`; keep the indexes across `adoptFile`; skip the whole-`WHERE` re-check on the fetched row; trust `ErrRowNotFound` as "no row"; update the index before the heap write succeeds.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order and PR split

1. **PR A: index lifecycle, uniqueness and maintenance.** `MaxKeyLenFor`; `tableIndexes` with lazy build, degrade and discard; the long-key check; `checkUniqueness` and `Update`'s conflict check through the index; index maintenance in `INSERT`, `DELETE`, `UPDATE`, `ALTER`, `Close` and `Drop`; the `CLAUDE.md` update.
2. **PR B: equality access path.** Qualifying-`WHERE` detection and the `Get`-based path for `Select`, `Delete` and `Update`, with the stale-entry fallback.

## Open questions

None blocking.
