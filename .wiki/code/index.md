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
| `secondarykey.go` | `EncodeSecondaryKey`, `SecondaryEqualRange`, `SecondaryKeyLen`: prefix-free order-preserving encoding for secondary non-unique indexes: the value part, then the row's page and slot |
| `snapshot.go` | `WriteSnapshot`, `ReadSnapshot`, `TreeID`, `SnapshotTree`: the on-disk `.idx` snapshot of a table's trees |

## Key types and entry points

- `EncodeKey(v any) ([]byte, error)`: encodes `int64` (8 bytes big-endian with sign bit flipped), `string` (raw bytes), and `bool` (1 byte). NULL (`nil`) and unsupported types return errors.
- `MaxKeyLenFor(nodeBytes int) int`: computes the maximum key length accepted by a tree with nodes of `nodeBytes`.
- `EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error)`: encodes a secondary index entry containing the value part and the `RowID` (page and slot), ensuring duplicate values sort in file order.
- `SecondaryEqualRange(v any) (lo, hi []byte, error)`: produces key bounds for looking up all rows with value `v` in a secondary tree.
- `New(nodeBytes int) (*Tree, error)`: creates an empty B+ tree. `DefaultNodeBytes` is 16KiB.
- `Tree.Insert(key []byte, id storage.RowID) error`: inserts a key and row ID. Keys are always unique in a tree; a secondary tree gets unique keys because each ends in the row id. Fails with `ErrDuplicateKey` or `ErrKeyTooLong`.
- `SecondaryKeyLen(v any) (int, error)`: the length of the secondary key for `v` (value part plus 8), which the engine compares with `MaxKeyLen` to reject a value too long to index.
- `Tree.Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID]`: yields keys in half-open range `[lo, hi)` in order.
- `WriteSnapshot(path, heapID, heapPages, trees []SnapshotTree) error`: writes the trees as sorted entries in a `storage.PageFile` (a header page, then entry pages) to `path.tmp`, fsyncs, renames it over `path` and fsyncs the directory, so `path` is absent or complete. A nil tree is saved as degraded.
- `ReadSnapshot(path, heapID, heapPages, want []TreeID) ([]SnapshotTree, error)`: loads the trees in `want`'s order. It fails on a damaged file, and wraps `ErrSnapshotMismatch` for a valid snapshot of another heap file, another page count, or another set of trees. Entries must be strictly increasing and counted exactly, with no trailing pages.
- `TreeID{Kind, Ordinal, Name, Column}`: a unique tree is its column's schema index and name; a CREATE INDEX tree is its index name, column name and the column's schema index (two columns can swap names).

## Called by / calls

Called by `internal/engine` for table primary key, unique, and secondary indexes. Calls `internal/storage` for `storage.RowID`.

## Gotchas

- The tree has no internal locking; the owning table must serialise access.
- `Range` yields the tree's own bytes; callers must not modify them.
- Secondary keys include page and slot at the tail so multiple rows with the same value are ordered by file position.
- A snapshot is only a cache: nothing here decides whether it is current. The engine does, by removing it before the first heap write (see [snapshot removal](../decisions/snapshot-removed-before-first-write.md)).

## Related

[Storage subsystem](../subsystems/storage.md), [Engine subsystem](../subsystems/engine.md).
