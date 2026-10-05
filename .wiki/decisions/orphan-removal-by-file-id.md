---
title: Orphan files are removed by id, never by directory name
kind: decision
status: accepted
sources:
  - internal/engine/catalogcleanup.go
  - internal/engine/catalog.go
  - pr: 16
updated: 09d8ab0
---
# Orphan files are removed by id, never by directory name

## Context

Crashes and failed cleanups leave table files that no catalog row refers to. On a case-insensitive filesystem (macOS) two databases whose names differ only by case share one directory (#5, #16).

## Decision

After `NewCatalog` has loaded the catalog and opened every surviving table, `removeOrphans` deletes only files named `<number>.tbl` whose id no survivor references, wherever they sit. It removes a directory only if it is empty and no database has its exact name, using non-recursive `os.Remove`, so it can never delete a file it did not create. Failures are ignored: an orphan wastes space and nothing else. A removed directory a database still needs is recreated by its next `CREATE TABLE`.

## Consequences

- A database whose name differs only by case never loses its files.
- Cleanup runs only after every survivor is open, and the allocator scanned ids first, so a removed orphan's id is not handed out again within the process.
- Table files left by builds before the catalog was durable have no catalog rows, so the first start of the catalog-loading build removes them (#16).
- A live catalog row whose file is missing or torn, or a damaged system file, refuses startup instead of guessing (#16); a damaged page inside a table file is only found when that table is first scanned.

## Alternatives

Not recorded.

## Superseded by

None.
