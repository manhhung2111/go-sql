---
title: internal/storage
kind: code
sources:
  - internal/storage/
  - docs/specs/2026-10-06-page-file-and-cache-design.md
  - docs/specs/2026-10-12-heap-get-design.md
  - pr: 3
  - pr: 4
  - pr: 25
  - pr: 36
  - pr: 41
  - pr: 45
updated: 33c2c35
---
# internal/storage

## Purpose

Heap files made of 16KiB checksummed slotted pages, plus page count and multi-row batched reads (`GetMany`). It deals only in `[]byte` rows.

## Depends on / Must not import

Standard library only. It never imports `internal/parser` or `internal/engine`.

## Files

| File | Responsibility |
| --- | --- |
| `page.go` | `Page` interface and slotted page: `NewPage`, `DecodePage`, `InsertRow`, `Row`, `DeleteRow`, `SlotCount`, `PageNumber`, `Encode`; constants `maxPageSize`, `MaxRowSize`, `PageSize`, `PageHeaderSize` |
| `pagefile.go` | `PageFile` interface (`ReadPage`, `WritePage`, `AllocatePage`, `NumPages`, `Sync`, `Close`), `CreatePageFile`, `OpenPageFile`: verifies and stamps format-neutral headers (checksum and page number) |
| `pagecache.go` | `NewCachedPageFile`: per-file, write-through LRU cache with immutable buffers |
| `file.go` | `File` interface built on top of `PageFile`, `CreateFile`, `OpenFile`, `RowID{Page, Slot}`, `Row`; `Insert`, `Delete`, `Get`, `GetMany`, `NumPages`, `Scan`, `Sync`, `Close` |
| `dir.go` | `SyncDir`: fsync a directory so created entries survive a power loss |
| `storage.go` | Package declaration only |

## Key types and entry points

- Page layout (little-endian header): checksum (bytes 0-4, CRC32 over bytes 4 to the end), page number (4-8), slot count (8-10), data start (10-12).
- `MaxRowSize` is `16384 - 12 - 4 = 16368` bytes.
- `PageFile` provides random page-level access.
- `File.NumPages() int32`: returns the number of pages in the file.
- `File.GetMany(ids []RowID) ([][]byte, error)`: reads a copy of rows at the given addresses. Reads each run of consecutive IDs on one page once. Fails with `ErrRowNotFound` if any ID is stale.
- `File.Get(id RowID) ([]byte, error)`: reads one row by its `RowID` using `GetMany`.
- `File.Scan()` yields every live row in page order.

## Called by / calls

Called by `internal/engine` (`SqlTable`, `catalogStore`, index snapshots). Calls only the OS.

## Gotchas

- `Insert` and `Delete` write the page straight through without fsync; the caller calls `Sync` once per statement.
- `GetMany` assumes IDs may not be sorted and groups/reads pages efficiently, but callers should provide sorted IDs when possible.
- Deletes tombstone a slot and space is never reclaimed.

## Related

[Storage subsystem](../subsystems/storage.md), [engine](engine.md).
