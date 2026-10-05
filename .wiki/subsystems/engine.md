---
title: Engine subsystem
kind: subsystem
sources:
  - internal/engine/
updated: 09d8ab0
---
# Engine subsystem

The engine executes parsed statements against a hierarchy that mirrors SQL: `Catalog` holds `Database`s, each holds `Table`s, each table holds its schema and a heap file. Package maps: [internal/engine](../code/engine.md) and [table.go](../code/engine-table.md).

## Execution path

`Engine.Execute(statement, database)` type-switches on the statement:

- Database statements (`CREATE`, `DROP`, `SHOW DATABASES`) go to the `Catalog`. `SHOW DATABASES` returns one column literally named `Database`.
- Table statements first require a non-empty database name (`resolveDatabase`), then look the table up (`resolveTable`, error `table "x" does not exist`).
- `CREATE TABLE`, `DROP TABLE` and `ALTER TABLE ... RENAME TO` go to the `Database`; other ALTER actions, INSERT, SELECT, UPDATE and DELETE go to the `Table`.
- A `Response` carries `Columns` and string `Rows`; the engine never builds protobuf messages.

## Properties to keep in mind

- Rows are only in the table's file: [rows only live in the file](../decisions/rows-only-in-file.md); finding rows means a [full scan](../concepts/full-table-scans.md).
- Mutations are [validate-then-write](../concepts/validate-then-write.md); [UPDATE writes new versions first](../decisions/update-inserts-before-tombstone.md).
- Locks are taken in a fixed order: [lock order](../concepts/lock-order.md).
- DDL is durable through the catalog: [catalog persistence](catalog-persistence.md).
- Literals are coerced here, not in the parser: [literal coercion](../concepts/literal-coercion.md).

## Storage layout

Table files are `<data_dir>/data/<db>/<file_id>.tbl`; see [files named by id](../decisions/file-ids-not-names.md). Rows are encoded by `EncodeRow` (a per-column `(offset uint16, length uint16)` table followed by payloads; offset `0xFFFF` means NULL, so `""` and NULL are distinct).
