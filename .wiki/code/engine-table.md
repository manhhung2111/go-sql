---
title: internal/engine/table.go
kind: code
sources:
  - internal/engine/table.go
  - pr: 12
updated: 09d8ab0
---
# internal/engine/table.go

This is the largest file in the repo (about 900 lines), so it has its own page. Package-level context is in [internal/engine](engine.md).

## Purpose

`SqlTable` is one table: its name, `Columns`, the heap `file` (the only copy of its rows), the file's `fileID`, and the `catalogStore` it records schema changes in. Every statement reads and writes the file under the table's `mu`.

## Files

Single file. Its main parts:

| Part | Responsibility |
| --- | --- |
| `NewTable`, `validateNewColumn` | Validate the schema (no duplicate columns, at most one PRIMARY KEY, DEFAULT coerces against its own type) before anything is created on disk |
| `createTableFile`, `tableFile` | Create a new, empty file in the database's directory, exclusively |
| `InsertValues`, `buildRow`, `checkUniqueness`, `existingKeys`, `appendToFile` | INSERT in four phases: resolve mapping, build rows, check keys, write the whole batch with one fsync |
| `Select` | Stream the file a page at a time, filter with `evalWhere`, project columns |
| `Update`, `Delete`, `deleteFromFile` | Mutations; see [update inserts before tombstoning](../decisions/update-inserts-before-tombstone.md) |
| `AlterColumns`, `addColumn`, `dropColumn`, `renameColumn` | ADD and DROP COLUMN rewrite the file via `rebuildFile` / `rebuildAndRecord` / `adoptFile`; RENAME COLUMN only changes metadata |
| `Rename`, `Close`, `Drop` | Rename records the new name in the catalog first; `Close` and `Drop` are idempotent |

## Key types and entry points

`Table` interface methods: `InsertValues`, `Select`, `Update`, `Delete`, `AlterColumns`, `Rename`, `Close`, `Drop`. Callers reach a table only through `Database.GetTable`.

## Gotchas

- Header comment of `NewTable`: no multi-column primary key; at most one PRIMARY KEY column; columns with no constraint are nullable.
- `PRIMARY KEY` / `UNIQUE` checks stream the file once against a small set of the statement's own key values, so memory grows with the statement and time with the table. NULLs never conflict.
- Row order from `Select` is file order: insertion order, except that an updated row has moved to the end.
- `ADD COLUMN` of a PRIMARY KEY or UNIQUE column with a default on two or more existing rows is rejected before any file work, using the same checks INSERT uses.
- Insert errors are held back so a duplicate key in an earlier row is reported before a later row's error (preserved ordering from the pre-file implementation; #12).
- A row whose encoding exceeds `storage.MaxRowSize` is rejected as `row too large`.

## Related

[Validate then write](../concepts/validate-then-write.md), [rows only live in the file](../decisions/rows-only-in-file.md), [lock order](../concepts/lock-order.md), [storage](storage.md).
