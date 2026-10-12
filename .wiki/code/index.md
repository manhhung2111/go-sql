---
title: internal/index
kind: code
sources:
  - internal/index/
  - docs/specs/2026-10-12-index-key-codec-design.md
  - docs/specs/2026-10-12-in-memory-btree-design.md
  - pr: 33
  - pr: 35
updated: 329e8d9
---
# internal/index

## Purpose

Order-preserving key encoding and in-memory B+ tree for indexing table columns.

## Depends on / Must not import

Imports standard library and `internal/storage` (for `storage.RowID`). It must not import `internal/engine`, `internal/parser`, or `internal/grpcserver`.

## Files

| File | Responsibility |
| --- | --- |
| `keycodec.go` | `EncodeKey(v any) ([]byte, error)`: turns a coerced column value (`int64`, `string`, `bool`) into bytes whose `bytes.Compare` order equals the value order |
| `btree.go` | `Tree`: in-memory B+ tree mapping unique byte-string keys to `storage.RowID`, with `New`, `Insert`, `Get`, `Delete`, `Range`, and `Len` |

## Key types and entry points

- `EncodeKey(v any) ([]byte, error)`: encodes `int64` (8 bytes big-endian with sign bit flipped), `string` (raw bytes), and `bool` (1 byte). NULL (`nil`) and unsupported types return errors.
- `New(nodeBytes int) (*Tree, error)`: creates an empty B+ tree with a specified node byte budget (default 16KiB).
- `Tree.Insert(key []byte, id storage.RowID) error`: inserts a unique key and row ID, splitting nodes when they exceed the byte budget.
- `Tree.Get(key []byte) (storage.RowID, bool)`: looks up a key and returns its row ID.
- `Tree.Delete(key []byte) bool`: deletes a key, rebalancing via borrowing or merging.
- `Tree.Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID]`: yields keys in half-open range `[lo, hi)` in order.

## Called by / calls

Nothing calls `internal/index` yet (engine wiring for table indexes is a future sub-project). It calls `internal/storage` for `storage.RowID`.

## Gotchas

- The tree has no internal locking; the owning table must serialise access.
- `Range` yields the tree's own bytes; callers must not modify them, and the tree must not be modified while iterating.
- Node sizes are accounted by byte budgets (mimicking slotted pages) rather than entry counts.

## Related

[Storage subsystem](../subsystems/storage.md), [Engine subsystem](../subsystems/engine.md).
