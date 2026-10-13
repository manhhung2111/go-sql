# Engine equality lookup (part B): design

Status: draft for review. Part B of [engine index wiring](2026-10-12-engine-index-wiring-design.md); it builds on part A (lazily built indexes, kept in step with every write, used for uniqueness checks), which is PR #37.

## Goal

Serve `SELECT`, `UPDATE` and `DELETE` statements whose `WHERE` pins a PRIMARY KEY or UNIQUE column to a literal from the index: look the key up, fetch that one row with `File.Get`, and skip the scan. Results, errors and error order stay exactly what the scan gives. This is the part that makes a point lookup fast.

## Context

Part A leaves every `SELECT`, `UPDATE` and `DELETE` scanning the whole file to find its rows. The wiring spec sketches this part: `col = literal`, or an `AND` chain containing one, goes `tree.Get → File.Get → decode → re-evaluate the whole WHERE`, and a literal that fails to coerce falls back to the scan.

Reading `evalWhere`, `evalComparison` and `compareValues` shows that sketch is not enough to keep results identical. The scan evaluates the `WHERE` on every row, so an error in any part of it surfaces as soon as the table has one row, whatever the key is:

- `id = 5 AND nosuch = 1` is an "unknown column" error on a non-empty table, but a lookup of the absent key `5` would evaluate nothing and return an empty result.
- `id = 5 AND name = 3` is a coercion error in the same way.
- `flag > true` on a boolean column errors for every row with a non-NULL flag.
- Comparing two columns of different types errors for rows where both are non-NULL.

So the lookup may only be used when the whole `WHERE` is known not to be able to fail.

## Decisions made during design

- **A lookup is used only for a `WHERE` that cannot fail at run time.** That is an `AND` chain, nested any way, of comparisons that each have exactly one column and one literal, where the column exists, the literal coerces to the column's type, and a boolean column is compared with `=` or `!=` only. Under those rules no comparison can return an error, whatever the row, so skipping the other rows cannot hide one. Anything else (`OR`, column against column, a literal against a literal, an unknown column, a literal that does not coerce, an ordering operator on a boolean, `NULL`) scans exactly as before.
- **The indexed comparison is `=` on a PRIMARY KEY/UNIQUE column that has a tree.** The first such conjunct, left to right, supplies the key. The whole `WHERE` is then evaluated on the one fetched row, so the other conjuncts keep their meaning. A column with no tree (degraded, or indexes off) is not indexed, and neither is any other column.
- **A first use builds the indexes, as in part A.** A point statement on a table whose indexes are not built pays for one build scan, and every later one is a lookup.
- **`UPDATE` uses a lookup only if every unique column it assigns has a tree.** Its conflict check for a column without a tree needs the scan of the other rows, which a lookup would skip.
- **A disagreeing index costs speed, not correctness.** If `File.Get` returns `ErrRowNotFound`, the index points at a row that is not there. The statement discards the indexes and runs its scan path. Any other `Get` error (damage, I/O) is returned.
- **Only one row can match.** A tree maps a key to one `RowID`, so a lookup yields at most one candidate. The key is unique in the table (a column holding duplicates has no tree).

## Design

### Recognising a lookup (`internal/engine/lookup.go`)

```go
// lookupPlan names the key a WHERE pins.
type lookupPlan struct {
    column int  // schema index of the PRIMARY KEY/UNIQUE column
    value  any  // the literal, coerced to the column's type; never nil
}

// planLookup reports whether where can be served from an index.
func (t *SqlTable) planLookup(where parser.Expression, columnIndex map[string]int, trees map[int]*index.Tree) (lookupPlan, bool)
```

It flattens the `AND` chain (any `OR`, or any node that is not a `*parser.ComparisonExpression` or an `AND` `*parser.BinaryExpression`, ends the attempt), checks every comparison against the rules above, and returns the first `=` conjunct on a key column whose `trees[column]` is non-nil and whose coerced value is non-nil. A nil `where` has no plan.

### Fetching (`lookupRows`)

```go
type fetchedRow struct {
    id  storage.RowID
    row []any
}

// lookupRows serves where from an index when it can. usable is false when the
// statement must scan instead; rows holds the one row that satisfies where, or
// none. Callers hold t.mu.
func (t *SqlTable) lookupRows(where parser.Expression, columnIndex map[string]int, needTrees []int) (rows []fetchedRow, usable bool, err error)
```

1. If `where` is nil, return not usable (no indexes touched).
2. `trees, err := t.indexes()` (builds if needed). Return not usable if `noIndexes` gave no trees.
3. If any column in `needTrees` has a nil tree, return not usable.
4. `planLookup`; if none, not usable.
5. `key := index.EncodeKey(plan.value)`, `id, found := trees[plan.column].Get(key)`; if not found, usable with no rows.
6. `File.Get(id)`: on `ErrRowNotFound`, `dropIndexes()` and return not usable; on another error, return it.
7. `DecodeRow`, then `evalWhere` on the whole `where`; return the row if it matches, none if not.

### Statements

- **`Select`:** after resolving the output columns and checking the table is open, call `lookupRows(where, columnIndex, nil)`. If usable, project the (at most one) row exactly as the scan loop does, factoring the projection out so both paths share it. Otherwise run the scan as today.
- **`Delete`:** `deleteFromFile` calls `lookupRows`; if usable, the matching `RowID` and key values come from the fetched row, and tombstoning, `Sync`, `indexDeleted` and error handling are unchanged.
- **`Update`:** after resolving the assignments, call `lookupRows(where, columnIndex, uniqueColumns)`. If usable, `oldIDs`, `oldKeys` and `updated` come from the fetched row instead of the scan. The conflict check against the trees, the "several matched rows" rule, the writes and `indexMoved` are unchanged. If not usable, the scan runs as today.

All three keep their lock: `Select` the read lock, the others the write lock. A reader that calls `lookupRows` may build or discard the indexes; part A made both safe under the read lock.

### Documentation

`CLAUDE.md`'s storage paragraph gains the sentence that an equality `WHERE` on a PRIMARY KEY/UNIQUE column is a lookup, and that anything the scan could reject still scans.

## Error handling

- Anything the scan would report is still reported by the scan: the lookup never runs for a `WHERE` that could fail.
- An `ErrRowNotFound` from `File.Get` discards the indexes and falls back to the scan, so a bug in the index costs speed. Damage and I/O errors are returned.
- A lookup that finds no key, or a row that fails the full `WHERE`, returns an empty result, which is what the scan would return.

## Out of scope (YAGNI)

- Range predicates (`<`, `>`, `BETWEEN`) through `Range`, `IN`, `OR` of equalities, and `ORDER BY`.
- Non-unique and composite keys, `CREATE INDEX`, and a cost-based planner.
- The on-disk index format and the page cache.
- Making the `WHERE` check static for more shapes (column against column, say). They scan.

## Testing and verification

Pure Go with `testify`, in `internal/engine`, with `go test -race ./...`. Every test is seen failing first. The part A `countingFile` counts `Scan` and `Get`.

- **Fewer reads:** once the indexes are built, `SELECT`, `UPDATE` and `DELETE` with `WHERE id = k` do no `Scan` and exactly one `Get`; `email = e` works the same; `5 = id` (reversed) and `id = 5 AND name = 'x'` too; a key that is absent does no `Get`.
- **Still scans:** `OR`; `id >= 3`; a non-key column; an ordering operator on a boolean column (`flag > true`); two columns compared; no `WHERE`; a column with no tree; `noIndexes`.
- **Same answers and errors as the scan:** a table of `WHERE`s that could fail (`id = 5 AND nosuch = 1`, `id = 5 AND name = 3`, a string literal against an INT key, a boolean ordering comparison, `NULL`), run on a non-empty table with a present and an absent key, must give the same result or error as a table with `noIndexes`.
- **Full `WHERE` re-check:** `id = 3 AND name = 'wrong'` returns no row for an existing key 3, and `UPDATE`/`DELETE` leave it alone.
- **`UPDATE` and `DELETE`:** after each, the indexes agree with a fresh build (`assertIndexesMatchHeap`); an `UPDATE` that assigns a unique column conflicts exactly as before; an `UPDATE` assigning a column with no tree scans.
- **Stale entry:** an index entry whose row was tombstoned behind its back makes the statement return the right answer by scanning, discards the indexes, and the next statement rebuilds them.
- **Differential:** the part A differential test already runs the same random statements against an indexed and an unindexed table; its `WHERE` generator gains the shapes above (key equalities joined by `AND`, a literal against a boolean, `NULL`), so the lookup path is compared step by step.
- **Concurrency:** many concurrent `SELECT ... WHERE id = k` on a table whose indexes are not built build them once and return correct rows under `-race`.
- **Mutation checks**, each confirmed to fail a test: skip the whole-`WHERE` re-check; treat `ErrRowNotFound` as "no row"; accept `OR`; accept a boolean ordering comparison; use a lookup for a column with no tree; skip the `needTrees` check in `Update`; return the lookup row without evaluating `WHERE` in `Delete`.
- A one-off measurement on a large table (not committed) is quoted in the PR: point `SELECT`, `UPDATE` and `DELETE` before and after.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. `planLookup` and its tests (pure, no I/O).
2. `lookupRows`, then `Select` through it.
3. `Delete`, then `Update`.
4. The differential and concurrency tests, `CLAUDE.md`, and the measurement.

## Open questions

None blocking.
