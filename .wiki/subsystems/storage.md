---
title: Storage subsystem
kind: subsystem
sources:
  - internal/storage/
  - internal/engine/rowcodec.go
  - docs/superpowers/specs/2026-10-06-page-file-and-cache-design.md
  - pr: 25
updated: 7660bd0
---
# Storage subsystem

The storage layer is a standalone heap-file library that knows only bytes. The engine owns what the bytes mean. Package map: [internal/storage](../code/storage.md).

## Layout

A heap file is a sequence of 16KiB pages. Each page has a 12-byte header (checksum, page number, slot count, data start), a slot array growing forward, and row data growing backward from the end. A row is addressed by `RowID{Page, Slot}`. Heap files are built on top of `PageFile`, which provides page-level random access and verifies/stamps format-neutral headers.

## Write path

`Insert` appends to the cached tail page or starts a new page; `Delete` tombstones a slot. Both write the page straight through without fsync; the caller calls `Sync` once per statement. `CreateFile` and `SyncDir` fsync directory entries so a created file survives a power loss.

## Read path

`Scan` yields every live row in page order, verifying each page's checksum and page number; a corrupt page ends the scan with its error. `OpenFile` rejects a file that is not a whole number of pages. A write-through page cache (`NewCachedPageFile`) provides per-file caching of immutable page buffers using LRU eviction without pins or dirty pages.

## Limits

- No indexes (other than page-level file and cache prerequisites), buffer pool (beyond per-file page cache), WAL, overflow pages or space reclamation: [tombstone deletes](../decisions/tombstone-deletes-no-reclaim.md), [full scans](../concepts/full-table-scans.md).
- A row over `MaxRowSize` (16368 bytes) is rejected by `Insert`; the engine rejects it earlier so a batch writes nothing.

## Engine-side encoding

Row bytes come from `EncodeRow` in `internal/engine/rowcodec.go`; the storage package never sees column types.
