---
title: internal/index
kind: code
sources:
  - internal/index/
  - docs/specs/2026-10-12-index-key-codec-design.md
  - docs/specs/2026-10-12-in-memory-btree-design.md
  - docs/specs/2026-10-15-on-disk-index-design.md
  - pr: 33
  - pr: 35
  - pr: 37
  - pr: 43
  - pr: 44
  - pr: 45
updated: 33c2c35
---
# internal/index

## Purpose

Order-preserving key encoding, secondary key encoding, in-memory B+ trees for indexing table columns, and on-disk snapshot persistence.

## Depends on / Must not import

Imports the standard library and `internal/storage` (for `storage.RowID`). `internal/storage` never imports `internal/index`.

## Files

| File | Responsibility |
| --- | --- |
| `keycodec.go` | `EncodeKey(v any) ([]byte, error)`: turns a coerced column value (`int64`, `string`, `bool`) into bytes whose `bytes.Compare` order equals the value order |
| `btree.go` | `Tree`: in-memory B+ tree mapping keys to `storage.RowID`, with `New`, `Insert`, `Get`, `Delete`, `Range`, `Len`, `MaxKeyLen`, and `MaxKeyLenFor` |
| `secondarykey.go` | `EncodeSecondaryKey`, `SecondaryEqualRange`, `SecondaryKeyLen`, `SecondaryKeyLenFor`: prefix-free order-preserving encoding for secondary non-unique indexes ending in row ID |
| `snapshot.go` | `WriteSnapshot`, `ReadSnapshot`: serialization of index trees to/from on-disk `.idx` snapshot files |

## Key types and entry points

- `EncodeKey(v any) ([]byte, error)`: encodes `int64` (8 bytes big-endian with sign bit flipped), `string` (raw bytes), and `bool` (1 byte). NULL (`nil`) and unsupported types return errors.
- `MaxKeyLenFor(nodeBytes int) int`: computes the maximum key length accepted by a tree with nodes of `nodeBytes`.
- `EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error)`: encodes a secondary index entry containing the value part and the `RowID` (page and slot), ensuring duplicate values sort in file order.
- `SecondaryEqualRange(v any) (lo, hi []byte, error)`: produces key bounds for looking up all rows with value `v` in a secondary tree.
- `New(nodeBytes int) (*Tree, error)`: creates an empty B+ tree. `DefaultNodeBytes` is 16KiB.
- `Tree.Insert(key []byte, id storage.RowID) error`: inserts a key and row ID. Fails with `ErrDuplicateKey` (for unique trees) or `ErrKeyTooLong`.
- `Tree.Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID]`: yields keys in half-open range `[lo, hi)` in order.
- `WriteSnapshot(path string, heapID int64, heapPages int32, trees []SnapshotTree) error` and `ReadSnapshot(...)`: write and read index trees to/from disk snapshots.

## Called by / calls

Called by `internal/engine` for table primary key, unique, and secondary indexes. Calls `internal/storage` for `storage.RowID`.

## Gotchas

- The tree has no internal locking; the owning table must serialise access.
- `Range` yields the tree's own bytes; callers must not modify them.
- Secondary keys include page and slot at the tail so multiple rows with the same value are ordered by file position.

## Related

[Storage subsystem](../subsystems/storage.md), [Engine subsystem](../subsystems/engine.md).
