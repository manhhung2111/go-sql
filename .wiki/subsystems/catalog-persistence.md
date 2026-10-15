---
title: Catalog persistence subsystem
kind: subsystem
sources:
  - internal/engine/catalogstore.go
  - internal/engine/catalogload.go
  - internal/engine/catalogcleanup.go
  - internal/engine/schemacodec.go
  - internal/engine/files.go
  - docs/specs/2026-10-15-create-index-design.md
  - docs/specs/2026-10-15-on-disk-index-design.md
  - pr: 14
  - pr: 15
  - pr: 16
  - pr: 43
  - pr: 45
updated: 33c2c35
---
# Catalog persistence subsystem

Databases, tables, index definitions, and index snapshots survive a restart. System heap files under `<storage.data_dir>/sys/`: `sys_databases.tbl`, `sys_tables.tbl`, and `sys_indexes.tbl`.

## Where it lives

`sys_indexes` records named secondary-index definitions (`db`, `tbl`, `name`, `col`). `catalogStore` owns all three system files.

## Write side

Every DDL statement writes its catalog row and fsyncs before changing memory: [catalog row is the commit point](../decisions/catalog-row-is-the-commit-point.md). Replacing a row is insert, sync, tombstone, sync: [replace-row ordering](../decisions/replace-row-insert-then-tombstone.md). Index definitions for renames write new index rows before replacing table rows.

## Startup (`NewCatalog`)

1. `newFileAllocator` scans existing `.tbl`, `.idx`, and `.idx.tmp` files to continue file IDs after the highest one.
2. `openCatalogStore` opens system files.
3. `catalogStore.load` reads databases, tables, and index definitions. Index definitions are validated against surviving tables and columns.
4. `removeOrphans` deletes table files and index snapshots no survivor references.

## Shutdown

`Catalog.Close()` closes every table's files and saves index trees to `<file_id>.idx` snapshots.
