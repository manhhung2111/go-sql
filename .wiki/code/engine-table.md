---
title: internal/engine/table.go
kind: code
sources:
  - internal/engine/table.go
  - internal/engine/tableindex.go
  - docs/specs/2026-10-12-engine-index-wiring-design.md
  - docs/specs/2026-10-13-engine-equality-lookup-design.md
  - docs/specs/2026-10-13-range-lookup-design.md
  - docs/specs/2026-10-14-page-count-range-rule-design.md
  - docs/specs/2026-10-15-on-disk-index-design.md
  - pr: 12
  - pr: 37
  - pr: 38
  - pr: 39
  - pr: 41
  - pr: 44
  - pr: 45
updated: 33c2c35
---
# internal/engine/table.go

Package-level context is in [internal/engine](engine.md).

## Purpose

`SqlTable` is one table: its name, `Columns`, heap `file`, `fileID`, catalog store, and `tableIndexes` (unique and secondary index trees). Every statement reads and writes the file and indexes under the table's `mu` and `tableIndexes.mu`.

## Files

Main parts across `table.go` and `tableindex.go`:

| Part | Responsibility |
| --- | --- |
| `NewTable`, `validateNewColumn` | Validate schema before creation on disk |
| `createTableFile`, `tableFile` | Create a new, empty file exclusively |
| `InsertValues`, `checkUniqueness`, `appendToFile` | INSERT: validates, checks uniqueness via index lookup (or scan fallback), writes batch |
| `Select`, `lookupRows`, `planLookup` | SELECT: serves point `=` and bounded range WHEREs through index trees (under page-count rule), or falls back to scan |
| `Update`, `Delete`, `deleteFromFile` | Mutations: updates/tombstones rows and maintains index trees |
| `CreateIndex`, `DropIndex` | In `indexdef.go`: record or remove a definition (catalog row first), discard the trees and any snapshot |
| `Close`, `Drop` | Close saves the built trees to `<fileID>.idx` (`saveSnapshot`) before closing the heap; Drop removes the heap file and its snapshot files |
| `prepareHeapWrite` | In `snapshot.go`: called by `appendToFile`, `deleteFromFile` and `Update` after validation and before the first heap write; removes the snapshot durably, once per open |

## Key types and entry points

`Table` interface methods: `InsertValues`, `Select`, `Update`, `Delete`, `AlterColumns`, `Rename`, `Close`, `Drop`, `CreateIndex`, `DropIndex`.

## Gotchas

- Primary key, unique, and secondary index trees are built lazily from one heap scan on first use (or loaded from `<fileID>.idx` snapshot) and maintained on every write.
- A write error or DDL change discards the index trees. After a heap write or fsync failure, or an entry that points at a missing row, the table also stops loading and saving snapshots for the rest of the run (`distrustIndexes`).
- `UPDATE` re-points the index entry of every updated row, even when its indexed value is unchanged, because each updated row gets a new `RowID` and a secondary key ends in it.
- A table created in this run or given a new file by ALTER has no snapshot to remove (`snapshotMayExist` is true only for tables loaded from the catalog).
- Range lookups use the page-count rule (`rangeCostFactor` = 2).

## Related

[Validate then write](../concepts/validate-then-write.md), [rows only live in the file](../decisions/rows-only-in-file.md), [lock order](../concepts/lock-order.md), [storage](storage.md), [index](index.md).
