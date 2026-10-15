---
title: The index snapshot is removed before the first heap write
kind: decision
status: accepted
sources:
  - internal/engine/snapshot.go
  - internal/engine/tableindex.go
  - internal/engine/table.go
  - internal/index/snapshot.go
  - docs/specs/2026-10-15-on-disk-index-design.md
  - pr: 45
updated: 33c2c35
---
# The index snapshot is removed before the first heap write

## Context

Index trees are a rebuildable cache of the heap, and there is no write-ahead log. A clean shutdown saves them to `<file_id>.idx` so a restart can skip the scan. But a saved tree may only be trusted if the heap has not changed since, and nothing records a heap change except the heap itself.

## Decision

Before the first INSERT, UPDATE or DELETE that changes a heap after the table is opened from the catalog, `prepareHeapWrite` unlinks its snapshot and fsyncs the directory, then clears `snapshotMayExist`. A crash after the write therefore finds no snapshot and rebuilds. If the removal fails, the statement fails before anything is written. It runs after validation, so a rejected statement or a DELETE that matches nothing leaves the snapshot alone.

A snapshot that fails to load (damage, another page count, another set of trees) is removed, but not durably, so `snapshotMayExist` stays true and the next heap write still syncs the directory. A load also checks the heap file id and page count, each tree's column (including its schema index, since two columns can swap names), entry order and entry counts.

After a heap write or fsync failure, or an index entry that points at a row that is not there, `distrustIndexes` stops the run from loading or saving a snapshot: the trees would be rebuilt from the page cache, which may hold bytes that never reached the disk, and a saved snapshot would pass every check against a heap that lost them. Only a table's new file (ALTER) clears this.

## Consequences

- Cost: one unlink and one directory fsync per table per process lifetime, on its first heap write.
- A table created in this run, or given a new file by `ADD`/`DROP COLUMN`, never has a snapshot to remove.
- `CREATE INDEX`, `DROP INDEX` and `DROP TABLE` remove the snapshot files; startup removes unreferenced ones: [orphan removal](orphan-removal-by-file-id.md).
- Restart is about 2x faster, not more, in the one measurement taken (100,000 rows, first indexed statement 15.5 ms against 29.1 ms), because reloading the entries still costs.
- Known limit: a tree saved as degraded stays degraded across restarts, so that column keeps scanning.

## Alternatives

The spec considered a heap fingerprint compared on load and writing the index through on every statement; it chose this protocol because it needs no heap scan to validate and no extra write per statement.

## Superseded by

None.
