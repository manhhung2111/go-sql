# Heap `File.Get(RowID)`: design

Status: draft for review. Third sub-project of the B-tree index; follows the [key codec](2026-10-12-index-key-codec-design.md) and the [in-memory B+ tree](2026-10-12-in-memory-btree-design.md).

## Goal

Let a caller read one row of a heap file by its `RowID` without scanning, so an index lookup (key to `RowID`) can fetch the row. Nothing in this change adds an index user, and no existing behaviour or on-disk format changes.

## Context

The heap already hands out addresses: `Insert` returns a `RowID{Page, Slot}`, `Delete(RowID)` takes one, and `Scan` yields rows with their ids. A `RowID` stays valid for the row's lifetime, because a delete tombstones the slot rather than shifting its neighbours. What is missing is the read: today the only way to read a row is `Scan`, which is O(file).

The index is a rebuildable cache of the heap, so an index entry can be stale. Within one file, an address stays unique for the file's lifetime (space is never reclaimed and a slot is never reused), so an entry for a deleted or updated row reliably points at a tombstone, or at a slot or page that does not exist. `Get` must let the caller tell that case apart from a damaged file, because the first means "the index is stale" and the second means "report corruption".

`ErrRowNotFound` only detects staleness within one file. `ALTER ADD/DROP COLUMN` rewrites the table into a new file packed from page 0, so an old `RowID` usually lands on a different live row there, and `Get` returns that row with no error. An index user must therefore invalidate the index whenever the table's `file_id` changes (or re-check the key against the returned row), and must not rely on `ErrRowNotFound` for that case.

## Decisions made during design

- **A method on `File`, next to `Delete`.** `Get(id RowID) ([]byte, error)`. `sqlFile` is the only real implementation. The engine's test helper `failingFile` embeds the interface, so it compiles unchanged.
- **A sentinel for "no such row".** `ErrRowNotFound` is returned, wrapped, when the page is outside `[0, NumPages)`, the slot is outside the page's slot count, or the slot is a tombstone. Everything else (I/O failure, bad checksum, wrong page number, inconsistent page layout) is a different error, so `errors.Is(err, ErrRowNotFound)` separates a stale address from a damaged file.
- **The returned bytes are a copy owned by the caller.** A view into the page buffer would be cheaper, but it keeps a whole 16KiB page alive for a row-sized result, and would alias a shared buffer once the page cache is used. The copy costs one row-sized allocation next to a 16KiB read, and the caller can keep or modify it.
- **Always read from the page file, never from the cached tail page.** The tail cache equals the disk because every write goes through, so the result is the same, and `Get` then touches no mutable state. That lets any number of `Get`s run concurrently with `Scan`s and with each other, under the table's read lock.
- **Validate the whole page layout, as `Scan` does.** `Get` reads the page through `PageFile.ReadPage` (size, checksum and page number verified) and runs `validateLayout` (O(slots), at most a few thousand). Validating only the requested slot would be cheaper; leave that until a benchmark says the cost matters.
- **Error wording is for `Get` alone.** `Delete`'s existing messages are not changed.

## Design

```go
// ErrRowNotFound is returned by Get when no live row has the given address:
// the page or slot does not exist, or the row was deleted.
var ErrRowNotFound = errors.New("row not found")

type File interface {
    // ...existing methods...

    // Get returns a copy of the row at id. It fails with an error wrapping
    // ErrRowNotFound if there is no live row there, and with another error if
    // the page cannot be read or fails verification.
    Get(id RowID) ([]byte, error)
}
```

`sqlFile.Get`:

1. If `id.Page < 0` or `id.Page >= NumPages()`, return `row not found: page %d out of range [0, %d)` wrapping `ErrRowNotFound`.
2. `buf, err := pages.ReadPage(id.Page)`; return the error as is. `validateLayout(buf)`; on failure return `page %d: %w`.
3. `row, live := (&sqlSlottedPage{buf: buf}).Row(id.Slot)`. If not live, return `row not found: page %d slot %d ...` wrapping `ErrRowNotFound`, saying "out of range [0, N)" or "is deleted".
4. Return `bytes.Clone(row)`. A live zero-length row returns a non-nil empty slice (`append([]byte(nil), row...)` would return nil for it).

Error messages follow the repo convention: lowercase, no trailing punctuation, "out of range [lo, hi)" phrasing.

Concurrency: the `File` contract gains "Get, like Scan, may run concurrently with other Gets and Scans; Insert, Delete, Sync and Close must not run concurrently with anything".

## Error handling

- A stale address is `ErrRowNotFound`; the caller (the index user) decides to rebuild.
- A corrupt page is never `ErrRowNotFound`, so it is never mistaken for a stale index.
- A negative page or slot is not found, not a panic.

## Out of scope (YAGNI)

- Using the page cache from `Get`, batch or multi-row reads, and any index or engine change.
- Changing `Scan`, `Delete` or the on-disk format.
- Validating only the requested slot.

## Testing and verification

Pure Go with `testify`, in `internal/storage`, table-driven where there is a matrix. Every test is seen failing first (against a stub that returns `nil, nil`).

- **Round trip:** rows inserted across several pages are returned by `Get` with the `RowID` `Insert` gave, for rows on the tail page and on earlier pages, and without reopening the file as well as after `OpenFile`.
- **Zero-length row:** an empty row is a live row (it takes a slot), and `Get` returns an empty non-nil slice for it.
- **Not found:** a deleted row, a slot at the slot count, a negative slot, a page at `NumPages`, a negative page, and any address in an empty file each satisfy `errors.Is(err, ErrRowNotFound)`.
- **Other rows survive:** `Get` of a neighbour of a deleted row still returns it.
- **Corruption is not "not found":** after flipping a byte in a page on disk, `Get` returns an error that does not satisfy `errors.Is(err, ErrRowNotFound)`. A second case stretches a slot's length past the page end and re-stamps the checksum, so that only the layout check can notice: the error must mention "outside row area" and must also not satisfy `ErrRowNotFound`.
- **Ownership:** modifying a returned slice does not change what the next `Get` returns.
- **Concurrency:** many `Get`s and a `Scan` at the same time pass under `-race`, on a reopened file so the tail cache is empty and a `Get` that loaded it would be caught.
- **Mutation checks**, each confirmed to fail a test: skip the tombstone check (a deleted row is returned as an empty row); skip the page-range check; return the page view instead of a copy; wrap `ErrRowNotFound` around the corruption error; wrap it around the layout error; drop the `validateLayout` call; load the tail cache inside `Get`.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. Add `ErrRowNotFound`, the `Get` method on the interface and a stub on `sqlFile`; write the tests and watch them fail.
2. Implement `Get`.
3. Run the mutation checks.

## Open questions

None blocking.
