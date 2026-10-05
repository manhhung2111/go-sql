---
title: Validate everything, then write
kind: concept
sources:
  - internal/engine/table.go
  - pr: 6
  - CLAUDE.md
updated: 09d8ab0
---
# Validate everything, then write

## Rule

Multi-row and multi-step mutations (`InsertValues`, `Update`, `AlterColumns`'s ADD and DROP COLUMN) validate and encode everything before the first write to the table's file, so a bad statement writes nothing.

## Why it matters

There is no write-ahead log. Only an I/O error can leave a statement partly applied (comments on `appendToFile` and `deleteFromFile`). #6 added the check that a batch with one oversized row writes nothing, not even the rows before it.

## Where it applies

- `InsertValues`: resolve the column mapping, build every row, check PRIMARY KEY / UNIQUE, then `appendToFile` writes the whole batch with a single fsync after `encodeRows` has size-checked every row.
- `Update`: encodes and size-checks all new row versions before inserting any.
- `deleteFromFile`: collects matching rows before writing the first tombstone, so an evaluation error touches nothing.
- `addColumn` and `dropColumn`: build the replacement file first, then adopt it (`rebuildAndRecord`, `adoptFile`).
- `ADD COLUMN`'s backfill reuses the checks `InsertValues` uses instead of separate rules; do the same for future ALTER-like operations.

## How to follow it when adding code

Do all coercion, constraint and size checks and build all encoded bytes first; only then write and fsync once per statement. If a step can fail halfway, order the writes so a crash cannot lose data (see [update inserts before tombstoning](../decisions/update-inserts-before-tombstone.md)).

## Verified by

Tests in `internal/engine` that assert a failed statement leaves the table unchanged (mutation checks are listed in #6 and #12). No tool enforces the rule for new statements.
