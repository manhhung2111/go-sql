# Range lookups: design

Status: draft for review. Extends [equality lookups (part B)](2026-10-13-engine-equality-lookup-design.md), which is PR #38; it needs that code.

## Goal

Serve `SELECT`, `UPDATE` and `DELETE` whose `WHERE` bounds a PRIMARY KEY or UNIQUE column with `<`, `<=`, `>` or `>=` from the index when the range is small: walk the tree between the bounds, fetch just those rows, and skip the scan. Results, errors and even row order stay exactly what the scan gives.

## Context

Part B turns `col = literal` into one tree lookup and one `File.Get`. Everything else still scans, including `WHERE id >= 100 AND id < 110`. The tree already has what this needs: `Tree.Range(lo, hi)` yields keys in `[lo, hi)` in key order, and the key encoding is order-preserving (integers by value, strings byte-wise), which is the order the scan's comparisons use.

Three facts shape the design:

- **The scan returns rows in file order, not key order.** Rows come back by page and slot, so after `UPDATE`s an updated row has moved to the end. A tree walk yields key order. To keep results identical the fetched rows must be put back in file order, which is `RowID` order (page, then slot).
- **A fetch is a page read per row; a scan reads each page once.** A range that matches most of the table is faster to scan. There are no statistics, and `File` does not expose the page count, so the lookup needs a cheap rule for giving up.
- **The parser has no `BETWEEN`, `ORDER BY` or `LIMIT`.** A range arises only from the four comparison operators, usually as a pair of conjuncts on one column.

## Decisions made during design

- **Same safety rule as part B.** A range lookup applies only to a `WHERE` that cannot fail at run time: an AND chain of column-against-literal comparisons with supported operators, known columns, literals that coerce, and `=`/`!=` only on boolean columns. A boolean key column is never range-planned, because the scan errors for an ordering operator on a boolean.
- **Bounds on one column are combined.** Every `>`/`>=` conjunct on a key column raises its lower bound and every `<`/`<=` lowers its upper bound; `id >= 3 AND id < 10` is one range. The tightest bounds win. An empty range (lower above upper) is a lookup that finds nothing, as the scan would.
- **Bounds become a half-open key range with one rule.** With `K` the encoded literal:

  | Predicate | Lower bound `lo` | Upper bound `hi` |
  | --- | --- | --- |
  | `col >= v` | `K` | none |
  | `col > v` | `K` followed by a zero byte | none |
  | `col < v` | none | `K` |
  | `col <= v` | none | `K` followed by a zero byte |

  `K‖0x00` is the first key after `K` in byte order, for fixed-width integers (no 8-byte key lies between `K` and `K‖0x00`) and for strings alike. It can be one byte over the tree's key limit; `Range` does not check lengths, only `Insert` does.
- **An equality on a key column still wins.** If the `WHERE` pins any key column with `=`, part B's point lookup is used and the ranges are only re-checked as part of the whole `WHERE`.
- **NULLs need no handling.** NULLs are never indexed, and the scan never matches a NULL under a comparison, so the tree's range is already right.
- **Rows are fetched in file order.** The `RowID`s from the walk are sorted by (page, slot), then fetched and decoded one by one, and the whole `WHERE` is evaluated on each, exactly as for a point lookup. The result and the order of `UPDATE`'s new versions are therefore the same as the scan's.
- **Give up on big ranges.** The walk collects at most `maxRangeRows` row ids (a package variable, 100). If it meets one more, the lookup reports not usable and the statement scans. A walk that gives up costs at most that many tree steps. A hundred page reads is a fraction of a scan of any table large enough to matter; tuning the number against a benchmark is future work and a one-line change.
- **A degraded column (no tree) is never range-planned**, as with equality. `UPDATE` keeps part B's rule that every assigned unique column needs a tree.
- **No maintenance changes.** Range lookups only read. Writes keep the indexes exactly as before.

## Design

### Planning (`internal/engine/lookup.go`)

`planLookup` keeps returning the equality plans. It also returns range plans:

```go
// rangePlan is a half-open range of encoded keys on one key column. A nil lo
// or hi is unbounded.
type rangePlan struct {
    column int
    lo, hi []byte
}

type lookupPlans struct {
    equalities []lookupPlan
    ranges     []rangePlan
}
```

Walking the AND chain, every comparison is validated as in part B. A comparison on a key column with `>`, `>=`, `<` or `<=` tightens that column's range: the new lower bound replaces the old if it is greater by `bytes.Compare`, the new upper bound if it is smaller (an unbounded side is greater/smaller than anything). A comparison on a non-key column, or `!=` on any column, constrains nothing but is still validated. The result has one `rangePlan` per key column that has at least one bound, in order of first appearance.

### Fetching (`lookupRows`)

`lookupRows` keeps its signature and result type. After the equality plans (unchanged), it tries range plans in order, using the first whose column has a tree:

1. Walk `trees[column].Range(lo, hi)`, collecting row ids. If more than `maxRangeRows` arrive, report not usable (the statement scans). Keys that arrive are not needed; only the ids.
2. Sort the ids by (page, slot).
3. For each id in order: `File.Get`, `DecodeRow`, evaluate the whole `WHERE`, keep the row if it matches. `ErrRowNotFound` discards the indexes and reports not usable, as for a point lookup; another error is returned.
4. Return the matching rows in file order.

`Select`, `deleteFromFile` and `Update` already loop over the rows `lookupRows` returns, so they need no change. For `UPDATE`, part A's rule that two matched rows cannot receive one unique value now applies to lookup results too, as it does to scan results.

### Documentation

`CLAUDE.md`'s storage paragraph says a WHERE that pins a key column with `=` is a lookup. It gains: a bounded range on a key column is also a lookup when it holds at most 100 rows, with the rows fetched in file order; a bigger range scans.

## Error handling

- Everything the scan reports is still reported by the scan, because a range lookup is used only for a `WHERE` that cannot fail.
- A range with too many rows, an empty tree column, a stale entry or an unbounded comparison simply scans.
- A lookup trusts that the index has every key whose row exists. A range widens the effect of a missing entry from one key to every key in the range: a range `DELETE` would skip a row whose entry is missing. Write maintenance and the differential test guard against that, as for equality lookups.
- Only the first range on a column that has a tree is used. If it holds more rows than the cap, the statement scans even when a later range in the same `WHERE` would be small (`id > 0 AND email < 'b'`). That is a performance choice, not a correctness one.
- A `Get` error other than `ErrRowNotFound` is returned.

## Out of scope (YAGNI)

- `OR` of ranges, `IN`, `!=`, and any use of an index for something other than finding rows (`ORDER BY` and `LIMIT` do not exist yet).
- A range on a column with no tree, a boolean column, or a column that is not a key. Non-unique indexes would change that and are a later phase.
- Statistics, a cost model, or fetching several rows of one page with a single read. Both would improve the give-up rule; neither is needed to be correct.
- The on-disk index format.

## Testing and verification

Pure Go with `testify`, in `internal/engine`, with `go test -race ./...`. Every test is seen failing first. The `countingFile` from part A counts scans and gets.

- **Planner:** a table of `WHERE`s to expected ranges: each of the four operators, both operand orders, a pair of bounds on one column, several bounds that tighten each other, an empty range, a bound with a string and with a negative integer, a range on a unique column, a range beside an equality, a range beside a non-key condition, and the shapes that must give no plan (a boolean key column, `!=`, `OR`, an unknown column, a literal that does not coerce, a non-key column).
- **Bounds are exact:** with keys 1 to 20 (and a table of strings), `>` excludes the literal and `>=` includes it, `<` and `<=` likewise, and a literal that is not a key falls between keys correctly. The key just above and below the boundaries are present, so an off-by-one fails.
- **Fewer reads:** once the indexes are built, `SELECT`, `UPDATE` and `DELETE` over a small range do no `Scan` and exactly one `Get` per row in the range; an empty range does none; a range of exactly 100 rows is a lookup and one of 101 scans (a table of 150 rows).
- **Same answers, errors and order as the scan:** after `UPDATE`s that move rows to the end, a range `SELECT` returns rows in the same order as the scan; an `UPDATE` over a range produces the same file as the scan; a `WHERE` that could fail gives the same error, with the range both present and absent.
- **Whole-`WHERE` re-check:** `id >= 3 AND id < 10 AND name = 'x'` returns only the rows named `x`.
- **Differential:** the existing differential test's `WHERE` generator gains range shapes (a single bound, a pair, a range beside an equality, a range beside a non-key condition, an empty range). It compares results, errors and the file after every statement against an unindexed table. Its tables are small, so one extra test lowers `maxRangeRows` to exercise the give-up path under the same comparison.
- **Mutation checks**, each confirmed to fail a test: `>` treated as `>=`; `<=` treated as `<`; skip the sort into file order; skip the cap; skip the whole-`WHERE` re-check on range rows; plan a range on a boolean column; take the looser of two lower bounds; use a range on a column with no tree.
- A one-off measurement on a large table (not committed), quoted in the PR: a range `SELECT` of about ten rows before and after, and one over the cap.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. Range planning (pure) and its tests.
2. The tree walk, sort and fetch in `lookupRows`, with `SELECT`; then `DELETE` and `UPDATE`.
3. The differential extension, the give-up test, `CLAUDE.md`, the mutation checks and the measurement.

## Open questions

- The cap of 100 is a guess that is safe and easy to change. The measurement in the PR gives a first data point (where a range lookup stops beating a scan on a 100,000-row table), and a later change can tune it or replace it with a page-count rule once `File` can report its size.
