# In-memory B+ tree: design

Status: draft for review. Second sub-project of the B-tree index; follows the [key codec](2026-10-12-index-key-codec-design.md).

## Goal

Add a B+ tree to `internal/index` that maps encoded keys to `storage.RowID`, holds everything in memory, and sizes its nodes in bytes as if they were serialised pages. Nothing in this change touches the engine or the heap.

## Context

The B-tree work is split into sub-projects, each with its own spec, plan and PR: (1) the key codec, (2) this tree, (3) a heap `File.Get(RowID)`, (4) engine wiring, (5) later phases: the on-disk node format on `PageFile`, index metadata in the catalog, `CREATE INDEX`, range queries.

Decisions already made for the whole effort:

- The index is a rebuildable cache of the heap: the heap write is the commit point, there is no WAL, and a missing or stale index is rebuilt from the heap.
- In memory first, disk format later.
- Only PRIMARY KEY and UNIQUE columns are indexed, one index per column, so keys are unique.

## Decisions made during brainstorming

- **B+ tree.** Keys and `RowID`s live only in leaves. Internal nodes hold separator keys and child pointers. Leaves are chained left to right so `Range` is a walk. This is the layout the on-disk version will use.
- **Nodes are limited by bytes, not entry count.** Keys are variable-length strings, so a count would not match what fits in a 16KiB page. Doing it in bytes from the start means the on-disk phase swaps pointers for page ids and does not rewrite split, borrow or merge.
- **Unique keys only.** `Insert` of an existing key fails and changes nothing.
- **Keys are the codec's `[]byte`; values are `storage.RowID`.** `internal/index` therefore imports `internal/storage`. `storage` never imports `index`.
- **No internal locking.** The owning `Table.mu` guards the tree: reads under the read lock, mutations under the write lock.
- **Stale separators are allowed.** A separator is a signpost that divides a node's left subtree (`< S`) from its right subtree (`>= S`); it is not a row. Delete changes only the leaf (plus the rebalancing it triggers) and may leave a separator whose row is gone. A search for that key still returns not found, because the leaf decides. The separator disappears when the two nodes around it merge. Rewriting ancestors on every delete would touch several nodes (several pages on disk) to save a few bytes.
- **No minimum-fill guarantee.** With variable-length keys, "at least half full" cannot always be met, so the invariants check structure and byte accounting, not fill.

## Design

### Size accounting

Every node tracks `used`, the bytes it would occupy serialised. The constants sit in one place:

| Item | Bytes |
| --- | --- |
| Node header (kind, entry count, next-leaf id or leftmost child id) | 16 |
| Leaf entry | 2 (key length) + key + 6 (`RowID`: page `int32`, slot `uint16`) |
| Internal entry | 2 (key length) + separator key + 4 (right child id) |

A slot always fits in 2 bytes because a 16KiB page cannot hold 65536 slots (a slot entry is 4 bytes).

`used` is the header plus the entries. A node overflows when `used > nodeBytes`.

### API

```go
const DefaultNodeBytes = 16 * 1024

var (
    ErrDuplicateKey = errors.New("duplicate key")
    ErrKeyTooLong   = errors.New("key too long")
)

type Tree struct { /* unexported */ }

func New(nodeBytes int) (*Tree, error)       // error if nodeBytes is too small for MaxKeyLen >= 1
func (t *Tree) MaxKeyLen() int
func (t *Tree) Len() int
func (t *Tree) Insert(key []byte, id storage.RowID) error
func (t *Tree) Delete(key []byte) bool       // reports whether the key was present
func (t *Tree) Get(key []byte) (storage.RowID, bool)
func (t *Tree) Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID]
```

- `Insert` returns an error wrapping `ErrDuplicateKey` or `ErrKeyTooLong` (checked with `errors.Is`) and leaves the tree unchanged. Messages follow the repo convention: lowercase, no trailing punctuation.
- The tree copies keys on `Insert`, so it owns its bytes. Keys yielded by `Range` are valid until the next modification; copy to keep.
- `Range` is half-open `[lo, hi)`. `lo == nil` starts at the first key; `hi == nil` runs to the end. A non-nil empty `hi` is an empty range, and a non-nil empty `lo` is the same as `nil`. Inclusive upper bounds and reverse iteration are left for the wiring phase if a query needs them.
- A tree must not be modified while a `Range` is being consumed; the caller holds the table lock.
- The empty key is a valid key.

### Key length limit

A tree needs at least four entries per node to stay valid. `MaxKeyLen()` is `(nodeBytes - 16) / 4 - 8`: 4084 bytes for the default node, 12 bytes for a 96-byte test node. `New` fails if that would be less than 1.

This closes the follow-up left in the codec spec. What the engine does with a UNIQUE value longer than `MaxKeyLen()` belongs to the wiring spec; the recommendation there is to reject the row with a "key too long" error, as MySQL does.

### Insert and split

Descend by separator to the leaf, insert in key order, and add the entry's size to `used`. If `used > nodeBytes`, split at the byte midpoint: the smallest index whose cumulative bytes reach half of `used`, kept within `[1, n-1]` so both halves are non-empty. Every entry is at most a quarter of the budget, so each half fits.

- A leaf split copies the first key of the new right leaf up into the parent as its separator.
- An internal split moves the middle separator up (it is not kept in either half).
- A split of the root creates a new root.

### Delete and rebalance

Remove the key from its leaf and subtract its size from `used`. The root may be empty. A non-root node with `used < nodeBytes/2` is under-full and is handled in this order:

1. **Borrow** one entry from an adjacent sibling under the same parent if the sibling stays at or above `nodeBytes/2`, and update the parent separator.
2. **Merge** with an adjacent sibling if the combined bytes (including the pulled-down separator for internal nodes) fit in one node, and remove the separator from the parent. The parent may then be under-full and is handled the same way.
3. Otherwise leave the node as it is.

When the root is an internal node with no separators, its only child becomes the root.

### Invariants (checked in tests)

An unexported `check()` verifies after every operation: keys within a node are strictly increasing; for each separator `S`, every key in the left subtree is `< S` and every key in the right subtree is `>= S`; all leaves are at the same depth; the leaf chain visits every leaf once, in order; each node's `used` equals the sum of its entries and the header and is at most `nodeBytes`; no non-root leaf is empty and no non-root internal node is without a separator; and `Len` equals the number of leaf entries.

## Error handling

- Duplicate and over-long keys are errors from `Insert` and change nothing.
- The tree has no I/O, so no operation fails halfway. A panic would mean a broken invariant and is a bug.

## Out of scope (YAGNI)

- The on-disk node format, page ids, the page cache, and rebuilding at startup.
- Non-unique keys, composite keys, prefix compression and shortened separators.
- Bulk loading, reverse iteration, inclusive upper bounds, and iterating while modifying.
- Locking, and any engine, catalog or heap change.

## Testing and verification

Pure Go with `testify` in `internal/index`. Every test is seen failing first (against a stub that does nothing).

- **Property test:** random `Insert`, `Delete`, `Get` and `Range` against a sorted-slice model, with a fixed seed and a 96-byte node so splits, borrows and merges happen constantly, with key lengths from 0 to `MaxKeyLen()`. `check()` runs after every operation. Run once with a few thousand operations, and once with a deletion-heavy mix.
- **Directed tests:** ascending and descending bulk inserts; deleting every key leaves an empty root and a working tree; a duplicate insert and an over-long key each change nothing and match `errors.Is`; the empty key; `Range` edges (`nil` bounds, `lo == hi`, a non-nil empty `hi`, `lo` past the last key, a range crossing leaf boundaries); `Delete` of an absent key; a stale separator still gives not found and the key can be re-inserted; `New` rejects a budget that is too small.
- **Mutation checks**, each confirmed to fail a test: skip the leaf-chain update on split; copy the wrong key up on a leaf split; skip adding to `used` on insert; merge without removing the parent separator; make `Range` inclusive of `hi`.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. Node types, size accounting, `New`, `Get` and `Insert` without splitting (single leaf).
2. Splits, with `check()` and the ascending/descending tests.
3. `Range` and the leaf chain.
4. `Delete`: leaf removal, then borrow, merge and root collapse.
5. The property tests and mutation checks.

## Open questions

- Whether the borrow-then-merge rule can leave an under-full node that holds no entry. The invariants forbid it and the property test with a tiny node is the proof; if it finds a counterexample, the plan records a ruling on the tightest fix.
