# On-disk index snapshots Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Save a table's index trees to `<file_id>.idx` at clean shutdown and load them at the next start instead of rescanning the heap, with a protocol that makes a stale snapshot impossible to load.

**Architecture:** `internal/index` gets a pure snapshot format (`WriteSnapshot`/`ReadSnapshot` over `storage.PageFile`: a header page plus runs of sorted entries). The engine loads it in `indexSet` before falling back to `buildIndexes`, saves it in `SqlTable.Close`, and removes it durably before the first heap mutation after open. Orphan cleanup and the file-id allocator learn `.idx` and `.idx.tmp`.

**Tech Stack:** Go 1.26.4, testify, `storage.PageFile`, the existing `index.Tree`.

**Spec:** `docs/specs/2026-10-15-on-disk-index-design.md` (committed on the same branch).

## Global Constraints

- Lock order is unchanged: `Catalog.mu` → `Database.mu` → `Table.mu` → `tableIndexes.mu` → `catalogStore.mu`. Snapshot code runs under `Table.mu` (and `tableIndexes.mu` while loading) and takes no other lock.
- A snapshot is a cache. Every failure to read or save one is invisible except in speed, with one exception: a failure to remove a snapshot before a heap write fails the statement before anything is written.
- Error messages: lowercase, no trailing punctuation, `%q` for names.
- Tests are testify, one behavior each. Run `go test -race ./...`, `go vet ./...` and `gofmt -l .` before every commit.
- The suite runs with `rangeCostFactor` 0 (`TestMain`); `withCostFactor` and `shippedRangeCostFactor` exist.
- Never push; commit per task with the Conventional Commit messages given. End commit messages with the `Co-Authored-By` trailer the harness gives.

## Review Focus

- A write that never built the trees (e.g. a DELETE with a non-key WHERE, or an UPDATE) must still remove the snapshot first: otherwise the next start loads a stale one.
- A snapshot that failed to load and was removed (not durably) must not stop the first write from removing the file and fsyncing the directory.
- A rejected statement (duplicate key, bad value) and a DELETE matching nothing must not touch the snapshot.
- A leftover `.idx` for an id with no `.tbl` must never be reused by the allocator for a new table.
- A table whose descriptors do not fit the header page, a table with no trees, a table whose trees were never built, and a degraded (nil) tree all save and load, or skip, without error.

---

### Task 1: The snapshot format

**Files:**
- Modify: `internal/storage/pagefile.go` (two exported constants)
- Create: `internal/index/snapshot.go`, `internal/index/snapshot_test.go`

**Interfaces:**
- Produces: `storage.PageSize` (16384) and `storage.PageHeaderSize` (8, the checksum and page number `PageFile` stamps); `index.TreeKind` with `UniqueTree`, `SecondaryTree`; `index.TreeID{Kind TreeKind; Ordinal int; Name string; Column string}`; `index.SnapshotTree{ID TreeID; Tree *Tree}` (`Tree` nil means degraded); `index.WriteSnapshot(path string, heapID int64, heapPages int32, trees []SnapshotTree) error`; `index.ReadSnapshot(path string, heapID int64, heapPages int32, want []TreeID) ([]SnapshotTree, error)` returning trees in `want` order; `index.ErrSnapshotMismatch`, `index.ErrSnapshotTooLarge`.
- Consumes: `index.New`, `Tree.Insert`, `Tree.Range`, `Tree.Len`, `storage.CreatePageFile`, `storage.OpenPageFile`, `storage.SyncDir`.

- [ ] **Step 1: Constants in storage**

In `internal/storage/pagefile.go`, after the imports, add:

```go
const (
	// PageSize is the size of every page of a PageFile.
	PageSize = maxPageSize
	// PageHeaderSize is the checksum and page number a PageFile stamps; the
	// owner's layout starts after it.
	PageHeaderSize = pageNumberPos + 4
)
```

Run: `go build ./...` — Expected: builds.

- [ ] **Step 2: Failing tests for the format**

Create `internal/index/snapshot_test.go`:

```go
package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

func snapEntries(tree *Tree) []string {
	out := []string{}
	if tree == nil {
		return out
	}
	for key, id := range tree.Range(nil, nil) {
		out = append(out, fmt.Sprintf("%x=%d/%d", key, id.Page, id.Slot))
	}
	return out
}

// snapTree builds a tree of n keys of the given byte length, in an order other
// than sorted, with row ids that depend on the key.
func snapTree(t *testing.T, n, keyLen int) *Tree {
	t.Helper()
	tree, err := New(DefaultNodeBytes)
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		j := (i * 7919) % n
		key := []byte(fmt.Sprintf("%0*d", keyLen, j))
		require.NoError(t, tree.Insert(key, storage.RowID{Page: int32(j / 10), Slot: j % 10}))
	}
	return tree
}

var (
	idPK     = TreeID{Kind: UniqueTree, Ordinal: 0, Column: "id"}
	idEmail  = TreeID{Kind: UniqueTree, Ordinal: 1, Column: "email"}
	idByName = TreeID{Kind: SecondaryTree, Name: "by_name", Column: "name"}
)

func snapPath(t *testing.T) string { return filepath.Join(t.TempDir(), "7.idx") }

// patchPage rewrites page n of the snapshot through the page file, which
// re-stamps the checksum, after change has edited its payload.
func patchPage(t *testing.T, path string, n int32, change func(payload []byte)) {
	t.Helper()
	pf, err := storage.OpenPageFile(path)
	require.NoError(t, err)
	page, err := pf.ReadPage(n)
	require.NoError(t, err)
	buf := append([]byte(nil), page...)
	change(buf[storage.PageHeaderSize:])
	require.NoError(t, pf.WritePage(n, buf))
	require.NoError(t, pf.Sync())
	require.NoError(t, pf.Close())
}

func writeSample(t *testing.T, path string) []SnapshotTree {
	t.Helper()
	trees := writeSampleTrees(t)
	require.NoError(t, WriteSnapshot(path, 7, 42, trees))
	return trees
}

func TestSnapshot_RoundTripsEveryKindOfTree(t *testing.T) {
	path := snapPath(t)
	empty, err := New(DefaultNodeBytes)
	require.NoError(t, err)
	idEmpty := TreeID{Kind: SecondaryTree, Name: "empty", Column: "x"}
	trees := append(writeSampleTrees(t), SnapshotTree{ID: idEmpty, Tree: empty})
	require.NoError(t, WriteSnapshot(path, 7, 42, trees))

	got, err := ReadSnapshot(path, 7, 42, []TreeID{idByName, idEmpty, idPK, idEmail})

	require.NoError(t, err)
	require.Len(t, got, 4, "in the order asked for")
	assert.Equal(t, []TreeID{idByName, idEmpty, idPK, idEmail}, []TreeID{got[0].ID, got[1].ID, got[2].ID, got[3].ID})
	assert.Equal(t, snapEntries(trees[2].Tree), snapEntries(got[0].Tree))
	require.NotNil(t, got[1].Tree, "an empty tree stays a tree")
	assert.Equal(t, 0, got[1].Tree.Len())
	assert.Equal(t, snapEntries(trees[0].Tree), snapEntries(got[2].Tree))
	assert.Equal(t, 3000, got[2].Tree.Len())
	assert.Nil(t, got[3].Tree, "a degraded tree stays degraded")
}

// writeSampleTrees is a unique tree spanning several pages, a degraded one and
// a small secondary tree.
func writeSampleTrees(t *testing.T) []SnapshotTree {
	t.Helper()
	return []SnapshotTree{
		{ID: idPK, Tree: snapTree(t, 3000, 100)},
		{ID: idEmail, Tree: nil},
		{ID: idByName, Tree: snapTree(t, 5, 3)},
	}
}

func TestSnapshot_SpansManyPages(t *testing.T) {
	path := snapPath(t)
	writeSample(t, path)

	pf, err := storage.OpenPageFile(path)
	require.NoError(t, err)
	defer pf.Close()

	assert.Greater(t, pf.NumPages(), int32(10), "3000 entries of 100 bytes do not fit a few pages")
}

func TestSnapshot_WriteLeavesNoTempFileAndReplacesAnOldOne(t *testing.T) {
	path := snapPath(t)
	require.NoError(t, os.WriteFile(path+".tmp", []byte("junk"), 0o644)) // a crashed earlier save
	writeSample(t, path)
	small := []SnapshotTree{{ID: idPK, Tree: snapTree(t, 2, 3)}}

	require.NoError(t, WriteSnapshot(path, 7, 42, small))

	_, err := os.Stat(path + ".tmp")
	assert.ErrorIs(t, err, os.ErrNotExist)
	got, err := ReadSnapshot(path, 7, 42, []TreeID{idPK})
	require.NoError(t, err)
	assert.Equal(t, 2, got[0].Tree.Len(), "the new content replaced the old")
}

func TestSnapshot_NoTreesIsAValidFile(t *testing.T) {
	path := snapPath(t)

	require.NoError(t, WriteSnapshot(path, 1, 1, nil))
	got, err := ReadSnapshot(path, 1, 1, nil)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSnapshot_TooManyDescriptorsFailsAndLeavesNothing(t *testing.T) {
	path := snapPath(t)
	var trees []SnapshotTree
	for i := 0; i < 2000; i++ {
		trees = append(trees, SnapshotTree{ID: TreeID{Kind: SecondaryTree, Name: fmt.Sprintf("index_number_%d", i), Column: "c"}})
	}

	err := WriteSnapshot(path, 1, 1, trees)

	assert.ErrorIs(t, err, ErrSnapshotTooLarge)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(path + ".tmp")
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSnapshot_ReadRejectsWhatDoesNotMatch(t *testing.T) {
	want := []TreeID{idPK, idEmail, idByName}
	tests := []struct {
		name     string
		heapID   int64
		pages    int32
		want     []TreeID
		mismatch bool
	}{
		{"another heap file", 8, 42, want, true},
		{"another page count", 7, 43, want, true},
		{"a tree missing from the table", 7, 42, []TreeID{idPK, idEmail}, true},
		{"a tree the file does not have", 7, 42, append(append([]TreeID{}, want...), TreeID{Kind: SecondaryTree, Name: "new", Column: "x"}), true},
		{"an index renamed", 7, 42, []TreeID{idPK, idEmail, {Kind: SecondaryTree, Name: "by_other", Column: "name"}}, true},
		{"a column renamed", 7, 42, []TreeID{idPK, idEmail, {Kind: SecondaryTree, Name: "by_name", Column: "label"}}, true},
		{"a unique column at another position", 7, 42, []TreeID{{Kind: UniqueTree, Ordinal: 3, Column: "id"}, idEmail, idByName}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := snapPath(t)
			writeSample(t, path)

			_, err := ReadSnapshot(path, tc.heapID, tc.pages, tc.want)

			assert.ErrorIs(t, err, ErrSnapshotMismatch)
		})
	}
}

func TestSnapshot_ReadRejectsDamage(t *testing.T) {
	want := []TreeID{idPK, idEmail, idByName}
	tests := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"a missing file", func(t *testing.T, path string) { require.NoError(t, os.Remove(path)) }},
		{"a flipped byte", func(t *testing.T, path string) {
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			b[2*storage.PageSize+100] ^= 0xFF
			require.NoError(t, os.WriteFile(path, b, 0o644))
		}},
		{"a torn file", func(t *testing.T, path string) {
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, b[:len(b)-1], 0o644))
		}},
		{"a bad magic", func(t *testing.T, path string) {
			patchPage(t, path, 0, func(p []byte) { p[hdrMagic] = 'X' })
		}},
		{"an unknown version", func(t *testing.T, path string) {
			patchPage(t, path, 0, func(p []byte) { p[hdrVersion] = 99 })
		}},
		{"entries out of order", func(t *testing.T, path string) {
			// Page 1's payload is the count (2 bytes), then entries of key
			// length (2), the 100-byte key, page (4) and slot (2). The first
			// two keys differ only in their last digit; swap those digits.
			patchPage(t, path, 1, func(p []byte) {
				first := 2 + 2
				second := first + 100 + 4 + 2 + 2
				p[first+99], p[second+99] = p[second+99], p[first+99]
			})
		}},
		{"a trailing page", func(t *testing.T, path string) {
			pf, err := storage.OpenPageFile(path)
			require.NoError(t, err)
			_, err = pf.AllocatePage()
			require.NoError(t, err)
			require.NoError(t, pf.Sync())
			require.NoError(t, pf.Close())
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := snapPath(t)
			writeSample(t, path)
			tc.damage(t, path)

			_, err := ReadSnapshot(path, 7, 42, want)

			assert.Error(t, err)
			assert.NotErrorIs(t, err, ErrSnapshotMismatch, "damage is not a mismatch")
		})
	}
}

func TestSnapshot_ReadRejectsAWrongEntryCount(t *testing.T) {
	path := snapPath(t)
	writeSample(t, path)
	patchPage(t, path, 0, func(p []byte) {
		// the first descriptor: kind(1) ordinal(4) degraded(1) then count(4)
		count := hdrDescriptors + 1 + 4 + 1
		p[count]++
	})

	_, err := ReadSnapshot(path, 7, 42, []TreeID{idPK, idEmail, idByName})

	assert.Error(t, err)
}
```

Add a stub to a new `internal/index/snapshot.go` so the test compiles:

```go
package index

import "errors"

type TreeKind uint8

const (
	UniqueTree TreeKind = iota + 1
	SecondaryTree
)

type TreeID struct {
	Kind    TreeKind
	Ordinal int
	Name    string
	Column  string
}

type SnapshotTree struct {
	ID   TreeID
	Tree *Tree
}

var (
	ErrSnapshotMismatch = errors.New("snapshot does not match")
	ErrSnapshotTooLarge = errors.New("snapshot header too large")
)

const (
	hdrMagic       = 0
	hdrVersion     = 4
	hdrHeapID      = 6
	hdrHeapPages   = 14
	hdrTreeCount   = 18
	hdrDescriptors = 20
)

func WriteSnapshot(path string, heapID int64, heapPages int32, trees []SnapshotTree) error {
	return nil
}

func ReadSnapshot(path string, heapID int64, heapPages int32, want []TreeID) ([]SnapshotTree, error) {
	return nil, nil
}
```

Run: `go test ./internal/index/ -run 'TestSnapshot_' 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)'`
Expected: FAIL on assertions (nothing is written or read), not build errors.

- [ ] **Step 3: Implement the format**

Replace `internal/index/snapshot.go` with:

```go
package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"manhhung2111/go-sql/internal/storage"
)

// A snapshot is a storage.PageFile holding a table's trees as sorted entries.
// Page 0 is the header, after the page file's own header:
//
//	magic "GSIX" (4) | version u16 | heap file id i64 | heap pages u32 |
//	tree count u16 | one descriptor per tree
//
// A descriptor is: kind u8 | ordinal u32 | degraded u8 | entry count u32 |
// first page u32 | name (u16 length, bytes) | column (u16 length, bytes).
//
// Entry pages follow, the trees one after another in descriptor order. An entry
// page is: count u16, then that many entries of key (u16 length, bytes) | row
// page u32 | row slot u16. All integers are little-endian.
const (
	snapshotMagic   = "GSIX"
	snapshotVersion = 1

	hdrMagic       = 0
	hdrVersion     = 4
	hdrHeapID      = 6
	hdrHeapPages   = 14
	hdrTreeCount   = 18
	hdrDescriptors = 20
)

// TreeKind says which of a table's trees a snapshot tree is.
type TreeKind uint8

const (
	// UniqueTree is the tree of a PRIMARY KEY/UNIQUE column.
	UniqueTree TreeKind = iota + 1
	// SecondaryTree is the tree of a CREATE INDEX definition.
	SecondaryTree
)

// TreeID names a tree of a table. Two snapshots hold the same trees exactly when
// their IDs are equal as a set.
type TreeID struct {
	Kind    TreeKind
	Ordinal int    // UniqueTree: the column's schema index
	Name    string // SecondaryTree: the index name
	Column  string // the column's name
}

// SnapshotTree is a tree and its identity. A nil Tree is a degraded one: a
// column or index that could not hold the heap's values.
type SnapshotTree struct {
	ID   TreeID
	Tree *Tree
}

var (
	// ErrSnapshotMismatch means the file is a valid snapshot of something else:
	// another heap file, another page count or another set of trees.
	ErrSnapshotMismatch = errors.New("snapshot does not match")
	// ErrSnapshotTooLarge means the descriptors do not fit the header page.
	ErrSnapshotTooLarge = errors.New("snapshot header too large")
)

// WriteSnapshot saves trees to path for the heap file heapID of heapPages
// pages. It writes path+".tmp", fsyncs it, renames it over path and fsyncs the
// directory, so path is either what it was or complete.
func WriteSnapshot(path string, heapID int64, heapPages int32, trees []SnapshotTree) (err error) {
	tmp := path + ".tmp"
	if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return rmErr
	}
	pf, err := storage.CreatePageFile(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = pf.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = pf.AllocatePage(); err != nil { // page 0, rewritten last
		return err
	}

	header := make([]byte, storage.PageSize)
	payload := header[storage.PageHeaderSize:]
	copy(payload[hdrMagic:], snapshotMagic)
	binary.LittleEndian.PutUint16(payload[hdrVersion:], snapshotVersion)
	binary.LittleEndian.PutUint64(payload[hdrHeapID:], uint64(heapID))
	binary.LittleEndian.PutUint32(payload[hdrHeapPages:], uint32(heapPages))
	binary.LittleEndian.PutUint16(payload[hdrTreeCount:], uint16(len(trees)))
	pos := hdrDescriptors
	for _, st := range trees {
		first, count, werr := writeEntries(pf, st.Tree)
		if werr != nil {
			return werr
		}
		need := 1 + 4 + 1 + 4 + 4 + 2 + len(st.ID.Name) + 2 + len(st.ID.Column)
		if pos+need > len(payload) || len(trees) > 1<<16-1 {
			return ErrSnapshotTooLarge
		}
		payload[pos] = byte(st.ID.Kind)
		binary.LittleEndian.PutUint32(payload[pos+1:], uint32(st.ID.Ordinal))
		if st.Tree == nil {
			payload[pos+5] = 1
		}
		binary.LittleEndian.PutUint32(payload[pos+6:], uint32(count))
		binary.LittleEndian.PutUint32(payload[pos+10:], uint32(first))
		pos += 14
		pos = putString(payload, pos, st.ID.Name)
		pos = putString(payload, pos, st.ID.Column)
	}
	if err = pf.WritePage(0, header); err != nil {
		return err
	}
	if err = pf.Sync(); err != nil {
		return err
	}
	if err = pf.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return storage.SyncDir(filepath.Dir(path))
}

func putString(b []byte, pos int, s string) int {
	binary.LittleEndian.PutUint16(b[pos:], uint16(len(s)))
	return pos + 2 + copy(b[pos+2:], s)
}

// writeEntries appends tree's entries to pf as entry pages and returns the page
// number of the first and the number of entries. A nil or empty tree writes
// no page.
func writeEntries(pf storage.PageFile, tree *Tree) (first int32, count int, err error) {
	if tree == nil {
		return 0, 0, nil
	}
	buf := make([]byte, storage.PageSize)
	pos, inPage := storage.PageHeaderSize+2, 0
	flush := func() error {
		if inPage == 0 {
			return nil
		}
		binary.LittleEndian.PutUint16(buf[storage.PageHeaderSize:], uint16(inPage))
		n := pf.NumPages()
		if first == 0 {
			first = n
		}
		if err := pf.WritePage(n, buf); err != nil {
			return err
		}
		clear(buf)
		pos, inPage = storage.PageHeaderSize+2, 0
		return nil
	}
	for key, id := range tree.Range(nil, nil) {
		if pos+2+len(key)+4+2 > len(buf) {
			if err := flush(); err != nil {
				return 0, 0, err
			}
		}
		binary.LittleEndian.PutUint16(buf[pos:], uint16(len(key)))
		pos += 2
		pos += copy(buf[pos:], key)
		binary.LittleEndian.PutUint32(buf[pos:], uint32(id.Page))
		binary.LittleEndian.PutUint16(buf[pos+4:], uint16(id.Slot))
		pos += 6
		inPage++
		count++
	}
	if err := flush(); err != nil {
		return 0, 0, err
	}
	return first, count, nil
}

// reader reads little-endian values from a page, remembering the first time it
// ran past the end.
type reader struct {
	b   []byte
	pos int
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || n < 0 || r.pos+n > len(r.b) {
		r.bad = true
		return nil
	}
	out := r.b[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *reader) u8() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() int {
	if b := r.take(2); b != nil {
		return int(binary.LittleEndian.Uint16(b))
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *reader) str() string { return string(r.take(r.u16())) }

type descriptor struct {
	id       TreeID
	degraded bool
	count    uint32
	first    uint32
}

// ReadSnapshot loads the trees of path, which must be a snapshot of heap file
// heapID with heapPages pages holding exactly the trees want names. It returns
// them in want's order. A damaged file is an error; a valid snapshot of
// something else wraps ErrSnapshotMismatch.
func ReadSnapshot(path string, heapID int64, heapPages int32, want []TreeID) ([]SnapshotTree, error) {
	pf, err := storage.OpenPageFile(path)
	if err != nil {
		return nil, err
	}
	defer pf.Close()
	bad := func(format string, args ...any) error {
		return fmt.Errorf("snapshot %q: %s", path, fmt.Sprintf(format, args...))
	}
	if pf.NumPages() < 1 {
		return nil, bad("no header page")
	}
	page, err := pf.ReadPage(0)
	if err != nil {
		return nil, err
	}
	hdr := page[storage.PageHeaderSize:]
	if string(hdr[hdrMagic:hdrMagic+4]) != snapshotMagic {
		return nil, bad("bad magic")
	}
	if v := binary.LittleEndian.Uint16(hdr[hdrVersion:]); v != snapshotVersion {
		return nil, bad("unsupported version %d", v)
	}
	if got := int64(binary.LittleEndian.Uint64(hdr[hdrHeapID:])); got != heapID {
		return nil, fmt.Errorf("%w: it is for heap file %d, not %d", ErrSnapshotMismatch, got, heapID)
	}
	if got := int32(binary.LittleEndian.Uint32(hdr[hdrHeapPages:])); got != heapPages {
		return nil, fmt.Errorf("%w: the heap had %d pages, it has %d", ErrSnapshotMismatch, got, heapPages)
	}
	n := int(binary.LittleEndian.Uint16(hdr[hdrTreeCount:]))
	if n != len(want) {
		return nil, fmt.Errorf("%w: %d trees, want %d", ErrSnapshotMismatch, n, len(want))
	}

	r := reader{b: hdr, pos: hdrDescriptors}
	descs := make([]descriptor, n)
	wanted := make(map[TreeID]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}
	seen := make(map[TreeID]bool, n)
	for i := range descs {
		d := &descs[i]
		d.id.Kind = TreeKind(r.u8())
		d.id.Ordinal = int(r.u32())
		d.degraded = r.u8() == 1
		d.count = r.u32()
		d.first = r.u32()
		d.id.Name = r.str()
		d.id.Column = r.str()
		if r.bad {
			return nil, bad("damaged header")
		}
		if !wanted[d.id] || seen[d.id] {
			return nil, fmt.Errorf("%w: tree %+v", ErrSnapshotMismatch, d.id)
		}
		seen[d.id] = true
	}

	trees := make(map[TreeID]*Tree, n)
	next := uint32(1)
	for _, d := range descs {
		if d.degraded {
			if d.count != 0 {
				return nil, bad("a degraded tree with entries")
			}
			trees[d.id] = nil
			continue
		}
		tree, err := New(DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		trees[d.id] = tree
		if d.count == 0 {
			continue
		}
		if d.first != next {
			return nil, bad("tree %+v starts at page %d, want %d", d.id, d.first, next)
		}
		remaining := int(d.count)
		var prev []byte
		havePrev := false
		for remaining > 0 {
			if int32(next) >= pf.NumPages() {
				return nil, bad("entries run past the end")
			}
			buf, err := pf.ReadPage(int32(next))
			if err != nil {
				return nil, err
			}
			next++
			er := reader{b: buf, pos: storage.PageHeaderSize}
			k := er.u16()
			if k == 0 || k > remaining {
				return nil, bad("page %d holds %d entries, %d remain", next-1, k, remaining)
			}
			for i := 0; i < k; i++ {
				key := er.take(er.u16())
				pg, sl := er.u32(), er.u16()
				if er.bad {
					return nil, bad("damaged entry page %d", next-1)
				}
				if havePrev && bytes.Compare(prev, key) >= 0 {
					return nil, bad("entries out of order on page %d", next-1)
				}
				if err := tree.Insert(key, storage.RowID{Page: int32(pg), Slot: sl}); err != nil {
					return nil, bad("page %d: %v", next-1, err)
				}
				prev, havePrev = key, true
			}
			remaining -= k
		}
	}
	if int32(next) != pf.NumPages() {
		return nil, bad("%d trailing pages", pf.NumPages()-int32(next))
	}

	out := make([]SnapshotTree, len(want))
	for i, w := range want {
		out[i] = SnapshotTree{ID: w, Tree: trees[w]}
	}
	return out, nil
}
```

Run: `go test -race ./internal/index/ 2>&1 | tail -20`
Expected: PASS. The sortedness mutation of Step 5 proves the out-of-order test fails for the right reason.

- [ ] **Step 4: Whole suite**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean.

- [ ] **Step 5: Mutation checks**

Commit first, then for each: edit, run `go test ./internal/index/ 2>&1 | grep -E '^(--- FAIL|ok)' | head`, confirm a failure, `git checkout internal`.

1. In `ReadSnapshot`, delete the `bytes.Compare(prev, key) >= 0` check.
2. Delete the heap page count comparison.
3. Delete the heap id comparison.
4. Delete the `!wanted[d.id] || seen[d.id]` check (replace its condition with `false`).
5. Delete the trailing-page check.
6. In `WriteSnapshot`, rename without the `tmp` step: write straight to `path` (`tmp := path`) — the temp-file test must fail.
7. In `writeEntries`, drop the `clear(buf)` after a flush.

A mutant that survives means a missing test: add one and rerun.

```bash
git add internal/storage internal/index
git commit -m "feat(index): add the on-disk snapshot format for index trees

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Load and save in the engine, and the file ids

**Files:**
- Create: `internal/engine/snapshot.go`, `internal/engine/snapshot_test.go`
- Modify: `internal/engine/tableindex.go` (`indexSet`), `internal/engine/table.go` (`Close`, the table struct), `internal/engine/database.go` (loaded tables), `internal/engine/files.go` (`fileIDOf`, `maxTableFileID`), `internal/engine/catalogcleanup.go`

**Interfaces:**
- Consumes: Task 1's `index.WriteSnapshot`, `index.ReadSnapshot`, `index.TreeID`, `index.SnapshotTree`; Part B's `treeSet`, `indexSet`, `builtSet`, `keyColumns`, `indexDefs`.
- Produces: `snapshotExt = ".idx"`; `snapshotPathFor(heapPath string) string`; `(*SqlTable).snapshotPath() string`; `(*SqlTable).treeIDs() []index.TreeID`; `(*SqlTable).loadSnapshot() *treeSet`; `(*SqlTable).saveSnapshot()`; field `SqlTable.snapshotMayExist bool` (true only for a table loaded from the catalog); `fileIDOf(name string) (int64, bool)`; test helpers `tableOf(t, c, db, table) *SqlTable`.

- [ ] **Step 1: Failing tests: the allocator and the orphan cleanup know snapshots**

Create `internal/engine/snapshot_test.go`:

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// names lists the files of a database directory, sorted.
func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func touch(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
}

func TestFileIDOf(t *testing.T) {
	tests := []struct {
		name string
		id   int64
		ok   bool
	}{
		{"12.tbl", 12, true},
		{"12.idx", 12, true},
		{"12.idx.tmp", 12, true},
		{"x.idx", 0, false},
		{"12.tbl.idx", 0, false},
		{"12.txt", 0, false},
		{".idx", 0, false},
		{"-3.idx", 0, false},
	}
	for _, tc := range tests {
		id, ok := fileIDOf(tc.name)
		assert.Equal(t, tc.ok, ok, tc.name)
		assert.Equal(t, tc.id, id, tc.name)
	}
}

func TestAllocator_NeverReusesTheIdOfALeftoverSnapshot(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "data", "shop", "3.tbl"))
	touch(t, filepath.Join(dir, "data", "shop", "9.idx"))

	files, err := newFileAllocator(DataDir(dir))

	require.NoError(t, err)
	assert.EqualValues(t, 10, files.nextFileID())
}

func TestRemoveOrphans_HandlesSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	shop := filepath.Join(dir, "data", "shop")
	for _, name := range []string{"5.tbl", "5.idx", "5.idx.tmp", "7.tbl", "7.idx", "8.idx", "8.idx.tmp", "x.idx", "9.idx.bak"} {
		touch(t, filepath.Join(shop, name))
	}
	files := &fileAllocator{dataDir: DataDir(dir)}

	removeOrphans(files, map[string]bool{"shop": true}, map[int64]bool{5: true})

	assert.Equal(t, []string{"5.idx", "5.idx.tmp", "5.tbl", "9.idx.bak", "x.idx"}, namesIn(t, shop),
		"a referenced table keeps its files (the .tmp of a crashed save is cleaned by the next save); unreferenced ids go, and so do names that are not <number> plus a known suffix")
}
```

(The expectation keeps `5.idx.tmp`: it belongs to a referenced id, and a live table's leftover `.tmp` is replaced by its next save.)

Run: `go test ./internal/engine/ -run 'TestFileIDOf|TestAllocator_NeverReuses|TestRemoveOrphans_HandlesSnapshot' 2>&1 | head -20`
Expected: a build error on `fileIDOf`. Add the stub `func fileIDOf(name string) (int64, bool) { return 0, false }` to `files.go` and rerun: FAIL on assertions.

- [ ] **Step 2: Implement `fileIDOf` and use it**

In `internal/engine/files.go` add:

```go
const (
	snapshotExt    = ".idx"
	snapshotTmpExt = ".idx.tmp"
)

// fileIDOf returns the id in the name of a file the engine creates for a table:
// <id>.tbl, <id>.idx or <id>.idx.tmp. Anything else is not ours.
func fileIDOf(name string) (int64, bool) {
	for _, ext := range []string{tableFileExt, snapshotTmpExt, snapshotExt} {
		if text, ok := strings.CutSuffix(name, ext); ok {
			id, err := strconv.ParseInt(text, 10, 64)
			if err != nil || id < 0 {
				return 0, false
			}
			return id, true
		}
	}
	return 0, false
}
```

(Replace the stub.) In `maxTableFileID`, replace

```go
			name, isTableFile := strings.CutSuffix(file.Name(), tableFileExt)
			if !isTableFile {
				continue
			}
			if id, err := strconv.ParseInt(name, 10, 64); err == nil && id > maxID {
				maxID = id
			}
```

with

```go
			if id, ok := fileIDOf(file.Name()); ok && id > maxID {
				maxID = id
			}
```

and update its doc comment ("the highest <id> of a table file or leftover snapshot"). In `catalogcleanup.go` replace the parsing in the loop:

```go
			idText, isTable := strings.CutSuffix(child.Name(), tableFileExt)
			id, parseErr := strconv.ParseInt(idText, 10, 64)
			if !isTable || parseErr != nil || referenced[id] {
```

with

```go
			id, isOurs := fileIDOf(child.Name())
			if !isOurs || referenced[id] {
```

drop the now-unused `strconv`/`strings` imports, and update the doc comment ("Only files named <number>.tbl, <number>.idx or <number>.idx.tmp are removed").

Run: `go test -race ./internal/engine/ -run 'TestFileIDOf|TestAllocator_NeverReuses|TestRemoveOrphans' 2>&1 | tail` — Expected: PASS (including the existing orphan tests).

- [ ] **Step 3: Failing restart tests**

Append to `internal/engine/snapshot_test.go` (add imports `strings` if needed, `fmt`):

```go
func tableOf(t *testing.T, c Catalog, database, name string) *SqlTable {
	t.Helper()
	db, err := c.GetDatabase(database)
	require.NoError(t, err)
	table, ok := db.GetTable(name)
	require.True(t, ok)
	return table.(*SqlTable)
}

// eventsEngine is a catalog over dir with shop.events(id INT, kind VARCHAR(20))
// and CREATE INDEX by_kind, holding n rows with kinds k0..k4.
func eventsEngine(t *testing.T, dir string, n int) (Catalog, Engine) {
	t.Helper()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE events (id INT, kind VARCHAR(20))")
	mustExec(t, e, "shop", "CREATE INDEX by_kind ON events (kind)")
	for i := 0; i < n; i++ {
		mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO events (id, kind) VALUES (%d, 'k%d')", i, i%5))
	}
	return c, e
}

const kindQuery = "SELECT id FROM events WHERE kind = 'k3'"

func TestSnapshot_ARestartLoadsTheTreesWithoutScanning(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	want := mustExec(t, e, "shop", kindQuery).Rows // builds the trees
	require.NotEmpty(t, want)
	path := tableOf(t, c, "shop", "events").snapshotPath()

	c, e = restart(t, dir, c)

	assert.FileExists(t, path, "a clean shutdown saved the trees")
	table := tableOf(t, c, "shop", "events")
	cf := countOn(table)
	assert.Equal(t, want, mustExec(t, e, "shop", kindQuery).Rows)
	assert.EqualValues(t, 0, cf.scans.Load(), "the heap was not read to build the trees")
	assert.EqualValues(t, 1, cf.getManyCalls.Load(), "and the lookup used them")
	assertIndexesMatchHeap(t, table)
}

func TestSnapshot_UniqueColumnsAreLoadedToo(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE users (id INT PRIMARY KEY, email VARCHAR(40) UNIQUE)")
	for i := 0; i < 20; i++ {
		mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO users (id, email) VALUES (%d, 'u%d@x.com')", i, i))
	}
	mustExec(t, e, "shop", "SELECT id FROM users WHERE id = 3")

	c, e = restart(t, dir, c)

	cf := countOn(tableOf(t, c, "shop", "users"))
	assert.Equal(t, [][]string{{"7"}}, mustExec(t, e, "shop", "SELECT id FROM users WHERE email = 'u7@x.com'").Rows)
	assert.EqualValues(t, 0, cf.scans.Load())
	err := execError(t, e, "shop", "INSERT INTO users (id, email) VALUES (3, 'new@x.com')")
	assert.ErrorContains(t, err, "duplicate", "the loaded tree still enforces the key")
}

func TestSnapshot_ARestartThatUsesNothingKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 10)
	mustExec(t, e, "shop", kindQuery)
	path := tableOf(t, c, "shop", "events").snapshotPath()
	c, _ = restart(t, dir, c)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, _ = restart(t, dir, c) // the table was never used in the second run

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "nothing was built, so nothing was saved over it")
}

func TestSnapshot_ADamagedFileFallsBackToTheScanAndIsRemoved(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	want := mustExec(t, e, "shop", kindQuery).Rows
	path := tableOf(t, c, "shop", "events").snapshotPath()
	c, e = restart(t, dir, c)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	b[len(b)-100] ^= 0xFF
	require.NoError(t, os.WriteFile(path, b, 0o644))
	cf := countOn(tableOf(t, c, "shop", "events"))

	assert.Equal(t, want, mustExec(t, e, "shop", kindQuery).Rows)

	assert.EqualValues(t, 1, cf.scans.Load(), "the trees were rebuilt")
	assert.NoFileExists(t, path)
}

func TestSnapshot_ARenamedColumnIsNotTrustedAfterACrash(t *testing.T) {
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	mustExec(t, e, "shop", kindQuery)
	c, e = restart(t, dir, c) // saved with the column called kind
	mustExec(t, e, "shop", "ALTER TABLE events RENAME COLUMN kind TO label")
	c = newTestCatalogAt(t, dir) // the old catalog is not closed: a crash
	e = NewEngine(c)
	cf := countOn(tableOf(t, c, "shop", "events"))

	rows := mustExec(t, e, "shop", "SELECT id FROM events WHERE label = 'k3'").Rows

	assert.Len(t, rows, 6)
	assert.EqualValues(t, 1, cf.scans.Load(), "the saved tree set no longer matches, so it was rebuilt")
}
```

Add stubs to `internal/engine/snapshot.go` so it compiles: `const snapshotExt` already exists in files.go; add

```go
package engine

import "strings"

func snapshotPathFor(heapPath string) string {
	return strings.TrimSuffix(heapPath, tableFileExt) + snapshotExt
}

func (t *SqlTable) snapshotPath() string { return snapshotPathFor(t.path) }
```

Run: `go test ./internal/engine/ -run 'TestSnapshot_' 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)'`
Expected: FAIL on assertions (no file exists, scans are 1), not build errors. If `eventsEngine` fails because CREATE INDEX or the INSERT syntax is rejected, fix the helper, not the production code.

- [ ] **Step 4: Implement load and save**

Replace `internal/engine/snapshot.go` with:

```go
package engine

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/storage"
)

// These are variables so a test can make the removal of a snapshot fail.
var (
	removeSnapshotFile = os.Remove
	syncSnapshotDir    = storage.SyncDir
)

// snapshotPathFor is where the snapshot of the heap file at heapPath lives.
func snapshotPathFor(heapPath string) string {
	return strings.TrimSuffix(heapPath, tableFileExt) + snapshotExt
}

func (t *SqlTable) snapshotPath() string { return snapshotPathFor(t.path) }

// treeIDs names the trees the table has now, in the order of keyColumns and then
// indexDefs.
func (t *SqlTable) treeIDs() []index.TreeID {
	var ids []index.TreeID
	for _, col := range t.keyColumns() {
		ids = append(ids, index.TreeID{Kind: index.UniqueTree, Ordinal: col, Column: t.Columns[col].Name})
	}
	for _, def := range t.indexDefs {
		ids = append(ids, index.TreeID{Kind: index.SecondaryTree, Name: def.name, Column: def.column})
	}
	return ids
}

// loadSnapshot returns the trees saved by an earlier run, or nil if there is no
// usable snapshot. Anything wrong with the file removes it and returns nil: the
// caller builds the trees from the heap. The table still remembers that a
// snapshot may exist, because this removal is not made durable; the first heap
// write does that (see prepareHeapWrite). Callers hold t.mu.
func (t *SqlTable) loadSnapshot() *treeSet {
	if !t.snapshotMayExist || t.file == nil {
		return nil
	}
	ids := t.treeIDs()
	if len(ids) == 0 {
		return nil
	}
	path := t.snapshotPath()
	saved, err := index.ReadSnapshot(path, t.fileID, t.file.NumPages(), ids)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			_ = removeSnapshotFile(path)
		}
		return nil
	}
	set := &treeSet{unique: map[int]*index.Tree{}, secondary: map[string]*index.Tree{}}
	for _, s := range saved {
		if s.ID.Kind == index.UniqueTree {
			set.unique[s.ID.Ordinal] = s.Tree
		} else {
			set.secondary[s.ID.Name] = s.Tree
		}
	}
	return set
}

// saveSnapshot writes the built trees next to the heap. It does nothing when
// they are not built: the file on disk, if any, is still what it was. A failure
// is ignored, since the snapshot is only a cache. Callers hold t.mu.
func (t *SqlTable) saveSnapshot() {
	if t.file == nil {
		return
	}
	set := t.builtSet()
	ids := t.treeIDs()
	if set == nil || len(ids) == 0 {
		return
	}
	trees := make([]index.SnapshotTree, len(ids))
	for i, id := range ids {
		trees[i].ID = id
		if id.Kind == index.UniqueTree {
			trees[i].Tree = set.unique[id.Ordinal]
		} else {
			trees[i].Tree = set.secondary[id.Name]
		}
	}
	_ = index.WriteSnapshot(t.snapshotPath(), t.fileID, t.file.NumPages(), trees)
}
```

In `files.go` the constants `snapshotExt`/`snapshotTmpExt` already exist. In `table.go`:
- add to `SqlTable` (after `indexDefs`): 

```go
	// snapshotMayExist is true for a table opened from the catalog: its
	// <id>.idx may exist and be read. Any heap write removes the file first
	// and clears this (see prepareHeapWrite). A table created in this run, or
	// given a new file by ALTER, never has one: ids are not reused.
	snapshotMayExist bool
```
- `Close`: 

```go
func (t *SqlTable) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saveSnapshot()
	return t.closeLocked()
}
```

In `database.go`'s `newLoadedDatabase`, add `snapshotMayExist: true` to the `SqlTable` literal. In `tableindex.go`'s `indexSet`, replace

```go
	set, err := t.buildIndexes()
	if err != nil {
		return nil, err
	}
```

with

```go
	set := t.loadSnapshot()
	if set == nil {
		var err error
		if set, err = t.buildIndexes(); err != nil {
			return nil, err
		}
	}
```

Run: `go test -race ./internal/engine/ 2>&1 | tail -20`
Expected: PASS for the Task 2 tests. (Without the Task 3 protocol the suite may still pass; Task 3 is where staleness is closed, so do not stop here.)

- [ ] **Step 5: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean.

```bash
git add internal/engine
git commit -m "feat(engine): save index snapshots at close and load them on first use

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Never load a stale snapshot

**Files:**
- Modify: `internal/engine/snapshot.go`, `internal/engine/table.go` (`appendToFile`, `deleteFromFile`, `Update`, `adoptFile`, `Drop`), `internal/engine/indexdef.go` (`CreateIndex`, `DropIndex`)
- Test: `internal/engine/snapshot_test.go`

**Interfaces:**
- Consumes: Task 2's `snapshotMayExist`, `snapshotPath`, `removeSnapshotFile`, `syncSnapshotDir`.
- Produces: `(*SqlTable).prepareHeapWrite() error`; `(*SqlTable).discardSnapshotFiles(heapPath string)`.

- [ ] **Step 1: Failing protocol tests**

Append to `internal/engine/snapshot_test.go`:

```go
// savedEvents returns a catalog restarted over a saved snapshot of 30 events,
// with the snapshot path.
func savedEvents(t *testing.T) (Catalog, Engine, string, string) {
	t.Helper()
	dir := t.TempDir()
	c, e := eventsEngine(t, dir, 30)
	mustExec(t, e, "shop", kindQuery)
	path := tableOf(t, c, "shop", "events").snapshotPath()
	c, e = restart(t, dir, c)
	require.FileExists(t, path)
	return c, e, path, dir
}

func TestSnapshot_EveryHeapWriteRemovesItFirst(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"INSERT", "INSERT INTO events (id, kind) VALUES (100, 'k1')"},
		{"UPDATE", "UPDATE events SET kind = 'k9' WHERE id = 1"},
		{"UPDATE of every row", "UPDATE events SET id = 5"},
		{"DELETE by the indexed column", "DELETE FROM events WHERE kind = 'k3'"},
		{"DELETE by a column with no index", "DELETE FROM events WHERE id = 1"},
		{"DELETE of everything", "DELETE FROM events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, path, _ := savedEvents(t)

			mustExec(t, e, "shop", tc.sql)

			assert.NoFileExists(t, path)
		})
	}
}

func TestSnapshot_StatementsThatWriteNothingKeepIt(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"SELECT", "SELECT id FROM events WHERE kind = 'k1'"},
		{"a SELECT that scans", "SELECT id FROM events WHERE id = 3"},
		{"a DELETE that matches nothing", "DELETE FROM events WHERE id = 999"},
		{"an UPDATE that matches nothing", "UPDATE events SET kind = 'z' WHERE id = 999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e, path, _ := savedEvents(t)

			mustExec(t, e, "shop", tc.sql)

			assert.FileExists(t, path)
		})
	}
}

func TestSnapshot_ARejectedInsertKeepsIt(t *testing.T) {
	dir := t.TempDir()
	c := newTestCatalogAt(t, dir)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE users (id INT PRIMARY KEY)")
	mustExec(t, e, "shop", "INSERT INTO users (id) VALUES (1)")
	mustExec(t, e, "shop", "SELECT id FROM users WHERE id = 1")
	path := tableOf(t, c, "shop", "users").snapshotPath()
	c, e = restart(t, dir, c)

	err := execError(t, e, "shop", "INSERT INTO users (id) VALUES (1)")

	assert.ErrorContains(t, err, "duplicate")
	assert.FileExists(t, path)
	_ = c
}

func TestSnapshot_AWriteAfterACrashIsNotMissedByTheNextStart(t *testing.T) {
	_, e, _, dir := savedEvents(t)
	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k3')")
	c := newTestCatalogAt(t, dir) // crash: the first catalog is never closed
	e = NewEngine(c)

	rows := mustExec(t, e, "shop", kindQuery).Rows

	assert.Contains(t, rows, []string{"100"}, "the new row is found: the trees were rebuilt, not loaded")
	assertIndexesMatchHeap(t, tableOf(t, c, "shop", "events"))
}

// A failed unlink must fail the statement before the heap is touched, because a
// write that went ahead could leave a snapshot that no longer matches.
func TestSnapshot_ARemovalThatFailsFailsTheWriteAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fileRemains bool
		fail        func(t *testing.T)
	}{
		{"the unlink fails", true, func(t *testing.T) {
			old := removeSnapshotFile
			removeSnapshotFile = func(string) error { return errInjected }
			t.Cleanup(func() { removeSnapshotFile = old })
		}},
		{"the directory fsync fails", false, func(t *testing.T) {
			old := syncSnapshotDir
			syncSnapshotDir = func(string) error { return errInjected }
			t.Cleanup(func() { syncSnapshotDir = old })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, e, path, _ := savedEvents(t)
			table := tableOf(t, c, "shop", "events")
			rowsBefore := fileRows(t, table)
			tc.fail(t)

			for _, sql := range []string{
				"INSERT INTO events (id, kind) VALUES (100, 'k1')",
				"UPDATE events SET kind = 'z' WHERE id = 1",
				"DELETE FROM events WHERE id = 1",
			} {
				assert.ErrorContains(t, execError(t, e, "shop", sql), errInjected.Error(), sql)
			}

			assert.Equal(t, rowsBefore, fileRows(t, table), "no statement wrote anything")
			if tc.fileRemains {
				assert.FileExists(t, path)
			}
		})
	}
}

func TestSnapshot_ARemovalIsRetriedByTheNextWrite(t *testing.T) {
	c, e, path, _ := savedEvents(t)
	old := syncSnapshotDir
	t.Cleanup(func() { syncSnapshotDir = old })
	syncSnapshotDir = func(string) error { return errInjected }
	require.Error(t, execError(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k1')"))
	syncSnapshotDir = old

	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k1')")

	assert.NoFileExists(t, path)
	assert.Len(t, fileRows(t, tableOf(t, c, "shop", "events")), 31)
}

func TestSnapshot_ARemovalIsOnceNotPerStatement(t *testing.T) {
	_, e, _, _ := savedEvents(t)
	syncs := 0
	old := syncSnapshotDir
	syncSnapshotDir = func(p string) error { syncs++; return old(p) }
	t.Cleanup(func() { syncSnapshotDir = old })

	for i := 100; i < 105; i++ {
		mustExec(t, e, "shop", fmt.Sprintf("INSERT INTO events (id, kind) VALUES (%d, 'k1')", i))
	}

	assert.Equal(t, 1, syncs)
}

// A failed load removes the file, but not durably: the first write must still
// fsync the directory.
func TestSnapshot_AFailedLoadStillGetsItsDirectorySynced(t *testing.T) {
	c, e, path, _ := savedEvents(t)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	b[len(b)-100] ^= 0xFF
	require.NoError(t, os.WriteFile(path, b, 0o644))
	mustExec(t, e, "shop", kindQuery) // the load fails, the file goes
	require.NoFileExists(t, path)
	syncs := 0
	old := syncSnapshotDir
	syncSnapshotDir = func(p string) error { syncs++; return old(p) }
	t.Cleanup(func() { syncSnapshotDir = old })

	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k1')")

	assert.Equal(t, 1, syncs)
	_ = c
}

func TestSnapshot_ANewTableNeverHasOneToRemove(t *testing.T) {
	c := newTestCatalog(t)
	e := NewEngine(c)
	mustExec(t, e, "", "CREATE DATABASE shop")
	mustExec(t, e, "shop", "CREATE TABLE t (id INT)")
	syncs := 0
	old := syncSnapshotDir
	syncSnapshotDir = func(p string) error { syncs++; return old(p) }
	t.Cleanup(func() { syncSnapshotDir = old })

	mustExec(t, e, "shop", "INSERT INTO t (id) VALUES (1)")

	assert.Equal(t, 0, syncs)
}

func TestSnapshot_SchemaAndDefinitionChangesDoNotLeaveOneBehind(t *testing.T) {
	t.Run("CREATE INDEX", func(t *testing.T) {
		_, e, path, _ := savedEvents(t)
		mustExec(t, e, "shop", "CREATE INDEX by_id ON events (id)")
		assert.NoFileExists(t, path)
	})
	t.Run("DROP INDEX", func(t *testing.T) {
		_, e, path, _ := savedEvents(t)
		mustExec(t, e, "shop", "DROP INDEX by_kind ON events")
		assert.NoFileExists(t, path)
	})
	t.Run("ADD COLUMN gives the table a new file and removes the old snapshot", func(t *testing.T) {
		c, e, path, dir := savedEvents(t)
		mustExec(t, e, "shop", "ALTER TABLE events ADD COLUMN extra INT")
		assert.NoFileExists(t, path)
		assert.NotEqual(t, path, tableOf(t, c, "shop", "events").snapshotPath())
		syncs := 0
		old := syncSnapshotDir
		syncSnapshotDir = func(p string) error { syncs++; return old(p) }
		t.Cleanup(func() { syncSnapshotDir = old })
		mustExec(t, e, "shop", "INSERT INTO events (id, kind, extra) VALUES (100, 'k1', 1)")
		assert.Equal(t, 0, syncs, "the new file has no snapshot to remove")
		_ = dir
	})
	t.Run("DROP TABLE removes the snapshot with the heap", func(t *testing.T) {
		_, e, path, dir := savedEvents(t)
		mustExec(t, e, "shop", "DROP TABLE events")
		assert.NoFileExists(t, path)
		assert.Empty(t, namesIn(t, filepath.Join(dir, "data", "shop")))
	})
}
```

And append:

```go
func TestSnapshot_ARunThatEndsAfterAWriteSavesTheNewTrees(t *testing.T) {
	c, e, _, dir := savedEvents(t)
	mustExec(t, e, "shop", "INSERT INTO events (id, kind) VALUES (100, 'k3')")
	mustExec(t, e, "shop", kindQuery) // builds the trees from the new heap

	c, e = restart(t, dir, c)

	cf := countOn(tableOf(t, c, "shop", "events"))
	assert.Contains(t, mustExec(t, e, "shop", kindQuery).Rows, []string{"100"})
	assert.EqualValues(t, 0, cf.scans.Load())
}
```

Add the stub `func (t *SqlTable) prepareHeapWrite() error { return nil }` and `func (t *SqlTable) discardSnapshotFiles(heapPath string) {}` to `snapshot.go`.

Run: `go test ./internal/engine/ -run 'TestSnapshot_' 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)'`
Expected: FAIL on assertions for the removal, failure, retry, once-only, DDL and crash tests; the "keeps it" tests and `ANewTableNeverHasOneToRemove` already pass.

- [ ] **Step 2: Implement the protocol**

Replace the stubs in `snapshot.go` (add imports `fmt`, `path/filepath`):

```go
// prepareHeapWrite makes sure no snapshot of the heap file can survive a write
// to it: the file is removed, and the removal made durable, before the first
// write after open. A crash after the write then finds no snapshot, which is a
// rebuild; without this it would find one that no longer matches. It is a
// no-op once done, and for tables that cannot have one. When it fails nothing
// has been written and the statement must fail. Callers hold t.mu for writing.
func (t *SqlTable) prepareHeapWrite() error {
	if !t.snapshotMayExist {
		return nil
	}
	path := t.snapshotPath()
	if err := removeSnapshotFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the index snapshot of table %q: %w", t.Name, err)
	}
	if err := syncSnapshotDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("removing the index snapshot of table %q: %w", t.Name, err)
	}
	t.snapshotMayExist = false
	return nil
}

// discardSnapshotFiles removes the snapshot and any half-written one of the heap
// file at heapPath. It is best effort: a file left behind is an orphan that
// startup removes, and never one that can be read, because the heap file it
// belongs to is gone or has a new id.
func (t *SqlTable) discardSnapshotFiles(heapPath string) {
	path := snapshotPathFor(heapPath)
	_ = removeSnapshotFile(path)
	_ = removeSnapshotFile(path + ".tmp")
}
```

Hook it (read each function first; each hook goes after validation, before the first write to the file, so a rejected statement does not touch the snapshot):

- `appendToFile` (`table.go`): after `encoded, err := encodeRows(...)` and its error check, before `ids := make(...)`:

```go
	if len(encoded) > 0 {
		if err := t.prepareHeapWrite(); err != nil {
			return err
		}
	}
```
- `deleteFromFile`: immediately before `for _, id := range matched {`:

```go
	if len(matched) > 0 {
		if err := t.prepareHeapWrite(); err != nil {
			return err
		}
	}
```
- `Update`: after `encoded, err := encodeRows(t.Columns, updated)` and its error check, before `newIDs := make(...)`:

```go
	if len(encoded) > 0 {
		if err := t.prepareHeapWrite(); err != nil {
			return err
		}
	}
```
- `adoptFile`: after `t.file, t.path, t.fileID = tf.file, tf.path, tf.id` add `t.snapshotMayExist = false`, and after the old file is removed (`os.Remove(oldPath)`; do it before the early return so it also runs when that removal fails) `t.discardSnapshotFiles(oldPath)`.
- `Drop`: after `closeErr := t.closeLocked()` add `t.discardSnapshotFiles(t.path)`.
- `CreateIndex` and `DropIndex` (`indexdef.go`): after `t.dropIndexes()` add `t.discardSnapshotFiles(t.path)`. The flag stays true on purpose: a removal that is not durable must still be made durable by the next heap write.

Run: `go test -race ./internal/engine/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 3: A restart differential**

Append to `internal/engine/snapshot_test.go` (imports `math/rand`):

```go
// The same random statements run on a table with a CREATE INDEX and on one
// without, across clean restarts and crashes (a new catalog over the same
// directory without closing the old one). Answers and errors must be equal
// throughout, which fails if any restart loads a snapshot that no longer
// matches its heap.
func TestSnapshot_RestartsNeverChangeAnAnswer(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir := t.TempDir()
			c := newTestCatalogAt(t, dir)
			e := NewEngine(c)
			mustExec(t, e, "", "CREATE DATABASE shop")
			mustExec(t, e, "shop", "CREATE TABLE indexed (id INT, kind VARCHAR(20))")
			mustExec(t, e, "shop", "CREATE INDEX by_kind ON indexed (kind)")
			mustExec(t, e, "shop", "CREATE TABLE plain (id INT, kind VARCHAR(20))")
			kind := func() string { return fmt.Sprintf("k%d", rng.Intn(5)) }
			where := func() string {
				switch rng.Intn(5) {
				case 0:
					return ""
				case 1:
					return fmt.Sprintf(" WHERE kind = '%s'", kind())
				case 2:
					return fmt.Sprintf(" WHERE kind >= '%s'", kind())
				case 3:
					return fmt.Sprintf(" WHERE kind < '%s'", kind())
				default:
					return fmt.Sprintf(" WHERE id = %d", rng.Intn(30))
				}
			}

			for op := 0; op < 300; op++ {
				var sql string
				switch rng.Intn(10) {
				case 0, 1, 2:
					sql = fmt.Sprintf("INSERT INTO %%s (id, kind) VALUES (%d, '%s')", rng.Intn(30), kind())
				case 3:
					sql = fmt.Sprintf("UPDATE %%s SET kind = '%s'%s", kind(), where())
				case 4:
					sql = "DELETE FROM %s" + where()
				default:
					sql = "SELECT id, kind FROM %s" + where()
				}
				gotResp, gotErr := runSQL(t, e, fmt.Sprintf(sql, "indexed"))
				wantResp, wantErr := runSQL(t, e, fmt.Sprintf(sql, "plain"))
				require.Equal(t, errText(wantErr), errText(gotErr), "op %d: %s", op, sql)
				require.Equal(t, wantResp.Rows, gotResp.Rows, "op %d: %s", op, sql)

				switch rng.Intn(25) {
				case 0:
					c, e = restart(t, dir, c)
				case 1:
					c = newTestCatalogAt(t, dir) // a crash
					e = NewEngine(c)
				}
			}
		})
	}
}

func runSQL(t *testing.T, e Engine, sql string) (Response, error) {
	t.Helper()
	statement, err := parser.NewParser(parser.NewLexer()).Parse(sql)
	require.NoError(t, err, sql)
	return e.Execute(statement, "shop")
}
```

(Add the `parser` and `math/rand` imports. `errText` is in `indexdiff_test.go`.)

Run: `go test -race ./internal/engine/ -run RestartsNeverChangeAnAnswer -v 2>&1 | tail -8` — Expected: PASS. This test exercises code already written, so it is a differential check, not a RED step; its value is shown by the mutations below.

- [ ] **Step 4: Mutation checks**

Commit, then for each: edit, run `go test ./internal/engine/ 2>&1 | grep -E '^(--- FAIL|ok)' | head`, confirm a failure, `git checkout internal`. Use a Python script for edits (BSD `sed -i` fails silently on macOS).

1. `prepareHeapWrite` returns nil immediately (never removes).
2. `prepareHeapWrite` skips `syncSnapshotDir`.
3. `prepareHeapWrite` does not set `snapshotMayExist = false` (the once-only test).
4. `loadSnapshot` sets `t.snapshotMayExist = false` after a failed read (the durable-sync test).
5. Remove the `prepareHeapWrite` call from `deleteFromFile` only.
6. Remove it from `Update` only.
7. Remove it from `appendToFile` only.
8. `saveSnapshot` is not called from `Close`.
9. `maxTableFileID` ignores `.idx` (use `tableFileExt` only in `fileIDOf`'s list).
10. `removeOrphans` removes referenced snapshots (treat `referenced[id]` as false for `.idx`).
11. `adoptFile` keeps `snapshotMayExist = true` for the new file (the ADD COLUMN subtest counts directory syncs).
12. In `ReadSnapshot` (index package), ignore the heap page count: the engine must still not load a snapshot of a heap with a different page count. If no engine test fails, that is covered by the index test in Task 1; confirm that one fails.

A mutant that survives means a missing test: add one and rerun.

- [ ] **Step 5: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean.

```bash
git add internal/engine
git commit -m "feat(engine): remove the index snapshot before the first heap write

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Documentation and measurement

**Files:**
- Modify: `CLAUDE.md`
- Test: a throwaway measurement file, deleted afterwards

- [ ] **Step 1: CLAUDE.md**

In the Storage paragraph, after the sentence ending `…the file an UPDATE writes are the scan's; a range spread over more of the file scans.`, leave as is. After the sentence that ends `…and one already in the file degrades only that index, which then scans.` (the CREATE INDEX part B sentence in the catalog paragraph), append:

`A clean shutdown saves each table's trees to <file_id>.idx next to the heap (sorted entries in checksummed pages, written to .idx.tmp and renamed), and the first use after a restart loads them instead of scanning the heap; it trusts the file only if the heap file id, page count and set of trees all match, and any doubt removes it and rebuilds. The first INSERT, UPDATE or DELETE that changes the heap after a table is opened removes its snapshot and fsyncs the directory before writing (a failure there fails the statement before anything is written), so a crash never leaves a snapshot that is older than the heap. Startup removes .idx and .idx.tmp files no table references, and the file-id allocator counts them so a leftover id is never reused.`

Also change the sentence in the Storage paragraph that says the index is "discarded whenever the file might disagree with it … close" if it conflicts: on close the trees are now saved first. Reword `(an I/O error mid-write, ADD/DROP COLUMN, close)` to `(an I/O error mid-write, ADD/DROP COLUMN, close, after being saved to a snapshot)`.

- [ ] **Step 2: One-off measurement**

Create `internal/engine/zz_measure_test.go`: a table `m(id INT PRIMARY KEY, grp INT, pad VARCHAR(100))` with 100,000 rows (batches of 5,000 through `InsertValues`, `pad` = 40 bytes), `CREATE INDEX by_grp ON m (grp)`, one `SELECT id FROM m WHERE grp = 5` (builds the trees), then time, for a restart over (a) the saved snapshot and (b) the same directory with the `.idx` file deleted, the first `SELECT id FROM m WHERE grp = 5` after `restart`, using `shippedRangeCostFactor`. Log both durations and the size of the `.idx` file. Run: `go test ./internal/engine/ -run ZZMeasure -v`. Quote the numbers in the PR. Delete the file afterwards.

- [ ] **Step 3: Whole suite and commit**

Run: `gofmt -l . ; go vet ./... && go test -race ./...` — Expected: clean.

```bash
git add CLAUDE.md
git commit -m "docs(engine): describe the index snapshots

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
