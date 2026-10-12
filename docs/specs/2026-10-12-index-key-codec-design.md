# Index key codec: design

Status: draft for review. First sub-project of the B-tree index (see "Context").

## Goal

Add `internal/index` with one function that turns a column value into bytes whose `bytes.Compare` order equals the value order. The B-tree compares encoded keys only, so it needs no knowledge of column types. Nothing in this change adds a tree, and no existing behaviour changes.

## Context

The B-tree work is split into sub-projects, each with its own spec, plan and PR:

1. **Key codec** (this spec).
2. In-memory B-tree in `internal/index`.
3. Heap `File.Get(RowID)` for reading a row by its address.
4. Engine wiring: per-table unique indexes built at startup, used by uniqueness checks, point `SELECT`, `UPDATE` and `DELETE`.
5. Later: on-disk node format on `PageFile` and the page cache, index metadata in the catalog, `CREATE INDEX`, range scans.

Decisions already made for the whole effort:

- **The index is a rebuildable cache of the heap.** The heap write stays the commit point, there is no WAL, and a missing or stale index is rebuilt from the heap.
- **In memory first, disk format later.**
- **Only PRIMARY KEY and UNIQUE columns are indexed, one index per column.** Today's `checkUniqueness` already treats each unique column on its own.

The engine stores three value kinds. `kindOf` in `internal/engine/rowcodec.go` maps every column type to string (`CHAR`, `VARCHAR`, `TEXT`), int64 (`SMALLINT`, `MEDIUMINT`, `INT`, `BIGINT`) or bool, and `coerceValue` yields exactly `string`, `int64`, `bool` or `nil` for NULL.

## Decisions made during brainstorming

- **Single-column keys only.** Composite keys would need an escape for zero bytes and a terminator so a prefix sorts before its extensions. Nothing needs them yet; the parser has no multi-column constraint. Adding them later does not change single-column encodings.
- **Strings compare byte-wise and case-sensitively**, like today's `==` over Go strings. A case-insensitive collation would change which inserts are accepted.
- **No type tag.** One tree indexes one column, so all its keys have the same type. A tag costs a byte per key and buys nothing.
- **No decode function.** The tree only compares keys, and the value lives in the heap row. Add decode when something needs it.
- **NULL is an error, not a key.** NULLs are never indexed, which keeps the existing rule that a UNIQUE column may hold many NULLs.

## Design

```go
// Package index holds the B-tree index and its key encoding.
package index

// EncodeKey returns the key bytes for v, a coerced column value: int64,
// string or bool. Keys of the same type compare under bytes.Compare in the
// same order as the values.
func EncodeKey(v any) ([]byte, error)
```

| Go type | Encoding | Order it gives |
| --- | --- | --- |
| `int64` | 8 bytes, big-endian, with the sign bit flipped (`uint64(v) ^ 1<<63`) | numeric, negatives before positives |
| `string` | the string's bytes, unchanged | byte-wise; a prefix sorts before its extensions |
| `bool` | 1 byte: 0 for false, 1 for true | false before true |

- `nil` returns an error stating that NULL is not indexed.
- Any other type (`int`, `float64`, ...) returns an error naming the type. The engine never produces one, so this guards against a caller bug rather than a user input.
- Error wording follows the repo convention: lowercase, no trailing punctuation, `%T` for the offending type.
- Keys of different types are not comparable with one another and are never mixed in one tree.
- The returned slice is freshly allocated and owned by the caller.

## Known follow-up (not in this change)

There is no maximum key length. An in-memory tree does not care, but a string key can be as large as a row (up to `storage.MaxRowSize`), which cannot fit a fixed-size on-disk node with useful fan-out. The on-disk phase must decide; the likely answer is that a UNIQUE column whose values can exceed the limit falls back to scanning for that column. That decision belongs to the on-disk spec.

## Out of scope (YAGNI)

- Composite keys, collations, and decode.
- A key length limit (see above).
- The tree, `File.Get`, catalog metadata, and any engine change.

## Testing and verification

Pure Go with `testify` in `internal/index`, table-driven where there is a matrix. Every test is seen failing first (the function does not exist, so the first failure is a compile error; the first stub returns `nil, nil` so the tests fail on their assertions).

- **int64:** `MinInt64`, `-1`, `0`, `1`, `MaxInt64` encode in strictly increasing `bytes.Compare` order; each encoding is 8 bytes; a randomised pairwise check (fixed seed) confirms `a < b` implies the encoding of `a` sorts before that of `b`, and equal values encode identically.
- **string:** empty string; a prefix sorts before its extension; embedded zero bytes (`"a"`, `"a\x00"`, `"a\x00b"`, `"ab"`) keep byte order; non-UTF-8 bytes are kept unchanged; a case difference orders by bytes (`"B"` before `"a"`).
- **bool:** `false` sorts before `true`, one byte each.
- **errors:** `nil` and an unsupported type (`int`, `float64`) each return an error with the expected wording.
- **ownership:** mutating a returned key does not change a later encoding of the same string.
- **Mutation checks**, each confirmed to fail a test: drop the sign-bit flip; use little-endian for ints; map `true` and `false` to the same byte.
- `go test -race ./...`, `go vet ./...` and `gofmt -l .` stay clean.

## Build order

1. Write the tests and a stub, and watch them fail.
2. Implement `EncodeKey`.
3. Run the mutation checks.

## Open questions

None blocking.
