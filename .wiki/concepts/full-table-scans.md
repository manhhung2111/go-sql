---
title: Point and range lookups via indexes, full-table scans as fallback
kind: concept
sources:
  - internal/engine/table.go
  - internal/engine/lookup.go
  - internal/storage/file.go
  - pr: 12
  - pr: 37
  - pr: 38
  - pr: 39
  - pr: 41
  - pr: 44
updated: 33c2c35
---
# Point and range lookups via indexes, full-table scans as fallback

## Rule

Primary key and unique columns, and columns with a `CREATE INDEX`, have in-memory B+ tree indexes. A WHERE that is an AND chain of column-against-literal comparisons that cannot fail on any row can be served from them: `=` on a PRIMARY KEY/UNIQUE column always is; any bounded range on those columns, and `=` or a range on an indexed column (which may hold duplicates), is served when the rows lie on at most half of the file's pages (the page-count rule). A PRIMARY KEY/UNIQUE column always uses its own tree, never a CREATE INDEX on it. Anything else (OR, two columns, a literal that does not coerce, NULL, `!=`) scans, so WHERE errors are exactly what the scan reports.

## Why it matters

A lookup reads only the pages that hold matching rows. In one measurement (100,000 rows) an equality on a clustered indexed column went from a 10.6 ms scan to 26 µs, while values spread over every page stayed at the scan's cost because the rule refuses them. A lookup also trusts the index: an entry that points at a missing row discards the trees (and any snapshot) and falls back to the scan.

## Where it applies

- `Select`, `Update`, `Delete`: use index lookups (`lookupRows`) when the WHERE clause pins or bounds an indexed key column safely.
- Uniqueness checks (`InsertValues`, `Update`): use index lookups instead of streaming the table file.
- Unindexed queries, wide ranges, or complex predicates: stream the file via `storage.File.Scan()`.

## How to follow it when adding code

When adding statement types or query predicates, leverage `planLookup` and `lookupRows` where index access is applicable, while preserving full-table scan fallback for unmatched queries.

## Verified by

Comprehensive differential and unit tests comparing indexed and unindexed table executions statement by statement.
