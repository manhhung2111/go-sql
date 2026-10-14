# Page-count rule for range lookups: design

Status: draft for review. Replaces the fixed cap of 100 rows in [range lookups](2026-10-13-range-lookup-design.md) (PR #39) with a rule based on how many pages the lookup would read compared with a scan. It builds on [heap `File.Get`](2026-10-12-heap-get-design.md).

## Goal

Decide between a range lookup and a scan by comparing the pages each would read, instead of by a row count that ignores the size of the table. A range that touches few pages is served from the index however many rows it holds; a range spread over most of the file scans. Results, errors and row order stay exactly what the scan gives.

## Context

A range lookup walks the tree, sorts the row ids into file order and calls `File.Get` once per row. Each `Get` reads and checksums a whole 16KiB page, so a lookup costs one page read per row, while a scan costs one read per page of the file. The cap of 100 rows (`maxRangeRows`) is a stand-in for that comparison, and it is wrong in both directions:

- On a table of 10,000 pages, a range of 500 rows packed onto 5 pages is refused although it costs 500 reads against 10,000, and would cost 5 if rows on one page shared a read.
- On a table of 150 pages, 100 rows spread over 100 pages is served although a scan reads 150 pages in sequence and the lookup does 100 scattered reads.

What is known, and when:

- **The tree knows the exact set of row ids in a range**, because it holds one entry per row. The ids carry their page number, so the number of distinct pages the lookup would read, `D`, is known once the ids are collected, without touching the file.
- **The scan's cost is the file's page count, `P`.** `File` does not report it today; `PageFile` has `NumPages()` and `sqlFile` already uses it in `Scan`, `Get` and `Delete`.
- **The ids are not stored in page order.** The tree is ordered by key; rows sit in insert order, and an UPDATE moves a row to the end of the file. After the walk the ids are sorted by (page, slot), which is also the order the scan returns rows in, so ids on one page are adjacent.

## Decisions made during design

- **Compare distinct pages with the file's pages.** A range lookup is used while `factor × D ≤ P`, with `factor = 2`. The factor is above 1 because a lookup's reads are scattered while a scan's are sequential and the scan also skips the per-row work of the lookup; 2 is a guess that is safe (the lookup never reads more than half the pages a scan would) and is one variable to change once measured.
- **Rows on one page share one page read.** `File` gains `GetMany`, which takes row ids and reads each run of ids on the same page once. Without it `D` would be the right measure of the lookup's cost only on paper.
- **Equality lookups are unchanged.** One row means one page read, never more than a scan; they stay unconditional.
- **The walk stops as soon as the answer is no.** While collecting ids the engine keeps the set of distinct pages seen and abandons the range when `factor × len(set) > P`. A range that is refused costs at most about `(P / factor) × rows-per-page` tree steps, since the walk meets that many rows before it has seen `P / factor` distinct pages when the keys are clustered, and fewer when they are spread. A range that is accepted is walked in full; that is bounded by the index, which is already in memory. All its ids, and the encoded rows `GetMany` returns for them, are held in memory together, however many of them the rest of the WHERE then discards (a scan streams a page at a time); reading the runs of pages in batches is a possible later change.
- **The 100-row cap goes away.** `maxRangeRows` is deleted; its job is done by the page rule. A range of 1,000 consecutive keys on 10 pages is now a lookup.
- **A test seam, not a setting.** The factor is a package variable, as `maxRangeRows` was, so a test can set it to 0 (always a lookup) and make small tables exercise the lookup path. A table of a few pages otherwise always scans.
- **Everything else in the range design stands:** the safety rule (a WHERE that cannot fail), combined bounds, the whole-WHERE re-check on each fetched row, file order, the stale-entry fallback, degraded columns, and `UPDATE`'s `needTrees` rule.

## Design

### Storage (`internal/storage/file.go`)

```go
type File interface {
    // ...existing methods...

    // NumPages returns the number of pages in the file.
    NumPages() int32

    // GetMany returns a copy of the row at each id, in the order of ids. Each
    // run of consecutive ids on one page reads that page once, so ids sorted
    // by page read each page once. It fails with an error wrapping
    // ErrRowNotFound if any id has no live row, and returns no rows then. Like
    // Get, it may run concurrently with other Gets and Scans.
    GetMany(ids []RowID) ([][]byte, error)
}
```

- `NumPages` returns `f.pages.NumPages()`. A snapshot is enough: the table's lock keeps writers out while a statement runs.
- `GetMany` shares its page handling with `Get`: the page range check, `ReadPage`, `validateLayout`, the slot and tombstone checks, `bytes.Clone`. `Get(id)` becomes a call of the same helper for one id, so the two cannot drift. A run is a maximal stretch of ids with equal `Page`; ids not sorted by page still give correct rows, only with more reads. An empty `ids` returns an empty result and reads nothing.
- Error wording and the `ErrRowNotFound` / damage split follow `Get`: a stale address wraps `ErrRowNotFound`, a bad checksum or layout does not.
- The engine's test wrappers (`failingFile`, `countingFile`) embed `File`, so they compile unchanged; `countingFile` additionally counts `GetMany` calls and pages read so tests can assert reads.

### Engine (`internal/engine/lookup.go`)

```go
// rangeCostFactor is how many pages a scan may read per page a range lookup
// reads before the lookup stops being worth it. A lookup's reads are scattered
// and a scan's are sequential. It is a variable so a test can set it to 0,
// which makes every range a lookup.
var rangeCostFactor = 2
```

`rangeRows` becomes:

1. `pages := int(t.file.NumPages())`.
2. Walk `tree.Range(lo, hi)`. Add each id to `ids` and its page to a set. If `rangeCostFactor * len(set) > pages`, return not usable. (With factor 0 the test never fires.)
3. Sort `ids` by (page, slot), as today.
4. `rowsBytes, err := t.file.GetMany(ids)`. `ErrRowNotFound` drops the indexes and reports not usable, as for a stale `Get`; another error is returned.
5. For each id in order: `DecodeRow`, evaluate the whole `WHERE`, keep the row if it matches.

`fetchMatching` keeps serving the equality path, with its `Get`. The decode-and-evaluate part is shared between the two so a range row and a point row go through the same code.

The decision is made before any file read, so a refused range costs tree steps and one `NumPages` call, never a page read. The empty file (`P = 0`) refuses every non-empty range at the first id, and an empty range has no ids, so it is a lookup that finds nothing, as the scan would.

### Documentation

`CLAUDE.md`'s storage paragraph says a bounded range is a lookup "when it holds at most 100 rows". It now says a bounded range is a lookup when its rows lie on at most half the file's pages (rows on one page are read once), and otherwise scans. The comment on `Select` and the range spec's decision about the cap are updated to point here.

## Error handling

- Everything the scan reports is still reported by the scan, because a range lookup is used only for a `WHERE` that cannot fail.
- A refused range, a stale entry, a degraded column or an unbounded comparison simply scans.
- `GetMany` returns either all rows or an error; the engine never sees a partial result.
- A `GetMany` error other than `ErrRowNotFound` (damage, I/O) is returned, as for `Get`.
- The page count is read once per statement under the table's lock. A concurrent writer cannot change it, because writers hold the write lock.

## Out of scope (YAGNI)

- Weighing random against sequential reads with measured constants, or any statistics beyond the exact set the tree gives.
- A page cache, so that repeated lookups share reads between statements.
- Batching equality lookups (there is only ever one).
- Making the walk itself cheaper (counts in tree nodes) or bounding it for a range that is accepted.
- `ORDER BY`, `LIMIT`, `OR`, `IN`, non-unique indexes.

## Testing and verification

Pure Go with `testify`, in `internal/storage` and `internal/engine`, with `go test -race ./...`. Every test is seen failing first.

**Storage:**

- `GetMany` returns the same bytes as `Get` for each id, across several pages, on the tail page, and after reopening the file.
- Order of the result follows the order of `ids`, including ids not sorted by page.
- Page reads: a counting `PageFile` shows ids on one page read it once, ids on three pages read three, and the same page appearing in two separate runs reads twice.
- Not found: a deleted row, a slot out of range, a page out of range and a negative page each give an error wrapping `ErrRowNotFound` and no rows, wherever in the list the bad id sits (first, middle, last).
- Corruption is not "not found": a flipped byte and a stretched slot length give errors that do not satisfy `ErrRowNotFound`.
- Empty `ids` reads nothing and returns an empty, non-nil result. Ownership: modifying a returned slice does not change the next read.
- `NumPages` grows with inserts and matches `Scan`'s page count; it is 0 for a new file.
- Concurrency: many `GetMany`, `Get` and `Scan` at once pass under `-race`.

**Engine:**

- **The rule itself:** on a table of known page count, a range whose rows lie on `D` pages is a lookup when `2D ≤ P` and a scan when `2D > P`, tested at the boundary on both sides. The table is built with wide rows so a handful of rows fill a page.
- **More rows than the old cap:** a range of several hundred rows on a few pages is a lookup (no `Scan`, a number of page reads equal to `D`).
- **Spread rows scan:** a range of few rows that sit on many pages, built by updating rows so they move to the end, or by inserting keys out of order, scans although the old cap would have served it.
- **Early exit:** a refused range makes no `GetMany` call, and the walk stops once it has seen more than `P / 2` distinct pages.
- **Same answers, errors and order as the scan:** the existing parity, whole-WHERE, file-order, multi-page-order, empty-range and stale-entry tests keep passing; the stale-entry test now also covers an entry found by `GetMany`.
- **Differential:** the seeded differential test runs with `rangeCostFactor = 0` (small tables exercise the lookup path) and again with the default factor and wide rows (both outcomes occur), comparing results, errors and the file after every statement against an unindexed table.
- **Mutation checks**, each confirmed to fail a test: compare `D` with `P` without the factor; count rows instead of distinct pages; skip the early exit; read with `Get` per row instead of `GetMany`; skip the sort before `GetMany`; treat `ErrRowNotFound` from `GetMany` as "no rows"; keep the old row cap beside the new rule.
- A one-off measurement on a 100,000-row table (not committed), quoted in the PR: a 1,000-row range over clustered keys (formerly over the cap) and a spread range, before and after.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. `File.NumPages` and `GetMany` with their storage tests (`Get` rebuilt on the shared helper).
2. The engine rule in `rangeRows`, replacing `maxRangeRows`, with the rule, early-exit and spread tests; existing cap tests are rewritten against the new variable.
3. The differential extension, `CLAUDE.md`, the mutation checks and the measurement.

## Open questions

- The factor of 2 is a guess. The measurement in the PR gives a first data point (the page ratio at which a scan beats the lookup on a 100,000-row table) and may change it.
