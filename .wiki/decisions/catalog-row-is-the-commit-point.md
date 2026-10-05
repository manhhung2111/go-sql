---
title: Catalog row is the commit point
kind: decision
status: accepted
sources:
  - internal/engine/catalogstore.go
  - internal/engine/catalog.go
  - internal/engine/database.go
  - pr: 15
updated: 09d8ab0
---
# Catalog row is the commit point

## Context

DDL changes both memory and disk, and a crash or I/O failure can land between the two. Before #15 nothing about the catalog was on disk; #15 made DDL write it, and #16 made startup read it back.

## Decision

Every DDL statement (`CREATE` / `DROP DATABASE`, `CREATE` / `DROP` / `RENAME TABLE`, `ALTER ... ADD` / `DROP` / `RENAME COLUMN`) writes its catalog row and fsyncs before changing memory. A failed catalog write changes nothing in memory and discards any new file the statement had created. For `DROP DATABASE`, tombstoning the database row is the commit point (`removeDatabase`); once it succeeds the database is gone after a restart.

## Consequences

- DDL statements can now fail with the catalog write's I/O error (#15).
- After the commit point only cleanup remains. If cleanup fails, `DropDatabase` returns `database "x" dropped, but cleaning up after it failed`, leaving orphan files and never a database that points at missing ones. `DropTable` behaves the same way for a table (`table "x" dropped, but removing its file failed`).
- If `DROP DATABASE` commits but tombstoning its table rows fails, the stale rows stay in the store's index and the next `CREATE DATABASE` of that name sweeps them (`addDatabase` calls `tombstoneTablesLocked` first), so the old tables are never inherited.
- Startup repairs leftovers: see [orphan removal by file id](orphan-removal-by-file-id.md).

## Alternatives

Not recorded.

## Superseded by

None.
