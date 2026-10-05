---
title: Catalog persistence subsystem
kind: subsystem
sources:
  - internal/engine/catalogstore.go
  - internal/engine/catalogload.go
  - internal/engine/catalogcleanup.go
  - internal/engine/schemacodec.go
  - internal/engine/files.go
  - pr: 14
  - pr: 15
  - pr: 16
updated: 09d8ab0
---
# Catalog persistence subsystem

Databases and tables survive a restart. The catalog was made durable in three PRs: #14 (schema codec, file ids, durable directory creation), #15 (writing the catalog on every DDL statement) and #16 (loading it at startup and recovering from crashes).

## Where it lives

Two system heap files under `<storage.data_dir>/sys/`: `sys_databases.tbl` with column `name`, and `sys_tables.tbl` with columns `db`, `name`, `file_id` and `schema` (JSON). Their own schemas are hard-coded. `catalogStore` owns them and remembers where each live row is (`dbRows`, `tableRows`), so a row can be tombstoned without scanning.

## Write side

Every DDL statement writes its catalog row and fsyncs before changing memory: [catalog row is the commit point](../decisions/catalog-row-is-the-commit-point.md). Replacing a row is insert, sync, tombstone, sync: [replace-row ordering](../decisions/replace-row-insert-then-tombstone.md). Schemas are JSON with stable tags: [schema codec](../decisions/schema-json-stable-tags.md). Directory creation fsyncs the directory, each ancestor up to the data directory, and its parent, so a catalog row never points into a directory a power loss forgets (`makeDir`).

## Startup (`NewCatalog`)

1. `newFileAllocator` scans existing files to continue file ids after the highest one.
2. `openCatalogStore` opens or creates the two system files; it does not read rows.
3. `catalogStore.load` reads `sys_databases` (a repeated name keeps the first row), then `sys_tables`: rows for unknown databases are stale, and a later row displaces a survivor sharing its `(db, name)` or `file_id`. Losers are tombstoned after the scans. Each surviving table's schema is decoded and its file opened.
4. `removeOrphans` deletes files no survivor references: [orphan removal](../decisions/orphan-removal-by-file-id.md).
5. A live row whose file is missing or torn, or a damaged system file, refuses startup rather than guessing, and every file already opened is closed again.

## Shutdown

`Catalog.Close()` closes every database's table files and the store. `cmd/server` runs it through the cleanup function from `wiring.InitializeServer` after `GracefulStop` and on every other exit path.

## Limits

A schema larger than one page cannot be created. A damaged page inside a table file shows up when that table is first scanned, not at startup (#16).
