---
title: Engine subsystem
kind: subsystem
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
# Engine subsystem

The engine executes parsed statements against a hierarchy that mirrors SQL: `Catalog` holds `Database`s, each holds `Table`s, each table holds its schema, heap file, index trees, and secondary index definitions. Package maps: [internal/engine](../code/engine.md) and [table.go](../code/engine-table.md).

## Execution path

`Engine.Execute(statement, database)` type-switches on the statement:

- Database statements (`CREATE`, `DROP`, `SHOW DATABASES`) go to the `Catalog`.
- Table statements first require a non-empty database name, then look the table up.
- `CREATE INDEX` and `DROP INDEX` manage secondary index definitions.
- A `Response` carries `Columns` and string `Rows`.

## Index wiring and lookups

- **Uniqueness checks**: PRIMARY KEY and UNIQUE constraints are checked via in-memory B+ tree lookups instead of full table scans.
- **Point and range lookups**: `SELECT`, `UPDATE`, and `DELETE` whose WHERE clause pins or bounds a PRIMARY KEY/UNIQUE column, or a column with a `CREATE INDEX` (`=`, `<`, `<=`, `>`, `>=`), execute via index lookups (`lookupRows`). `=` on a PRIMARY KEY/UNIQUE column always does; every range, and `=` on an indexed column, follows the page-count rule (`rangeCostFactor` = 2): rows must lie on at most half of the file's pages. Otherwise, execution falls back to a full-table scan: [scans and lookups](../concepts/full-table-scans.md).
- **CREATE INDEX**: `CREATE INDEX name ON table (column)` records a non-unique, single-column definition in `sys_indexes` ([catalog persistence](catalog-persistence.md)); the tree is built later, in the same scan as the others.
- **Persistence**: a clean shutdown saves the trees to `<file_id>.idx`, and the first use after a restart loads them instead of scanning the heap. The first heap write after open removes the snapshot before writing, so a crash never leaves one older than the heap: [snapshot removal](../decisions/snapshot-removed-before-first-write.md).

## Properties to keep in mind

- Rows live only in the table's file: [rows only live in the file](../decisions/rows-only-in-file.md); unindexed queries or wide ranges fall back to [scans](../concepts/full-table-scans.md).
- Mutations are [validate-then-write](../concepts/validate-then-write.md); [UPDATE writes new versions first](../decisions/update-inserts-before-tombstone.md).
- Locks are taken in a fixed order: [lock order](../concepts/lock-order.md).
- DDL is durable through the catalog: [catalog persistence](catalog-persistence.md).
- Literals are coerced here: [literal coercion](../concepts/literal-coercion.md).

## Storage layout

Table files are `<data_dir>/data/<db>/<file_id>.tbl`, index snapshots are `<file_id>.idx`, and system files live under `sys/`.
