---
title: Storage subsystem
kind: subsystem
sources:
  - internal/storage/
  - internal/engine/rowcodec.go
  - docs/specs/2026-10-06-page-file-and-cache-design.md
  - docs/specs/2026-10-12-heap-get-design.md
  - docs/specs/2026-10-15-on-disk-index-design.md
  - pr: 25
  - pr: 36
  - pr: 41
  - pr: 45
updated: 33c2c35
---
# Storage subsystem

The storage layer is a standalone heap-file library that knows only bytes. The engine owns what the bytes mean. Package map: [internal/storage](../code/storage.md).

## Layout

A heap file is a sequence of 16KiB pages. Each page has a 12-byte header (checksum, page number, slot count, data start), a slot array growing forward, and row data growing backward from the end. A row is addressed by `RowID{Page, Slot}`. Heap files are built on top of `PageFile`, whose own 8-byte header (checksum and page number) is separate from the slotted page's 12 bytes. An index snapshot is also a `PageFile`, with a different layout: see [internal/index](../code/index.md).

## Write path

`Insert` appends to the cached tail page or starts a new page; `Delete` tombstones a slot. Both write the page straight through without fsync; the caller calls `Sync` once per statement. `CreateFile` and `SyncDir` fsync directory entries.

## Read path

`Scan` yields every live row in page order, verifying each page's checksum and page number.
`Get` and `GetMany` read rows by `RowID` without scanning; `GetMany` reads each run of consecutive row IDs on one page once.

## Limits

- Deletions tombstone slots and space is not reclaimed yet: [tombstone deletes](../decisions/tombstone-deletes-no-reclaim.md).
- A row over `MaxRowSize` (16368 bytes) is rejected.

## Engine-side encoding

Row bytes come from `EncodeRow` in `internal/engine/rowcodec.go`.
