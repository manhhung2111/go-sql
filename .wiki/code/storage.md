---
title: internal/storage
kind: code
sources:
  - internal/storage/
  - docs/superpowers/specs/2026-10-06-page-file-and-cache-design.md
  - pr: 3
  - pr: 4
  - pr: 25
updated: 7660bd0
---
# internal/storage

## Purpose

Heap files made of 16KiB checksummed slotted pages. It deals only in `[]byte` rows.

## Depends on / Must not import

Standard library only. It never imports `internal/parser` or `internal/engine`.

## Files

| File | Responsibility |
| --- | --- |
| `page.go` | `Page` interface and slotted page: `NewPage`, `DecodePage`, `InsertRow`, `Row`, `DeleteRow`, `SlotCount`, `PageNumber`, `Encode`; constants `maxPageSize`, `MaxRowSize` |
| `pagefile.go` | `PageFile` interface (`ReadPage`, `WritePage`, `AllocatePage`, `NumPages`, `Sync`, `Close`), `CreatePageFile`, `OpenPageFile`: verifies and stamps format-neutral headers (checksum and page number) |
| `pagecache.go` | `NewCachedPageFile`: per-file, write-through LRU cache with immutable buffers (no pins, no dirty pages) |
| `file.go` | `File` interface built on top of `PageFile`, `CreateFile`, `OpenFile`, `RowID{Page, Slot}`, `Row`; `Insert`, `Delete`, `Scan`, `Sync`, `Close` |
| `dir.go` | `SyncDir`: fsync a directory so created entries survive a power loss |
| `storage.go` | Package declaration only |

## Key types and entry points

- Page layout (little-endian header): checksum (bytes 0-4, CRC32 over bytes 4 to the end), page number (4-8), slot count (8-10), data start (10-12). The slot array grows forward from the header and row data grows backward from the page end.
- `MaxRowSize` is `16384 - 12 - 4 = 16368` bytes: a full page minus the header and the row's own slot.
- `PageFile` provides random page-level access (`ReadPage`, `WritePage`, `AllocatePage`) verifying and stamping the 8 format-neutral header bytes.
- `NewCachedPageFile` wraps a `PageFile` with a write-through LRU cache of immutable buffers (no pins, no dirty pages; replacements replace cached frames, and failed writes drop the entry).
- `File.Scan()` is an `iter.Seq2[Row, error]`: it verifies each page's checksum and page number and ends with an error on a corrupt page. `Row.Bytes` is only valid until the next iteration.
- `CreateFile` fails if the path exists and fsyncs the parent directory; `OpenFile` rejects a size that is not a whole number of pages.

## Called by / calls

Called by `internal/engine` (`SqlTable`, `catalogStore`). Calls only the OS.

## Gotchas

- `Insert` and `Delete` write the page straight through but do not fsync. The caller calls `Sync` once per statement.
- Any number of `Scan`s may run concurrently; `Insert`, `Delete`, `Sync` and `Close` must not run concurrently with anything. The owning table enforces this with its read and write locks.
- A scan snapshots the page count when it starts, so rows inserted during a scan are not part of it.
- If a write fails, the cached tail page is dropped so a change that never reached disk is not flushed by a later write.
- Deletes tombstone a slot (offset 0, length 0) and space is never reclaimed; see [tombstone deletes](../decisions/tombstone-deletes-no-reclaim.md).
- A torn page write is detected by the checksum but not repaired; there is no WAL.

## Related

[Storage subsystem](../subsystems/storage.md), [engine](engine.md).
