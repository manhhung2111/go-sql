---
title: internal/engine
kind: code
sources:
  - internal/engine/
  - docs/specs/2026-10-12-engine-index-wiring-design.md
  - docs/specs/2026-10-13-engine-equality-lookup-design.md
  - docs/specs/2026-10-13-range-lookup-design.md
  - docs/specs/2026-10-14-page-count-range-rule-design.md
  - docs/specs/2026-10-15-create-index-design.md
  - docs/specs/2026-10-15-on-disk-index-design.md
  - pr: 37
  - pr: 38
  - pr: 39
  - pr: 41
  - pr: 43
  - pr: 44
  - pr: 45
updated: 33c2c35
---
# internal/engine

## Purpose

Executes parsed statements. The types mirror the SQL hierarchy: `Catalog` (databases) holds `Database` (tables) holds `Table` (schema plus its heap file). Table rows live only in the table's file, and the catalog and index definitions are stored in system files.

## Depends on / Must not import

Imports `internal/parser`, `internal/storage`, and `internal/index`. It must not import `proto/sqlpb`; no file in `internal/engine` does. Converting rows to protobuf messages is `internal/grpcserver`'s job.

## Files

| File | Responsibility |
| --- | --- |
| `engine.go` | `Engine` interface, `NewEngine`, `Execute`: type-switch over statement types; `CreateIndexStatement` and `DropIndexStatement` call `Table.CreateIndex` / `DropIndex` |
| `catalog.go` | `Catalog` interface, `SqlCatalog`, `NewCatalog` (open, load, remove orphans), create/drop database |
| `database.go` | `Database` interface, `SqlDatabase`: create, rename, drop table (a drop also removes the table's index rows from `sys_indexes`) |
| `table.go` | `Table` interface, `SqlTable`: all row operations, point/range lookups, index maintenance |
| `tableindex.go` | `treeSet` (the unique and secondary trees of a table), `indexSet` (load a snapshot or build from one scan), `buildIndexes`, maintenance after INSERT/UPDATE/DELETE, `dropIndexes` and `distrustIndexes` |
| `indexdef.go` | `indexDef`, `SqlTable.CreateIndex` / `DropIndex` (catalog row first, then memory), `checkSecondaryLengths` (`key too long for index`) |
| `catalogstore.go` | `catalogStore`: `sys_databases`, `sys_tables`, and `sys_indexes` files and their row indexes |
| `catalogload.go` | `catalogStore.load`: startup read, duplicate resolution, opening table files, index loading |
| `catalogcleanup.go` | `removeOrphans`: delete table files and `.idx` snapshots no catalog row references |
| `files.go` | `DataDir`, `fileAllocator`: file ids, paths, `makeDir`, `validateDatabaseName`, `fileIDOf` (the id in a `.tbl`, `.idx` or `.idx.tmp` name) |
| `rowcodec.go` | `EncodeRow` / `DecodeRow`: row bytes with a per-column (offset, length) table |
| `schemacodec.go` | `EncodeSchema` / `DecodeSchema`: JSON schema stored in `sys_tables` |
| `coerce.go` | `coerceValue`: raw literal `Token` to a native Go value for a column's type |
| `lookup.go` | Recognition (`planLookup`) and execution (`lookupRows`) of index equality and range lookups |
| `where.go` | `evalWhere`, `evalExpr`, `evalComparison`, `compareValues` |
| `response.go` | `Response{Columns, Rows}` and `stringifyValue` |
| `snapshot.go` | Snapshot lifecycle: `loadSnapshot`, `saveSnapshot`, `prepareHeapWrite` (removes the snapshot before the first heap write), `discardSnapshotFiles` |
| `wireset.go` | `WireSet` providing `NewEngine` |

## Key types and entry points

- `NewCatalog(dataDir DataDir) (Catalog, error)`: opens data directory, loads catalog and index definitions, removes orphan files and snapshots.
- `Engine.Execute(statement parser.SqlStatement, database string) (Response, error)`: executes statements including DDL (`CREATE/DROP INDEX`).

## Called by / calls

`internal/wiring` builds the catalog; `internal/grpcserver` calls `Execute`. The engine calls `storage.File` and `internal/index`.

## Gotchas

- Lock order is `Catalog.mu`, `Database.mu`, `Table.mu`, `tableIndexes.mu`, then `catalogStore.mu`. See [lock order](../concepts/lock-order.md).
- Multi-row and multi-step mutations validate and encode everything before the first write. See [validate then write](../concepts/validate-then-write.md).
- Bounded range lookups, and `=` or ranges on a CREATE INDEX column, use the page-count rule (`rangeCostFactor` = 2): fetched rows must lie on at most half of the file's pages. `lookupRows` tries a PRIMARY KEY/UNIQUE equality first, then unique ranges, then secondary ranges.
- A value too long to index is rejected by INSERT and UPDATE with `key too long for index`; a value already in the file degrades only that index to scans (a nil tree in the `treeSet`). `DROP COLUMN` of an indexed column is refused.
- Index snapshots are a cache removed before the first heap write after open, and neither loaded nor saved after a write failure or a stale entry: [snapshot removal](../decisions/snapshot-removed-before-first-write.md). Orphan snapshots are removed with orphan table files: [orphan removal](../decisions/orphan-removal-by-file-id.md).

## Related

[Engine subsystem](../subsystems/engine.md), [catalog persistence](../subsystems/catalog-persistence.md), [storage](storage.md), [parser](parser.md), [index](index.md).
