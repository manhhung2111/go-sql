# Page file and page cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a page-level file API (`PageFile`) and a per-file write-through LRU page cache to `internal/storage`, and move the heap file onto `PageFile`, with no on-disk format change.

**Architecture:** A new `osPageFile` verifies and stamps only the format-neutral page header (checksum at 0:4, page number at 4:8) and exposes `ReadPage`/`WritePage`/`AllocatePage`. The heap `sqlFile` is rebuilt on it and keeps its tail cache, `Insert`, `Delete` and `Scan`. `NewCachedPageFile` is a decorator over any `PageFile` with immutable buffers and plain LRU; nothing uses it yet.

**Tech Stack:** Go 1.26.4, `container/list`, testify (`assert`/`require`).

**Spec:** `docs/specs/2026-10-06-page-file-and-cache-design.md`

## Global Constraints

- Go 1.26.4; `internal/storage` never imports `internal/parser` or `internal/engine`.
- No on-disk format change: existing heap files open and scan exactly as before.
- Error messages: lowercase, no trailing punctuation, wrap OS errors with `%w`. The existing wording that tests match on stays: `page N: checksum mismatch: ...`, `page N: header says page M`, `file %q is %d bytes, not a multiple of the %d-byte page size (torn or corrupt)`, `creating file %q: %w`.
- `PageFile` concurrency contract: any number of `ReadPage` calls may run concurrently; `WritePage`, `AllocatePage`, `Sync` and `Close` need exclusive access from the owner.
- A buffer returned by `ReadPage` is read-only; callers copy it to modify it.
- The cache is write-through (disk first), per file, immutable buffers, plain LRU. No pins, dirty pages, config values, or root priority.
- Tests: testify, table-driven where there is a matrix. Every test is seen failing first, for the intended reason.
- Commits: Conventional Commits `<type>(scope): description`, ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Before each commit: `go test -race ./internal/storage/...`; before the final commit also `go test -race ./...`, `go vet ./...`, `gofmt -l .` (must print nothing).

## Review Focus

Failure modes the spec implies that no single happy-path test would catch, most likely first:

1. A heap file written before this change must open and scan after it, and a page written through `PageFile` must scan as heap rows (format compatibility, both directions). Pinned in Task 2.
2. A buffer a reader already holds must never change when a later `WritePage` replaces the frame or later reads evict it. Pinned in Task 3.
3. A failed write must leave the cache matching disk: the next read reloads the on-disk page, and `NumPages` does not advance on a failed append. Pinned in Tasks 1 and 3.
4. Two readers missing the same page at once must not create two frames or push the cache over capacity. Pinned in Task 3.
5. `WritePage` must not modify the caller's buffer (the heap passes `Page.Encode()`, which is the page's own buffer). Pinned in Tasks 1 and 3.
6. A torn or short file, a page at the wrong position, a corrupt checksum and an out-of-range page are errors, never a panic and never cached. Pinned in Tasks 1 and 3.

---

## File structure

- Create `internal/storage/pagefile.go`: `PageFile`, `osPageFile`, `CreatePageFile`, `OpenPageFile`, `stampPage`.
- Create `internal/storage/pagefile_test.go`.
- Modify `internal/storage/page.go`: split `validateLayout` out of `DecodePage`.
- Modify `internal/storage/file.go`: `sqlFile` holds a `PageFile`.
- Modify `internal/storage/file_test.go`: retarget one helper, add two format tests.
- Create `internal/storage/pagecache.go` and `internal/storage/pagecache_test.go`.

---

### Task 1: `PageFile`

**Files:**
- Create: `internal/storage/pagefile.go`
- Test: `internal/storage/pagefile_test.go`

**Interfaces:**
- Consumes: constants `maxPageSize`, `checksumPos`, `pageNumberPos` from `page.go`.
- Produces:
  - `type PageFile interface { ReadPage(n int32) ([]byte, error); WritePage(n int32, buf []byte) error; AllocatePage() (int32, error); NumPages() int32; Sync() error; Close() error }`
  - `func CreatePageFile(path string) (PageFile, error)`
  - `func OpenPageFile(path string) (PageFile, error)`
  - `type osPageFile struct { file *os.File; numPages int32 }`
  - `func stampPage(buf []byte, n int32)` (sets page number and checksum in place; used by Task 3)

- [ ] **Step 1: Write the failing tests**

Create `internal/storage/pagefile_test.go`:

```go
package storage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPageFile(t *testing.T) (PageFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.idx")
	pf, err := CreatePageFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pf.Close() })
	return pf, path
}

// filledPage is a page-sized buffer with every byte set to b.
func filledPage(b byte) []byte {
	buf := make([]byte, maxPageSize)
	for i := range buf {
		buf[i] = b
	}
	return buf
}

func TestCreatePageFile(t *testing.T) {
	t.Run("creates the file", func(t *testing.T) {
		_, path := newTestPageFile(t)

		_, err := os.Stat(path)
		assert.NoError(t, err)
	})

	t.Run("fails if the file already exists, leaving it untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.idx")
		require.NoError(t, os.WriteFile(path, []byte("existing"), 0644))

		_, err := CreatePageFile(path)

		assert.Error(t, err)
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, []byte("existing"), got)
	})

	t.Run("fails when the parent directory does not exist", func(t *testing.T) {
		_, err := CreatePageFile(filepath.Join(t.TempDir(), "missing-dir", "index.idx"))
		assert.Error(t, err)
	})
}

func TestOpenPageFile(t *testing.T) {
	t.Run("fails when the file does not exist", func(t *testing.T) {
		_, err := OpenPageFile(filepath.Join(t.TempDir(), "nope.idx"))
		assert.Error(t, err)
	})

	t.Run("fails when the size is not a whole number of pages", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "torn.idx")
		require.NoError(t, os.WriteFile(path, make([]byte, maxPageSize+1), 0644))

		_, err := OpenPageFile(path)

		assert.ErrorContains(t, err, "torn or corrupt")
	})

	t.Run("an empty file opens with no pages", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.idx")
		require.NoError(t, os.WriteFile(path, nil, 0644))

		pf, err := OpenPageFile(path)
		require.NoError(t, err)
		defer func() { _ = pf.Close() }()

		assert.Equal(t, int32(0), pf.NumPages())
	})

	t.Run("reopening keeps the pages", func(t *testing.T) {
		pf, path := newTestPageFile(t)
		require.NoError(t, pf.WritePage(0, filledPage(1)))
		require.NoError(t, pf.WritePage(1, filledPage(2)))
		require.NoError(t, pf.Sync())
		require.NoError(t, pf.Close())

		reopened, err := OpenPageFile(path)
		require.NoError(t, err)
		defer func() { _ = reopened.Close() }()

		assert.Equal(t, int32(2), reopened.NumPages())
		got, err := reopened.ReadPage(1)
		require.NoError(t, err)
		assert.Equal(t, byte(2), got[maxPageSize-1])
	})
}

func TestOsPageFile_WriteAndReadPage(t *testing.T) {
	t.Run("round-trips the payload and stamps the header", func(t *testing.T) {
		pf, _ := newTestPageFile(t)
		require.NoError(t, pf.WritePage(0, filledPage(7)))

		got, err := pf.ReadPage(0)

		require.NoError(t, err)
		require.Len(t, got, maxPageSize)
		assert.Equal(t, uint32(0), binary.LittleEndian.Uint32(got[pageNumberPos:]), "page number is stamped")
		assert.Equal(t, filledPage(7)[8:], got[8:], "the payload after the 8-byte generic header is untouched")
	})

	t.Run("does not modify the caller's buffer", func(t *testing.T) {
		pf, _ := newTestPageFile(t)
		buf := filledPage(7)

		require.NoError(t, pf.WritePage(0, buf))

		assert.Equal(t, filledPage(7), buf, "header stamping happens on a copy")
	})

	t.Run("overwriting a page replaces it", func(t *testing.T) {
		pf, _ := newTestPageFile(t)
		require.NoError(t, pf.WritePage(0, filledPage(1)))
		require.NoError(t, pf.WritePage(0, filledPage(2)))

		got, err := pf.ReadPage(0)

		require.NoError(t, err)
		assert.Equal(t, byte(2), got[maxPageSize-1])
		assert.Equal(t, int32(1), pf.NumPages())
	})

	t.Run("writing at NumPages appends, and the write is on disk without a sync", func(t *testing.T) {
		pf, path := newTestPageFile(t)

		require.NoError(t, pf.WritePage(0, filledPage(1)))
		require.NoError(t, pf.WritePage(1, filledPage(2)))

		assert.Equal(t, int32(2), pf.NumPages())
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.EqualValues(t, 2*maxPageSize, info.Size())
	})

	t.Run("a write beyond NumPages, a negative page and a wrong-size buffer are errors", func(t *testing.T) {
		pf, _ := newTestPageFile(t)

		assert.Error(t, pf.WritePage(1, filledPage(1)), "leaves a gap")
		assert.Error(t, pf.WritePage(-1, filledPage(1)))
		assert.Error(t, pf.WritePage(0, make([]byte, maxPageSize-1)))
		assert.Equal(t, int32(0), pf.NumPages())
	})

	t.Run("reading a page that does not exist is an error", func(t *testing.T) {
		pf, _ := newTestPageFile(t)
		require.NoError(t, pf.WritePage(0, filledPage(1)))

		_, err := pf.ReadPage(1)
		assert.ErrorContains(t, err, "page 1 out of range")
		_, err = pf.ReadPage(-1)
		assert.Error(t, err)
	})
}

func TestOsPageFile_AllocatePage(t *testing.T) {
	pf, path := newTestPageFile(t)

	first, err := pf.AllocatePage()
	require.NoError(t, err)
	second, err := pf.AllocatePage()
	require.NoError(t, err)

	assert.Equal(t, int32(0), first)
	assert.Equal(t, int32(1), second)
	assert.Equal(t, int32(2), pf.NumPages())
	got, err := pf.ReadPage(1)
	require.NoError(t, err, "an allocated page has a valid header")
	assert.Equal(t, make([]byte, maxPageSize-8), got[8:], "the payload is all zero")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.EqualValues(t, 2*maxPageSize, info.Size())
}

func TestOsPageFile_ReadDetectsDamage(t *testing.T) {
	t.Run("a corrupt page is reported with its number", func(t *testing.T) {
		pf, path := newTestPageFile(t)
		require.NoError(t, pf.WritePage(0, filledPage(1)))
		require.NoError(t, pf.WritePage(1, filledPage(2)))

		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = raw.WriteAt([]byte{0xFF}, 2*maxPageSize-1) // last byte of page 1
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		_, err = pf.ReadPage(1)
		assert.ErrorContains(t, err, "page 1")
		assert.ErrorContains(t, err, "checksum mismatch")
		_, err = pf.ReadPage(0)
		assert.NoError(t, err, "the other page is still readable")
	})

	t.Run("a page stored at the wrong position is reported", func(t *testing.T) {
		pf, path := newTestPageFile(t)
		require.NoError(t, pf.WritePage(0, filledPage(1)))
		require.NoError(t, pf.WritePage(1, filledPage(2)))

		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		copy(contents[maxPageSize:], contents[:maxPageSize]) // a valid copy of page 0 at page 1
		require.NoError(t, os.WriteFile(path, contents, 0644))

		_, err = pf.ReadPage(1)
		assert.ErrorContains(t, err, "header says page 0")
	})
}

// A failed append must not advance NumPages, or the next append would leave a
// gap and the file would claim a page that was never written.
func TestOsPageFile_FailedWrite(t *testing.T) {
	pf, path := newTestPageFile(t)
	require.NoError(t, pf.WritePage(0, filledPage(1)))

	osf := pf.(*osPageFile)
	readWrite := osf.file
	readOnly, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = readOnly.Close() }()
	osf.file = readOnly
	assert.Error(t, pf.WritePage(1, filledPage(2)), "append")
	assert.Error(t, pf.WritePage(0, filledPage(3)), "overwrite")
	_, allocErr := pf.AllocatePage()
	assert.Error(t, allocErr, "allocate")
	osf.file = readWrite

	assert.Equal(t, int32(1), pf.NumPages())
	got, err := pf.ReadPage(0)
	require.NoError(t, err)
	assert.Equal(t, byte(1), got[maxPageSize-1], "the failed overwrite changed nothing")
	require.NoError(t, pf.WritePage(1, filledPage(4)), "the next append lands at page 1")
}

// Several goroutines may read at once; ReadPage must only read. Meaningful
// under -race.
func TestOsPageFile_ReadsMayRunConcurrently(t *testing.T) {
	pf, _ := newTestPageFile(t)
	const pages = 8
	for i := 0; i < pages; i++ {
		require.NoError(t, pf.WritePage(int32(i), filledPage(byte(i+1))))
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < pages; i++ {
				got, err := pf.ReadPage(int32(i))
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, byte(i+1), got[maxPageSize-1])
			}
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/storage/ -run 'PageFile' 2>&1 | head -20`
Expected: build failure, `undefined: CreatePageFile` (and `OpenPageFile`, `osPageFile`). That is the missing feature, not a typo.

- [ ] **Step 3: Write the implementation**

Create `internal/storage/pagefile.go`:

```go
package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// PageFile is a file of 16KiB pages addressed by page number. It verifies and
// stamps only the format-neutral page header (checksum at 0:4, page number at
// 4:8); everything after is the owner's to lay out.
//
// Any number of ReadPage calls may run concurrently with one another. WritePage,
// AllocatePage, Sync and Close must not run concurrently with anything else, so
// the owner serializes them.
type PageFile interface {
	// ReadPage returns page n after verifying its size, checksum and page
	// number. The returned buffer is read-only: copy it to modify it.
	ReadPage(n int32) ([]byte, error)
	// WritePage stamps page number n and the checksum into a copy of buf and
	// writes it through. It does not fsync. buf must be exactly one page.
	// n may be NumPages(), which appends the page; n beyond that is an error.
	WritePage(n int32, buf []byte) error
	// AllocatePage appends an all-zero payload page with a valid header and
	// returns its number. It is for owners whose blank page is valid; the heap
	// appends with WritePage instead.
	AllocatePage() (int32, error)
	NumPages() int32
	Sync() error
	Close() error
}

type osPageFile struct {
	file     *os.File
	numPages int32
}

// CreatePageFile makes a new, empty page file. It fails if path already
// exists, so a new file can never silently pick up an old file's pages.
func CreatePageFile(path string) (PageFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating file %q: %w", path, err)
	}
	if err := SyncDir(filepath.Dir(path)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("creating file %q: %w", path, err)
	}
	return &osPageFile{file: f}, nil
}

// OpenPageFile opens an existing page file. A size that is not a whole number
// of pages means a torn or corrupt file.
func OpenPageFile(path string) (PageFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening file %q: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat file %q: %w", path, err)
	}
	if info.Size()%maxPageSize != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("file %q is %d bytes, not a multiple of the %d-byte page size (torn or corrupt)", path, info.Size(), maxPageSize)
	}

	return &osPageFile{file: f, numPages: int32(info.Size() / maxPageSize)}, nil
}

// stampPage sets buf's page number and then its checksum, which covers
// everything after its own 4 bytes. buf must be one page.
func stampPage(buf []byte, n int32) {
	binary.LittleEndian.PutUint32(buf[pageNumberPos:], uint32(n))
	binary.LittleEndian.PutUint32(buf[checksumPos:], crc32.ChecksumIEEE(buf[pageNumberPos:]))
}

func (p *osPageFile) ReadPage(n int32) ([]byte, error) {
	if n < 0 || n >= p.numPages {
		return nil, fmt.Errorf("page %d out of range [0, %d)", n, p.numPages)
	}

	buf := make([]byte, maxPageSize)
	// ReadAt may return io.EOF alongside a full read at the end of the file.
	if got, err := p.file.ReadAt(buf, int64(n)*maxPageSize); got != len(buf) && err != nil {
		return nil, fmt.Errorf("reading page %d: %w", n, err)
	}

	want := binary.LittleEndian.Uint32(buf[checksumPos:])
	if got := crc32.ChecksumIEEE(buf[pageNumberPos:]); got != want {
		return nil, fmt.Errorf("page %d: checksum mismatch: stored %#x, computed %#x", n, want, got)
	}
	if got := int32(binary.LittleEndian.Uint32(buf[pageNumberPos:])); got != n {
		return nil, fmt.Errorf("page %d: header says page %d", n, got)
	}
	return buf, nil
}

func (p *osPageFile) WritePage(n int32, buf []byte) error {
	if len(buf) != maxPageSize {
		return fmt.Errorf("page is %d bytes, want %d", len(buf), maxPageSize)
	}
	if n < 0 || n > p.numPages {
		return fmt.Errorf("page %d out of range [0, %d]", n, p.numPages)
	}

	out := append([]byte(nil), buf...)
	stampPage(out, n)
	if _, err := p.file.WriteAt(out, int64(n)*maxPageSize); err != nil {
		return fmt.Errorf("writing page %d: %w", n, err)
	}
	if n == p.numPages {
		p.numPages++
	}
	return nil
}

func (p *osPageFile) AllocatePage() (int32, error) {
	n := p.numPages
	if err := p.WritePage(n, make([]byte, maxPageSize)); err != nil {
		return 0, err
	}
	return n, nil
}

func (p *osPageFile) NumPages() int32 { return p.numPages }

func (p *osPageFile) Sync() error {
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	return nil
}

func (p *osPageFile) Close() error {
	if err := p.file.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/storage/ -run 'PageFile' -v 2>&1 | tail -30`
Expected: all `PageFile` tests PASS.

- [ ] **Step 5: Mutation-check the failed-append rule**

In `WritePage`, move `p.numPages++` above the `WriteAt` call (so it advances before the write). Run `go test ./internal/storage/ -run TestOsPageFile_FailedWrite`; expected FAIL (`NumPages` is 2). Revert and re-run: PASS.

- [ ] **Step 6: Run the whole package and commit**

Run: `go test -race ./internal/storage/... && go vet ./internal/storage/... && gofmt -l internal/storage`
Expected: PASS, no vet output, gofmt prints nothing.

```bash
git add internal/storage/pagefile.go internal/storage/pagefile_test.go
git commit -m "$(cat <<'EOF'
feat(storage): add a page-level file API

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Move the heap file onto `PageFile`

**Files:**
- Modify: `internal/storage/page.go` (split `validateLayout` out of `DecodePage`)
- Modify: `internal/storage/file.go`
- Modify: `internal/storage/file_test.go` (retarget one helper, add two tests)

**Interfaces:**
- Consumes: `PageFile`, `CreatePageFile`, `OpenPageFile`, `osPageFile` (Task 1).
- Produces: unchanged public API (`File`, `CreateFile`, `OpenFile`, `RowID`, `Row`, `Page`, `NewPage`, `DecodePage`). New unexported `validateLayout(data []byte) error`. `sqlFile` now has a `pages PageFile` field in place of `file` and `numPages`.

- [ ] **Step 1: Write the failing tests**

In `internal/storage/file_test.go`, add after `TestSqlFile_Sync`:

```go
// The heap's pages and a PageFile's pages are the same bytes: a heap file
// opens as a PageFile, and a heap page written through a PageFile scans as rows.
func TestSqlFile_OnDiskFormatIsUnchanged(t *testing.T) {
	t.Run("a heap file opens and reads as a PageFile", func(t *testing.T) {
		f, path := newTestFile(t)
		_, err := f.Insert([]byte("one"))
		require.NoError(t, err)
		_, err = f.Insert(make([]byte, maxPageSize/2)) // a second page
		require.NoError(t, err)
		require.NoError(t, f.Sync())
		require.NoError(t, f.Close())

		pf, err := OpenPageFile(path)
		require.NoError(t, err)
		defer func() { _ = pf.Close() }()

		assert.Equal(t, int32(2), pf.NumPages())
		buf, err := pf.ReadPage(0)
		require.NoError(t, err)
		page, err := DecodePage(buf)
		require.NoError(t, err)
		row, live := page.Row(0)
		assert.True(t, live)
		assert.Equal(t, []byte("one"), row)
	})

	t.Run("a heap page written through a PageFile scans as heap rows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		pf, err := CreatePageFile(path)
		require.NoError(t, err)
		page := NewPage(0)
		_, err = page.InsertRow([]byte("written-by-pagefile"))
		require.NoError(t, err)
		require.NoError(t, pf.WritePage(0, page.Encode()))
		require.NoError(t, pf.Sync())
		require.NoError(t, pf.Close())

		f, err := OpenFile(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()

		assert.Equal(t, []string{"written-by-pagefile"}, rowStrings(scanAll(t, f)))
	})
}
```

These two pass before the refactor too (they pin behaviour, not the new structure), so they are a safety net. The refactor's own failing test is the retargeted helper: replace the helper body in `TestSqlFile_FailedWriteDropsTailCache`:

```go
	failWrites := func(t *testing.T, f File, path string, fn func()) {
		t.Helper()
		pf := f.(*sqlFile).pages.(*osPageFile)
		readWrite := pf.file
		readOnly, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = readOnly.Close() }()

		pf.file = readOnly
		fn()
		pf.file = readWrite
	}
```

- [ ] **Step 2: Run the tests to verify the right failure**

Run: `go test ./internal/storage/ 2>&1 | head -20`
Expected: compile error `f.(*sqlFile).pages undefined (type *sqlFile has no field or method pages)`. That is the intended failure: the structure the refactor adds.

- [ ] **Step 3: Split the layout validation out of `DecodePage`**

In `internal/storage/page.go`, replace `DecodePage` with:

```go
// DecodePage validates data as an encoded page — exact size, matching
// checksum, and a slot array whose live rows all lie inside the row area —
// and returns a Page that owns its own copy of the bytes.
func DecodePage(data []byte) (Page, error) {
	if len(data) != maxPageSize {
		return nil, fmt.Errorf("page is %d bytes, want %d", len(data), maxPageSize)
	}

	want := binary.LittleEndian.Uint32(data[checksumPos:])
	if got := crc32.ChecksumIEEE(data[pageNumberPos:]); got != want {
		return nil, fmt.Errorf("checksum mismatch: stored %#x, computed %#x", want, got)
	}

	if err := validateLayout(data); err != nil {
		return nil, err
	}
	return &sqlSlottedPage{buf: append([]byte(nil), data...)}, nil
}

// validateLayout checks that data's slot array and row area are consistent:
// the data start lies after the slot array, and every live row lies inside the
// row area. It only reads data, and assumes it is one page whose checksum was
// already verified.
func validateLayout(data []byte) error {
	p := &sqlSlottedPage{buf: data}

	slotArrayEnd := pageHeaderSize + p.SlotCount()*slotSize
	dataStart := p.dataStart()
	if dataStart < slotArrayEnd || dataStart > maxPageSize {
		return fmt.Errorf("data start %d outside [%d, %d]", dataStart, slotArrayEnd, maxPageSize)
	}

	for i := 0; i < p.SlotCount(); i++ {
		offset, length := p.slot(i)
		if offset == 0 {
			if length != 0 {
				return fmt.Errorf("slot %d: tombstone with length %d", i, length)
			}
			continue
		}
		if offset < dataStart || offset+length > maxPageSize {
			return fmt.Errorf("slot %d: row [%d, %d) outside row area [%d, %d)", i, offset, offset+length, dataStart, maxPageSize)
		}
	}
	return nil
}
```

Run: `go test ./internal/storage/ -run 'DecodePage|SlottedPage|NewPage'`
Expected: PASS (pure refactor, the existing page tests pin the messages).

- [ ] **Step 4: Rebuild `file.go` on `PageFile`**

Replace the top of `internal/storage/file.go` through `Close` (keep the `RowID`, `Row` and `File` declarations and their comments as they are) so the file reads:

```go
package storage

import (
	"fmt"
	"iter"
)

// ... RowID, Row and File declarations unchanged ...

type sqlFile struct {
	pages PageFile
	// tail caches the last page. Every write goes straight to disk, so the
	// cache always equals what is on disk; it is nil when not loaded.
	tail Page
}

// CreateFile makes a new, empty heap file. It fails if path already exists,
// so a new table can never silently pick up an old file's rows.
func CreateFile(path string) (File, error) {
	pages, err := CreatePageFile(path)
	if err != nil {
		return nil, err
	}
	return &sqlFile{pages: pages}, nil
}

// OpenFile opens an existing heap file. A size that is not a whole number of
// pages means a torn or corrupt file.
func OpenFile(path string) (File, error) {
	pages, err := OpenPageFile(path)
	if err != nil {
		return nil, err
	}
	return &sqlFile{pages: pages}, nil
}

// readPage returns page n as a private, mutable copy: the page file's buffer
// is read-only, and the tail and Delete paths change the page they read.
func (f *sqlFile) readPage(n int32) (Page, error) {
	buf, err := f.pages.ReadPage(n)
	if err != nil {
		return nil, err
	}
	if err := validateLayout(buf); err != nil {
		return nil, fmt.Errorf("page %d: %w", n, err)
	}
	return &sqlSlottedPage{buf: append([]byte(nil), buf...)}, nil
}

// loadTail fills the tail cache from disk if it is empty and the file has
// any pages.
func (f *sqlFile) loadTail() error {
	if f.tail != nil || f.pages.NumPages() == 0 {
		return nil
	}
	page, err := f.readPage(f.pages.NumPages() - 1)
	if err != nil {
		return err
	}
	f.tail = page
	return nil
}

func (f *sqlFile) Insert(rowBytes []byte) (RowID, error) {
	if len(rowBytes) > maxRowSize {
		return RowID{}, fmt.Errorf("row of %d bytes exceeds max row size of %d bytes", len(rowBytes), maxRowSize)
	}

	if err := f.loadTail(); err != nil {
		return RowID{}, err
	}

	if f.tail != nil {
		slot, err := f.tail.InsertRow(rowBytes)
		if err == nil {
			tailNumber := f.pages.NumPages() - 1
			if err := f.pages.WritePage(tailNumber, f.tail.Encode()); err != nil {
				// The cached page now holds a row that never reached disk;
				// drop it so the next operation reloads what is really there.
				f.tail = nil
				return RowID{}, err
			}
			return RowID{Page: tailNumber, Slot: slot}, nil
		}
		// The tail is full (a failed InsertRow changes nothing): fall
		// through to a fresh page.
	}

	newNumber := f.pages.NumPages()
	page := NewPage(newNumber)
	slot, err := page.InsertRow(rowBytes)
	if err != nil {
		return RowID{}, fmt.Errorf("row too large to fit in a page: %w", err)
	}
	if err := f.pages.WritePage(newNumber, page.Encode()); err != nil {
		return RowID{}, err
	}

	f.tail = page
	return RowID{Page: newNumber, Slot: slot}, nil
}

func (f *sqlFile) Delete(id RowID) error {
	numPages := f.pages.NumPages()
	if id.Page < 0 || id.Page >= numPages {
		return fmt.Errorf("page %d out of range [0, %d)", id.Page, numPages)
	}

	isTail := id.Page == numPages-1
	var page Page
	if isTail {
		if err := f.loadTail(); err != nil {
			return err
		}
		page = f.tail
	} else {
		var err error
		if page, err = f.readPage(id.Page); err != nil {
			return err
		}
	}

	if err := page.DeleteRow(id.Slot); err != nil {
		return fmt.Errorf("page %d: %w", id.Page, err)
	}
	if err := f.pages.WritePage(id.Page, page.Encode()); err != nil {
		if isTail {
			f.tail = nil // see Insert: don't keep a change that never reached disk
		}
		return err
	}
	return nil
}

func (f *sqlFile) Scan() iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		// Snapshot the page count: rows inserted while a scan is running
		// are not part of it.
		numPages := f.pages.NumPages()
		for pageNumber := int32(0); pageNumber < numPages; pageNumber++ {
			// A scan only reads, so it views the page buffer in place instead
			// of copying it; the buffer is fresh per read and never mutated.
			buf, err := f.pages.ReadPage(pageNumber)
			if err == nil {
				if err = validateLayout(buf); err != nil {
					err = fmt.Errorf("page %d: %w", pageNumber, err)
				}
			}
			if err != nil {
				yield(Row{}, err)
				return
			}
			page := &sqlSlottedPage{buf: buf}
			for slot := 0; slot < page.SlotCount(); slot++ {
				rowBytes, live := page.Row(slot)
				if !live {
					continue
				}
				if !yield(Row{ID: RowID{Page: pageNumber, Slot: slot}, Bytes: rowBytes}, nil) {
					return
				}
			}
		}
	}
}

func (f *sqlFile) Sync() error { return f.pages.Sync() }

// Close closes the underlying file. There is nothing to flush: every write
// already went straight to disk.
func (f *sqlFile) Close() error { return f.pages.Close() }
```

Remove the now-unused `os` and `path/filepath` imports. Keep the `RowID`, `Row` and `File` blocks (including their doc comments) exactly as they were.

- [ ] **Step 5: Run the whole package**

Run: `go test -race ./internal/storage/... && go vet ./internal/storage/... && gofmt -l internal/storage`
Expected: PASS (every pre-existing heap test, plus the two format tests and the retargeted failed-write test), no vet output, gofmt prints nothing.

- [ ] **Step 6: Run the engine tests, which sit on the heap file**

Run: `go test -race ./...`
Expected: PASS. The engine never saw the refactor.

- [ ] **Step 7: Mutation-check the tail-drop rule survived the refactor**

In `Insert`, delete the `f.tail = nil` line in the failed-write branch. Run `go test ./internal/storage/ -run FailedWriteDropsTailCache`; expected FAIL. Revert and re-run: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/storage/page.go internal/storage/file.go internal/storage/file_test.go
git commit -m "$(cat <<'EOF'
refactor(storage): build the heap file on the page file

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Page cache

**Files:**
- Create: `internal/storage/pagecache.go`
- Test: `internal/storage/pagecache_test.go`

**Interfaces:**
- Consumes: `PageFile`, `osPageFile`, `stampPage`, `maxPageSize`, `filledPage` and `newTestPageFile`-style helpers (Task 1).
- Produces: `func NewCachedPageFile(inner PageFile, capacity int) (PageFile, error)`; unexported `cachedPageFile` with fields `frames map[int32]*list.Element` and `lru *list.List` (used by the concurrency test's invariant check).

- [ ] **Step 1: Write the failing tests**

Create `internal/storage/pagecache_test.go`:

```go
package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingPageFile counts the reads that reach the wrapped file and can be told
// to fail writes.
type countingPageFile struct {
	PageFile
	reads      atomic.Int32
	failWrites bool
}

func (c *countingPageFile) ReadPage(n int32) ([]byte, error) {
	c.reads.Add(1)
	return c.PageFile.ReadPage(n)
}

func (c *countingPageFile) WritePage(n int32, buf []byte) error {
	if c.failWrites {
		return errors.New("disk full")
	}
	return c.PageFile.WritePage(n, buf)
}

// newCachedTest returns a cache of the given capacity over a file whose page i
// is filled with byte i+1, plus the counter in front of the file and its path.
func newCachedTest(t *testing.T, capacity, pages int) (*cachedPageFile, *countingPageFile, string) {
	t.Helper()
	inner, path := newTestPageFile(t)
	for i := 0; i < pages; i++ {
		require.NoError(t, inner.WritePage(int32(i), filledPage(byte(i+1))))
	}
	counter := &countingPageFile{PageFile: inner}
	cache, err := NewCachedPageFile(counter, capacity)
	require.NoError(t, err)
	return cache.(*cachedPageFile), counter, path
}

func TestNewCachedPageFile(t *testing.T) {
	inner, _ := newTestPageFile(t)

	_, err := NewCachedPageFile(inner, 0)
	assert.ErrorContains(t, err, "capacity must be at least 1")
	_, err = NewCachedPageFile(inner, -3)
	assert.Error(t, err)
	_, err = NewCachedPageFile(inner, 1)
	assert.NoError(t, err)
}

func TestCachedPageFile_ReadPage(t *testing.T) {
	t.Run("a hit does not reach the wrapped file", func(t *testing.T) {
		cache, counter, _ := newCachedTest(t, 4, 2)

		first, err := cache.ReadPage(0)
		require.NoError(t, err)
		second, err := cache.ReadPage(0)
		require.NoError(t, err)

		assert.Equal(t, int32(1), counter.reads.Load())
		assert.Equal(t, first, second)
		assert.Equal(t, byte(1), second[maxPageSize-1])
	})

	t.Run("a full cache evicts the least recently used page", func(t *testing.T) {
		cache, counter, _ := newCachedTest(t, 2, 3)
		for _, n := range []int32{0, 1} {
			_, err := cache.ReadPage(n)
			require.NoError(t, err)
		}
		_, err := cache.ReadPage(0) // hit: page 0 is now the most recent
		require.NoError(t, err)
		_, err = cache.ReadPage(2) // evicts page 1, the least recent
		require.NoError(t, err)
		require.Equal(t, int32(3), counter.reads.Load())

		_, err = cache.ReadPage(0)
		require.NoError(t, err)
		assert.Equal(t, int32(3), counter.reads.Load(), "page 0 survived")

		_, err = cache.ReadPage(1)
		require.NoError(t, err)
		assert.Equal(t, int32(4), counter.reads.Load(), "page 1 was evicted")
	})

	t.Run("a corrupt page is an error and is never cached", func(t *testing.T) {
		cache, counter, path := newCachedTest(t, 4, 2)
		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = raw.WriteAt([]byte{0xFF}, 2*maxPageSize-1) // last byte of page 1
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		_, err = cache.ReadPage(1)
		assert.ErrorContains(t, err, "checksum mismatch")
		_, err = cache.ReadPage(1)
		assert.Error(t, err)

		assert.Equal(t, int32(2), counter.reads.Load(), "the second read went to the file again")
	})

	t.Run("a page that does not exist is an error", func(t *testing.T) {
		cache, _, _ := newCachedTest(t, 4, 1)

		_, err := cache.ReadPage(5)

		assert.Error(t, err)
	})
}

func TestCachedPageFile_WritePage(t *testing.T) {
	t.Run("installs the new page, so the next read is a hit", func(t *testing.T) {
		cache, counter, _ := newCachedTest(t, 4, 1)

		require.NoError(t, cache.WritePage(0, filledPage(9)))
		got, err := cache.ReadPage(0)

		require.NoError(t, err)
		assert.Equal(t, byte(9), got[maxPageSize-1])
		assert.Equal(t, int32(0), counter.reads.Load())
	})

	t.Run("does not modify the caller's buffer", func(t *testing.T) {
		cache, _, _ := newCachedTest(t, 4, 1)
		buf := filledPage(9)

		require.NoError(t, cache.WritePage(0, buf))

		assert.Equal(t, filledPage(9), buf)
	})

	t.Run("a failed write drops the entry, so the next read reloads the on-disk page", func(t *testing.T) {
		cache, counter, _ := newCachedTest(t, 4, 1)
		_, err := cache.ReadPage(0)
		require.NoError(t, err)
		require.Equal(t, int32(1), counter.reads.Load())

		counter.failWrites = true
		assert.Error(t, cache.WritePage(0, filledPage(9)))
		counter.failWrites = false

		got, err := cache.ReadPage(0)
		require.NoError(t, err)
		assert.Equal(t, int32(2), counter.reads.Load(), "the entry was dropped")
		assert.Equal(t, byte(1), got[maxPageSize-1], "and the disk still holds the old page")
	})

	t.Run("a failed append caches nothing", func(t *testing.T) {
		cache, counter, _ := newCachedTest(t, 4, 1)

		counter.failWrites = true
		assert.Error(t, cache.WritePage(1, filledPage(9)))
		counter.failWrites = false

		assert.Equal(t, int32(1), cache.NumPages())
		assert.Zero(t, cache.lru.Len())
	})

	t.Run("a wrong-size buffer is an error", func(t *testing.T) {
		cache, _, _ := newCachedTest(t, 4, 1)

		assert.Error(t, cache.WritePage(0, make([]byte, 10)))
	})

	t.Run("appends through the cache", func(t *testing.T) {
		cache, _, _ := newCachedTest(t, 4, 1)

		require.NoError(t, cache.WritePage(1, filledPage(5)))

		assert.Equal(t, int32(2), cache.NumPages())
		got, err := cache.ReadPage(1)
		require.NoError(t, err)
		assert.Equal(t, byte(5), got[maxPageSize-1])
	})
}

// A reader that already holds a page must keep seeing the bytes it read, however
// the cache later replaces or evicts that page.
func TestCachedPageFile_HeldBufferIsNeverMutated(t *testing.T) {
	cache, _, _ := newCachedTest(t, 2, 3)
	held, err := cache.ReadPage(0)
	require.NoError(t, err)
	snapshot := append([]byte(nil), held...)

	require.NoError(t, cache.WritePage(0, filledPage(9)), "replaced while held")
	assert.Equal(t, snapshot, held)

	_, err = cache.ReadPage(1)
	require.NoError(t, err)
	_, err = cache.ReadPage(2) // evicts the replacement of page 0
	require.NoError(t, err)
	assert.Equal(t, snapshot, held, "evicted while held")
}

func TestCachedPageFile_AllocatePageIsNotCached(t *testing.T) {
	cache, counter, _ := newCachedTest(t, 4, 0)

	n, err := cache.AllocatePage()
	require.NoError(t, err)
	assert.Equal(t, int32(0), n)
	assert.Equal(t, int32(1), cache.NumPages())
	assert.Zero(t, cache.lru.Len())

	_, err = cache.ReadPage(0)
	require.NoError(t, err)
	assert.Equal(t, int32(1), counter.reads.Load())
}

func TestCachedPageFile_CloseEmptiesTheCache(t *testing.T) {
	cache, _, _ := newCachedTest(t, 4, 1)
	_, err := cache.ReadPage(0)
	require.NoError(t, err)

	require.NoError(t, cache.Close())

	_, err = cache.ReadPage(0)
	assert.Error(t, err, "a closed file is not served from a stale cache")
}

// Readers that miss the same page at once must not leave two frames for it or
// push the cache over capacity. Meaningful under -race.
func TestCachedPageFile_ConcurrentReaders(t *testing.T) {
	const pages = 6
	cache, _, _ := newCachedTest(t, 3, pages)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				for i := 0; i < pages; i++ {
					got, err := cache.ReadPage(int32(i))
					if !assert.NoError(t, err) {
						return
					}
					assert.Equal(t, byte(i+1), got[maxPageSize-1])
				}
			}
		}()
	}
	wg.Wait()

	assert.LessOrEqual(t, cache.lru.Len(), 3)
	assert.Equal(t, cache.lru.Len(), len(cache.frames), "the map and the list agree")
}

func TestCachedPageFile_ReadsAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.idx")
	inner, err := CreatePageFile(path)
	require.NoError(t, err)
	cache, err := NewCachedPageFile(inner, 2)
	require.NoError(t, err)
	require.NoError(t, cache.WritePage(0, filledPage(3)))
	require.NoError(t, cache.Sync())
	require.NoError(t, cache.Close())

	reopened, err := OpenPageFile(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()

	got, err := reopened.ReadPage(0)
	require.NoError(t, err)
	assert.Equal(t, byte(3), got[maxPageSize-1], "the write went through to disk")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/storage/ -run 'CachedPageFile' 2>&1 | head -20`
Expected: build failure, `undefined: NewCachedPageFile` and `undefined: cachedPageFile`. The missing feature.

- [ ] **Step 3: Write the implementation**

Create `internal/storage/pagecache.go`:

```go
package storage

import (
	"container/list"
	"fmt"
	"sync"
)

// cachedPageFile is a write-through LRU cache in front of a PageFile.
//
// Cached buffers are immutable: a write installs a new buffer rather than
// changing the cached one, and nothing recycles a buffer, so a reader that
// already holds one keeps seeing the bytes it read even after the page is
// replaced or evicted. The mutex covers only the map and the list.
type cachedPageFile struct {
	inner    PageFile
	capacity int

	mu     sync.Mutex
	frames map[int32]*list.Element
	lru    *list.List // front is the most recently used
}

type frame struct {
	page int32
	buf  []byte
}

// NewCachedPageFile wraps inner with a write-through LRU cache of capacity
// pages. Like the file it wraps, reads may run concurrently with one another
// and writes need exclusive access from the owner.
func NewCachedPageFile(inner PageFile, capacity int) (PageFile, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("page cache capacity must be at least 1, got %d", capacity)
	}
	return &cachedPageFile{
		inner:    inner,
		capacity: capacity,
		frames:   make(map[int32]*list.Element),
		lru:      list.New(),
	}, nil
}

func (c *cachedPageFile) ReadPage(n int32) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.frames[n]; ok {
		c.lru.MoveToFront(el)
		buf := el.Value.(*frame).buf
		c.mu.Unlock()
		return buf, nil
	}
	c.mu.Unlock()

	// The read happens outside the lock so a slow read does not block hits.
	buf, err := c.inner.ReadPage(n)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.frames[n]; ok { // another reader installed it first
		c.lru.MoveToFront(el)
		return el.Value.(*frame).buf, nil
	}
	c.install(n, buf)
	return buf, nil
}

func (c *cachedPageFile) WritePage(n int32, buf []byte) error {
	if len(buf) != maxPageSize {
		return fmt.Errorf("page is %d bytes, want %d", len(buf), maxPageSize)
	}

	// Stamp a private copy: it is what lands in the cache, so the caller's
	// buffer is never modified and never aliased by a cached frame.
	stamped := append([]byte(nil), buf...)
	stampPage(stamped, n)

	// Disk first. On failure the cache must not keep anything the disk lacks,
	// so the page's entry is dropped and the next read reloads what is there.
	if err := c.inner.WritePage(n, stamped); err != nil {
		c.mu.Lock()
		c.drop(n)
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	c.install(n, stamped)
	c.mu.Unlock()
	return nil
}

// AllocatePage appends a blank page. Nothing is cached until it is first read
// or written.
func (c *cachedPageFile) AllocatePage() (int32, error) { return c.inner.AllocatePage() }

func (c *cachedPageFile) NumPages() int32 { return c.inner.NumPages() }

func (c *cachedPageFile) Sync() error { return c.inner.Sync() }

func (c *cachedPageFile) Close() error {
	c.mu.Lock()
	c.frames = make(map[int32]*list.Element)
	c.lru.Init()
	c.mu.Unlock()
	return c.inner.Close()
}

// install makes buf page n's frame and the most recently used, replacing any
// existing frame with a new one (the old frame's buffer is left untouched), then
// evicts from the back while over capacity. Callers hold c.mu.
func (c *cachedPageFile) install(n int32, buf []byte) {
	if el, ok := c.frames[n]; ok {
		el.Value = &frame{page: n, buf: buf}
		c.lru.MoveToFront(el)
		return
	}
	c.frames[n] = c.lru.PushFront(&frame{page: n, buf: buf})
	for c.lru.Len() > c.capacity {
		oldest := c.lru.Back()
		delete(c.frames, oldest.Value.(*frame).page)
		c.lru.Remove(oldest)
	}
}

// drop forgets page n. Callers hold c.mu.
func (c *cachedPageFile) drop(n int32) {
	if el, ok := c.frames[n]; ok {
		c.lru.Remove(el)
		delete(c.frames, n)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/storage/ -run 'CachedPageFile' -v 2>&1 | tail -40`
Expected: all PASS.

- [ ] **Step 5: Mutation checks (one at a time; revert each)**

1. In `WritePage`, remove the `c.drop(n)` in the failure branch. Run `go test ./internal/storage/ -run 'CachedPageFile_WritePage'`. Expected FAIL ("the entry was dropped"). Revert.
2. In `install`, replace the `el.Value = &frame{...}` line with `copy(el.Value.(*frame).buf, buf)` (mutate in place). Run `-run HeldBufferIsNeverMutated`. Expected FAIL. Revert.
3. In `ReadPage`'s hit branch, delete `c.lru.MoveToFront(el)`. Run `-run 'CachedPageFile_ReadPage'`. Expected FAIL (the LRU test). Revert.
4. In `WritePage`, call `c.install(n, stamped)` before `c.inner.WritePage`. Run `-run 'CachedPageFile_WritePage'`. Expected FAIL (failed-write tests). Revert.
5. In `ReadPage`'s miss path, delete the `if el, ok := c.frames[n]; ok { ... }` re-check after the read. Run `go test -race -count=20 ./internal/storage/ -run ConcurrentReaders`. Expected FAIL or a flaky failure of the map/list agreement assertion; record which in the PR. Revert.

After the last revert, re-run `go test -race ./internal/storage/...`: PASS.

- [ ] **Step 6: Full verification and commit**

Run: `go test -race ./... && go vet ./... && gofmt -l .`
Expected: PASS, no vet output, gofmt prints nothing.

```bash
git add internal/storage/pagecache.go internal/storage/pagecache_test.go
git commit -m "$(cat <<'EOF'
feat(storage): add a write-through page cache

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>
EOF
)"
```

---

## Wrap-up (after Task 3)

- One fresh whole-branch review against the spec and this plan.
- Open the PR with the repo's `create-pr` skill. Test plan lists the tests seen failing first and the mutation checks above.
- `.wiki/` is not edited here: the sync-wiki workflow ingests the merged PR and updates `subsystems/storage.md` and `code/storage.md`.

## Self-review notes

- **Spec coverage:** `PageFile` and constructors (Task 1); `AllocatePage`, append via `WritePage(NumPages())`, torn-append handling (Task 1); heap refactor with `validateLayout` split to avoid the double copy, `Scan` bypassing the cache, `tail` cache kept (Task 2); cache decorator, write-through, replace-not-mutate, plain LRU, `capacity < 1` rejected, `Close` empties (Task 3); all listed tests and mutation checks present; out-of-scope items (config, pins, dirty pages, root priority, routing `Scan` through the cache) have no task.
- **Type consistency:** `PageFile` methods, `osPageFile.file`/`numPages`, `stampPage(buf, n)`, `validateLayout(data)`, `cachedPageFile.frames`/`lru`, `filledPage`, `newTestPageFile` are used with the same names and signatures across tasks.
- **Placeholders:** none. The only "unchanged" instruction is for the existing `RowID`, `Row` and `File` declarations in `file.go`, which stay as they are.
