# Page file and page cache: design

Status: draft for review. First two of the prerequisites for a B-tree index (see "Context").

## Goal

Give `internal/storage` a page-level file API and a per-file page cache, so an index file can read and write whole 16KiB pages at random page numbers without going through the heap's slotted-row layer. Nothing in this change adds an index, and no existing behaviour or on-disk format changes.

## Context

A table read already streams: `Select` calls `File.Scan()`, which reads one page at a time and the engine decodes and filters row by row. Three real costs remain, and the index work targets the first:

- Every lookup, uniqueness check, `Update` and `Delete` reads every page (O(N)).
- `Select` buffers its whole result in `Response.Rows` (not addressed by an index).
- `readPage` allocates a fresh 16KiB buffer and calls `ReadAt` on every read, with no cache.

`storage.File` offers only `Insert`, `Delete`, `Scan`, `Sync` and `Close`. A B-tree node is not a slotted page, so an index needs random page access. This spec covers prerequisites 2 (page-level file API) and 3 (page cache) of the index plan. The others (benchmark baseline, key codec, catalog metadata, index interface, access-path selection) get their own specs.

The first 8 bytes of every page are already format-neutral: `checksum(0:4)` then `page number(4:8)`, with the checksum covering bytes 4 to the end. Bytes 8 to 12 (slot count, data start) and everything after are slotted-specific. So a page file can verify and stamp those 8 bytes for any page type and leave the rest to its owner.

## Decisions made during brainstorming

- **A separate `PageFile` type, not extra methods on `File`.** The heap file keeps owning its layout; raw pages are not exposed on it. The heap file is rebuilt on top of a `PageFile`.
- **No on-disk format change.** Existing heap files open and read exactly as before.
- **Per-file cache, write-through, immutable buffers, plain LRU.** No pins, no dirty pages, no root priority.
- **Pins are unnecessary** because Go's GC keeps a buffer alive while a reader holds it. The cache never recycles buffers, so eviction only drops the map entry.
- **A write replaces the cached frame, never mutates it.** The reason is failure atomicity: if the caller edited a cached buffer in place and the disk write then failed, the cache would hold a change that is not on disk. Callers copy a page, edit the copy and pass it to `WritePage`, which installs its own copy only after the disk write succeeds. It also makes the cache correct without relying on the table lock.
- **No root priority.** Every lookup touches the root, so LRU keeps it hot. A large range scan can flush it at the cost of one re-read. Revisit only if the benchmark baseline shows root misses, and then prefer scan-resistant eviction over hard-wiring the root.
- **Heap `Scan` bypasses the cache**, so a table scan cannot flush hot index pages. The heap's `tail` cache is left as is; folding it into the cache is a possible later cleanup.

## Design

### `PageFile` (`internal/storage/pagefile.go`)

```go
// PageFile is a file of 16KiB pages addressed by page number. It verifies and
// stamps only the format-neutral header (checksum and page number); the page
// payload belongs to the owner.
type PageFile interface {
    // ReadPage returns page n after verifying its size, checksum and page
    // number. The returned buffer is read-only: copy it to modify it.
    ReadPage(n int32) ([]byte, error)
    // WritePage stamps page number n and the checksum into a copy of buf and
    // writes it through. It does not fsync. buf must be exactly one page.
    // n may be NumPages(), which appends the page; n beyond that is an error.
    WritePage(n int32, buf []byte) error
    // AllocatePage appends an all-zero payload page with a valid header and
    // returns its number. It is for owners whose blank page is valid (the
    // index); the heap appends with WritePage instead.
    AllocatePage() (int32, error)
    NumPages() int32
    Sync() error
    Close() error
}

func CreatePageFile(path string) (PageFile, error) // O_EXCL, fsyncs the directory
func OpenPageFile(path string) (PageFile, error)   // rejects a size that is not a whole number of pages
```

- Same concurrency contract as `File`: any number of `ReadPage` calls may run concurrently (`ReadAt` is a `pread`); `WritePage`, `AllocatePage`, `Sync` and `Close` need exclusive access from the owner.
- `AllocatePage` writes a page with a stamped header and an all-zero payload instead of truncate-extending. A torn append (from `AllocatePage` or from `WritePage(NumPages(), …)`) leaves a file whose size is not a whole number of pages, which `OpenPageFile` already rejects, and `NumPages` only advances after a successful write.
- The heap cannot use `AllocatePage`: an all-zero payload is not a valid slotted page (its data start would be 0), and a separate allocate-then-write would add a second write and a crash window per new page. It appends with `WritePage(NumPages(), page)`, exactly the one write it does today.
- `ReadPage` of a number outside `[0, NumPages())` is an error naming the page and the range.
- Error wording follows the repo convention: lowercase, no trailing punctuation, `reading page %d: ...`.

### Heap file refactor

`sqlFile` holds a `PageFile` in place of its `*os.File` and `numPages`. Its `readPage`, `writePage`, `loadTail`, `Insert`, `Delete` and `Scan` keep their behaviour; `readPage` becomes `PageFile.ReadPage` followed by the existing slotted validation. `CreateFile` and `OpenFile` keep their signatures and call the page-file constructors. `Scan` keeps snapshotting the page count and reading directly, not through a cache.

One adjustment: `DecodePage` copies its input, and the heap mutates the tail page in place, so a page the heap will mutate must be its own copy. Heap `Scan` only reads, so it should validate the slotted layout without the second 16KiB copy (split `DecodePage` into a validate step and a copying wrapper); the tail and `Delete` paths keep a private copy. Otherwise the refactor would add one copy per scanned page. This is not a behaviour change.

### Page cache (`internal/storage/pagecache.go`)

```go
// NewCachedPageFile wraps inner with a write-through LRU cache of capacity pages.
func NewCachedPageFile(inner PageFile, capacity int) (PageFile, error) // capacity < 1 is an error
```

- State: `mu sync.Mutex`, `frames map[int32]*list.Element`, `lru *list.List` (front is most recent). The mutex covers the map and list only, never page contents.
- `ReadPage` hit: move to front and return the cached buffer. Miss: `inner.ReadPage`, insert, then evict from the back while over capacity. A read error caches nothing.
- `WritePage`: `inner.WritePage` first. On success, install a copy of the stamped page as the frame (replacing any existing one). On failure, drop any existing entry for that page.
- `AllocatePage`, `NumPages`, `Sync` and `Close` delegate to `inner`. `Close` also empties the cache.
- A returned buffer stays valid after eviction or replacement because buffers are never reused or mutated.
- Capacity comes from the caller. The index work will add a `storage.cache_pages` config value (default 256, 4MiB per index) when an index first uses the cache; this spec adds no config.

## Error handling

- Page-file errors wrap the OS error with `%w`, like the heap's today.
- A corrupt page (bad checksum, wrong page number) is an error from `ReadPage` and is never cached.
- A failed write leaves the cache consistent with disk: the old frame is dropped, so the next read reloads what is really there.

## Out of scope (YAGNI)

- Write-back caching, dirty pages, pins and a WAL.
- A shared pool or global memory budget.
- Priority for root or upper-level pages, scan-resistant eviction (see the TODO).
- Routing heap `Scan` through the cache (see the TODO), or replacing the heap's `tail` cache.
- Any B-tree, key codec, catalog or engine change.
- A `storage.cache_pages` config value.

## TODO (follow-ups, not part of this change)

- **Route heap `Scan` through the page cache instead of bypassing it.** Today the heap bypasses the cache so a table scan cannot flush hot index pages. The cache is per file, so the lasting benefit is that repeated scans of a small table become memory hits. Doing this needs the heap's in-place tail mutation and its `Insert`/`Delete` writes to follow the copy-then-`WritePage` rule, and a benchmark showing the gain.
- **Make eviction scan-resistant with a young and an old list** (midpoint insertion, as InnoDB does). A page first read enters the head of the old list; a second access promotes it to the young list; eviction takes from the old list's tail. A one-pass scan then cycles through the old list without displacing hot pages such as an index root. `Scan` reads each page once, so a promotion time window (InnoDB's `old_blocks_time`) is probably unnecessary, but revisit it if rows ever cause repeated page accesses. Do this together with the item above, since a scan routed through plain LRU would flush the cache.

## Testing and verification

Pure Go with `testify`, in `internal/storage`. Every test is seen failing first.

- **Page file:** create/open (existing file refused, torn size refused), allocate then read a blank page, write then read round-trip, out-of-range read and write, corrupt checksum, wrong page number, a failed write (read-only file) reported, concurrent readers under `-race`.
- **Heap refactor:** the existing `file_test.go` and `page_test.go` suites are the regression net. They pass with one change: the helper in `TestSqlFile_FailedWriteDropsTailCache` that swaps the file handle to a read-only one now swaps the page file's handle. New tests also pin the format: a heap file opens as a `PageFile` and reads back, and a page written through `PageFile` scans as heap rows.
- **Cache:** a hit does not call `inner` (counting fake `PageFile`); LRU order and eviction; a failed write drops the entry; a corrupt page is not cached; a buffer held across eviction stays intact; concurrent `ReadPage` under `-race`; `capacity < 1` is rejected.
- **Mutation checks** for the plan's test list: keep the cache write-through, then skip the drop-on-failure, then recycle a buffer, and confirm a test fails each time.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. Extract `PageFile` and move the heap file onto it (pure refactor).
2. Add the cache decorator.

## Open questions

- None blocking. Whether the heap's `tail` cache folds into the page cache is deferred until the index exists and there is a benchmark to judge it.
