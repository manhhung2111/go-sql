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

Primary key, unique, and secondary columns have in-memory B+ tree indexes that serve point equality lookups (`=`) and bounded range lookups (under the page-count rule: fetched rows lie on at most half of the file's pages). Unindexed queries, wide ranges, and non-equality/non-bounded conditions fall back to full-table scans.

## Why it matters

Point and range lookups bypass full-table scans, reducing latency from milliseconds to microseconds on large tables. Scans remain the robust fallback for unindexed queries and range scans spanning most of the table.

## Where it applies

- `Select`, `Update`, `Delete`: use index lookups (`lookupRows`) when the WHERE clause pins or bounds an indexed key column safely.
- Uniqueness checks (`InsertValues`, `Update`): use index lookups instead of streaming the table file.
- Unindexed queries, wide ranges, or complex predicates: stream the file via `storage.File.Scan()`.

## How to follow it when adding code

When adding statement types or query predicates, leverage `planLookup` and `lookupRows` where index access is applicable, while preserving full-table scan fallback for unmatched queries.

## Verified by

Comprehensive differential and unit tests comparing indexed and unindexed table executions statement by statement.
