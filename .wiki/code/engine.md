---
title: internal/engine
kind: code
sources:
  - internal/engine/
updated: 09d8ab0
---
# internal/engine

## Purpose

Executes parsed statements. The types mirror the SQL hierarchy: `Catalog` (databases) holds `Database` (tables) holds `Table` (schema plus its heap file). Table rows live only in the table's file, and the catalog itself is stored in two system files.

## Depends on / Must not import

Imports `internal/parser` and `internal/storage`. It must not import `proto/sqlpb`; no file in `internal/engine` does. Converting rows to protobuf messages is `internal/grpcserver`'s job.

## Files

| File | Responsibility |
| --- | --- |
| `engine.go` | `Engine` interface, `NewEngine`, `Execute`: type-switch over statement types |
| `catalog.go` | `Catalog` interface, `SqlCatalog`, `NewCatalog` (open, load, remove orphans), create/drop database |
| `database.go` | `Database` interface, `SqlDatabase`: create, rename, drop table; per-database directory |
| `table.go` | `Table` interface, `SqlTable`: all row operations; see [engine-table](engine-table.md) |
| `catalogstore.go` | `catalogStore`: the `sys_databases` and `sys_tables` files and their row index |
| `catalogload.go` | `catalogStore.load`: startup read, duplicate resolution, opening table files |
| `catalogcleanup.go` | `removeOrphans`: delete table files no catalog row references |
| `files.go` | `DataDir`, `fileAllocator`: file ids, paths, `makeDir`, `validateDatabaseName` |
| `rowcodec.go` | `EncodeRow` / `DecodeRow`: row bytes with a per-column (offset, length) table |
| `schemacodec.go` | `EncodeSchema` / `DecodeSchema`: JSON schema stored in `sys_tables` |
| `coerce.go` | `coerceValue`: raw literal `Token` to a native Go value for a column's type |
| `where.go` | `evalWhere`, `evalExpr`, `evalComparison`, `compareValues` |
| `response.go` | `Response{Columns, Rows}` and `stringifyValue` |
| `wireset.go` | `WireSet` providing `NewEngine` |

## Key types and entry points

- `NewCatalog(dataDir DataDir) (Catalog, error)`: opens the data directory, loads the catalog, removes orphan files.
- `Engine.Execute(statement parser.SqlStatement, database string) (Response, error)`: the seam for new statement types. An unhandled statement type falls into `default` and returns `statement not supported, got %T`; keep it an error, not a no-op. See [AST sum types](../concepts/ast-sum-types.md).
- `Catalog.Close()` closes every database's table files and the catalog store.

## Called by / calls

`internal/wiring` builds the catalog (`ProvideCatalog`); `internal/grpcserver` calls `Execute`. The engine calls `storage.File` for all disk I/O.

## Gotchas

- Lock order is `Catalog.mu`, then `Database.mu`, then `Table.mu`, then `catalogStore.mu`. See [lock order](../concepts/lock-order.md).
- Multi-row and multi-step mutations validate and encode everything before the first write. See [validate then write](../concepts/validate-then-write.md).
- `DROP TABLE` and `DROP DATABASE` can return an error after the object is already gone (`table "x" dropped, but removing its file failed` and `database "x" dropped, but cleaning up after it failed`). That leaves an orphan file and never a dangling catalog entry. See [catalog row is the commit point](../decisions/catalog-row-is-the-commit-point.md).
- A database name becomes a directory name, so `validateDatabaseName` rejects empty, `.`, `..` and names containing `/`, `\` or NUL.
- A table whose schema JSON does not fit one page cannot be created (`schema of table ... is too large to store`).
- Every statement that has to find rows scans the whole table file; there are no indexes. See [full-table scans](../concepts/full-table-scans.md).

## Related

[Engine subsystem](../subsystems/engine.md), [catalog persistence](../subsystems/catalog-persistence.md), [storage](storage.md), [parser](parser.md).
