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

Imports the standard library and `internal/storage` (for `storage.RowID`). `internal/storage` never imports `internal/index`.

## Files

| File | Responsibility |
| --- | --- |
| `keycodec.go` | `EncodeKey(v any) ([]byte, error)`: turns a coerced column value (`int64`, `string`, `bool`) into bytes whose `bytes.Compare` order equals the value order |
| `btree.go` | `Tree`: in-memory B+ tree mapping unique byte-string keys to `storage.RowID`, with `New`, `Insert`, `Get`, `Delete`, `Range`, `Len` and `MaxKeyLen` |

## Key types and entry points

- `EncodeKey(v any) ([]byte, error)`: encodes `int64` (8 bytes big-endian with sign bit flipped), `string` (raw bytes), and `bool` (1 byte). NULL (`nil`) and unsupported types return errors. Keys compare in value order only against keys of the same type.
- `New(nodeBytes int) (*Tree, error)`: creates an empty B+ tree whose nodes hold at most `nodeBytes` bytes. It fails if `nodeBytes` cannot hold a key of even one byte. `DefaultNodeBytes` is 16KiB, one storage page.
- `Tree.Insert(key []byte, id storage.RowID) error`: inserts a unique key and row ID, splitting nodes when they exceed the byte budget. It keeps its own copy of `key`. It fails and leaves the tree unchanged with `ErrDuplicateKey` if the key is present, or `ErrKeyTooLong` if it is longer than `MaxKeyLen`.
- `Tree.MaxKeyLen() int`: the longest key `Insert` accepts (4084 bytes with `DefaultNodeBytes`).
- `Tree.Get(key []byte) (storage.RowID, bool)`: looks up a key and returns its row ID.
- `Tree.Delete(key []byte) bool`: deletes a key, rebalancing via borrowing or merging.
- `Tree.Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID]`: yields keys in half-open range `[lo, hi)` in order. A nil `lo` starts at the first key and a nil `hi` runs to the last; a non-nil empty `hi` is an empty range.

## Called by / calls

Nothing calls `internal/index` yet (engine wiring for table indexes is a future sub-project). It calls `internal/storage` for `storage.RowID`.

## Gotchas

- The tree has no internal locking; the owning table must serialise access.
- `Range` yields the tree's own bytes; callers must not modify them, and the tree must not be modified while iterating.
- Nodes are limited by bytes, not entry count, as if each were serialised into a page, because keys vary in length. The spec records that this keeps split, borrow and merge unchanged when an on-disk node format arrives.
- `Delete` changes only the leaf holding the key plus the rebalancing that triggers (which can split a parent), so a separator can outlive its row. A lookup still reports the key missing because the leaf decides.

## Related

[Storage subsystem](../subsystems/storage.md), [Engine subsystem](../subsystems/engine.md).
