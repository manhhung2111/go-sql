---
title: Deletes tombstone a slot and space is not reclaimed
kind: decision
status: accepted
sources:
  - internal/storage/page.go
  - internal/storage/file.go
  - pr: 3
  - pr: 4
updated: 09d8ab0
---
# Deletes tombstone a slot and space is not reclaimed

## Context

Rows are addressed by `RowID{Page, Slot}`. #3 scoped the heap file to insert and append only (no indexing, buffer pool, crash recovery or on-disk UPDATE/DELETE); #4 added in-place modification.

## Decision

`DeleteRow` sets a slot's offset and length to 0 (a live row can never start at offset 0, since the header occupies the first bytes), so slot indices never shift and a `RowID` stays valid for the row's lifetime (#4). Insert never reuses a tombstoned slot.

## Consequences

- Deleted rows' space is never reclaimed; `Page.DeleteRow` says "its space is not reclaimed".
- Scans still read tombstoned slots and skip them.
- Out of scope in #4: a WAL and space reclamation. A torn page write is detected by the checksum but not repaired.
- A row larger than one page is rejected (`MaxRowSize`, 16368 bytes) because there are no overflow pages.

## Alternatives

Not recorded.

## Superseded by

None.
