---
title: Full-table scans until an index exists
kind: concept
sources:
  - internal/engine/table.go
  - internal/storage/file.go
  - pr: 12
updated: 09d8ab0
---
# Full-table scans until an index exists

## Rule

There are no indexes. Every statement that has to find rows reads the whole table file a page at a time.

## Why it matters

Memory stays small (it grows with the statement's own result or key set, not the table), but time grows with table size. The space of tombstoned rows is also not reclaimed, so scans read dead slots too (#12 lists both as known costs).

## Where it applies

- `Select`: streams the file under the table's read lock, filters with `evalWhere`, and keeps only matching, projected rows.
- `Update` and `Delete`: scan to find matching rows.
- PRIMARY KEY / UNIQUE checks on `InsertValues`, `Update` and `ADD COLUMN`: `existingKeys` streams the file once against a small set of the statement's own candidate key values.
- `ADD COLUMN` / `DROP COLUMN`: stream the old file into a new one.

## How to follow it when adding code

Use `storage.File.Scan()` and decode one row at a time; do not load a whole table into memory. If an index is added, the uniqueness checks and `WHERE pk = ?` lookups are the first users. UPDATE moves rows to the end of the file, so an index would have to track new row ids.

## Verified by

Nothing measures scan cost. The engine tests assert behaviour, not complexity.
