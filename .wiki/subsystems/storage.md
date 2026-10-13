---
title: Storage subsystem
kind: subsystem
sources:
  - internal/storage/
  - internal/engine/rowcodec.go
  - docs/specs/2026-10-06-page-file-and-cache-design.md
  - docs/specs/2026-10-12-heap-get-design.md
  - pr: 25
  - pr: 36
updated: 329e8d9
---
# Storage subsystem

The storage layer is a standalone heap-file library that knows only bytes. The engine owns what the bytes mean. Package map: [internal/storage](../code/storage.md).

## Layout

A heap file is a sequence of 16KiB pages. Each page has a 12-byte header (checksum, page number, slot count, data start), a slot array growing forward, and row data growing backward from the end. A row is addressed by `RowID{Page, Slot}`. Heap files are built on top of `PageFile`, which provides page-level random access and verifies/stamps format-neutral headers.

## Write path

`Insert` appends to the cached tail page or starts a new page; `Delete` tombstones a slot. Both write the page straight through without fsync; the caller calls `Sync` once per statement. `CreateFile` and `SyncDir` fsync directory entries so a created file survives a power loss.

## Read path

`Scan` yields every live row in page order, verifying each page's checksum and page number; a corrupt page ends the scan with its error. `OpenFile` rejects a file that is not a whole number of pages. `Scan` reads pages straight through `PageFile.ReadPage` and does not use a cache.

`Get` reads one row by its `RowID` without scanning, and fails with `ErrRowNotFound` when the address is stale; see [internal/storage](../code/storage.md).

`NewCachedPageFile` is a write-through LRU cache of immutable page buffers (no pins, no dirty pages) that wraps any `PageFile`. Nothing uses it yet: it is a building block for a future index, and neither the heap file nor `Scan` goes through it.

## Limits

- No wired-in indexes, buffer pool, WAL, overflow pages or space reclamation. An in-memory B+ tree and key codec ([internal/index](../code/index.md)) exist but no table uses them yet. The `PageFile` and the unused write-through page cache are groundwork for an index, not a buffer pool: [tombstone deletes](../decisions/tombstone-deletes-no-reclaim.md), [full scans](../concepts/full-table-scans.md).
- A row over `MaxRowSize` (16368 bytes) is rejected by `Insert`; the engine rejects it earlier so a batch writes nothing.

## Engine-side encoding

Row bytes come from `EncodeRow` in `internal/engine/rowcodec.go`; the storage package never sees column types.
