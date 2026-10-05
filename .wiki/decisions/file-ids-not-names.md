---
title: Table files are named by numeric id
kind: decision
status: accepted
sources:
  - internal/engine/files.go
  - internal/engine/table.go
  - pr: 5
  - pr: 14
updated: 09d8ab0
---
# Table files are named by numeric id

## Context

A table's file lives at `<data_dir>/data/<db>/<file_id>.tbl`. Naming it by the table name would make `ALTER TABLE ... RENAME TO` touch the disk.

## Decision

Files are named by a numeric `file_id` (#5). `fileAllocator.nextFileID()` hands out ids and `tablePath(db, id)` builds paths. Ids are shared across all databases and are not reused within a process. At startup `newFileAllocator` continues after the highest `<id>.tbl` found on disk, so a restart cannot pick an id whose file already exists. An `ALTER ADD/DROP COLUMN` rebuild writes a new file with a new, larger id (#14).

## Consequences

- Renaming a table never touches disk; only the catalog row changes.
- Ids are numbered from the highest file on disk, so after a restart that follows removal of the highest-numbered files, numbering can restart lower than before the restart.
- Orphan detection can work by id alone; see [orphan removal by file id](orphan-removal-by-file-id.md).

## Alternatives

Not recorded.

## Superseded by

None.
