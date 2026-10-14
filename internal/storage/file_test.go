package storage

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestFile(t *testing.T) (File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "table.tbl")
	f, err := CreateFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

// scanAll drains a scan, copying each row's bytes since they are only valid
// until the next iteration.
func scanAll(t *testing.T, f File) []Row {
	t.Helper()
	var rows []Row
	for row, err := range f.Scan() {
		require.NoError(t, err)
		rows = append(rows, Row{ID: row.ID, Bytes: append([]byte(nil), row.Bytes...)})
	}
	return rows
}

func rowStrings(rows []Row) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = string(row.Bytes)
	}
	return out
}

func TestCreateFile(t *testing.T) {
	t.Run("creates the file", func(t *testing.T) {
		_, path := newTestFile(t)

		_, err := os.Stat(path)
		assert.NoError(t, err)
	})

	t.Run("fails if the file already exists, leaving it untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		require.NoError(t, os.WriteFile(path, []byte("existing"), 0644))

		_, err := CreateFile(path)

		assert.Error(t, err)
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, []byte("existing"), got)
	})

	t.Run("fails when the parent directory does not exist", func(t *testing.T) {
		_, err := CreateFile(filepath.Join(t.TempDir(), "missing-dir", "table.tbl"))
		assert.Error(t, err)
	})
}

func TestOpenFile(t *testing.T) {
	t.Run("fails when the file does not exist", func(t *testing.T) {
		_, err := OpenFile(filepath.Join(t.TempDir(), "nope.tbl"))
		assert.Error(t, err)
	})

	t.Run("fails when the size is not a whole number of pages", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "torn.tbl")
		require.NoError(t, os.WriteFile(path, make([]byte, maxPageSize+1), 0644))

		_, err := OpenFile(path)

		assert.ErrorContains(t, err, "torn or corrupt")
	})

	t.Run("an empty file opens with no rows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.tbl")
		require.NoError(t, os.WriteFile(path, nil, 0644))

		f, err := OpenFile(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()

		assert.Empty(t, scanAll(t, f))
	})

	t.Run("reopening keeps the rows and continues RowID numbering", func(t *testing.T) {
		f, path := newTestFile(t)
		id0, err := f.Insert([]byte("one"))
		require.NoError(t, err)
		id1, err := f.Insert([]byte("two"))
		require.NoError(t, err)
		require.NoError(t, f.Sync())
		require.NoError(t, f.Close())

		reopened, err := OpenFile(path)
		require.NoError(t, err)
		defer func() { _ = reopened.Close() }()

		assert.Equal(t, []string{"one", "two"}, rowStrings(scanAll(t, reopened)))
		id2, err := reopened.Insert([]byte("three"))
		require.NoError(t, err)

		assert.Equal(t, RowID{Page: 0, Slot: 0}, id0)
		assert.Equal(t, RowID{Page: 0, Slot: 1}, id1)
		assert.Equal(t, RowID{Page: 0, Slot: 2}, id2, "the reloaded tail page keeps taking rows")
		assert.Equal(t, []string{"one", "two", "three"}, rowStrings(scanAll(t, reopened)))
	})
}

func TestSqlFile_Insert(t *testing.T) {
	t.Run("rows on one page get consecutive slots", func(t *testing.T) {
		f, _ := newTestFile(t)

		id0, err := f.Insert([]byte("a"))
		require.NoError(t, err)
		id1, err := f.Insert([]byte("b"))
		require.NoError(t, err)

		assert.Equal(t, RowID{Page: 0, Slot: 0}, id0)
		assert.Equal(t, RowID{Page: 0, Slot: 1}, id1)
	})

	t.Run("a row that does not fit rolls over to a new page", func(t *testing.T) {
		f, path := newTestFile(t)
		rowSize := maxPageSize / 2

		id0, err := f.Insert(make([]byte, rowSize))
		require.NoError(t, err)
		id1, err := f.Insert(make([]byte, rowSize))
		require.NoError(t, err)

		assert.Equal(t, RowID{Page: 0, Slot: 0}, id0)
		assert.Equal(t, RowID{Page: 1, Slot: 0}, id1)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.EqualValues(t, 2*maxPageSize, info.Size(), "every insert is on disk without any flush or Close")
	})

	t.Run("a row too large for any page is rejected without touching the file", func(t *testing.T) {
		f, path := newTestFile(t)

		_, err := f.Insert(make([]byte, maxRowSize+1))

		require.Error(t, err)
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.Zero(t, info.Size())
	})

	t.Run("the largest allowed row fits on a page of its own", func(t *testing.T) {
		f, _ := newTestFile(t)

		id, err := f.Insert(make([]byte, maxRowSize))

		require.NoError(t, err)
		assert.Equal(t, RowID{Page: 0, Slot: 0}, id)
	})

	t.Run("bytes on disk match Page.Encode for the same row", func(t *testing.T) {
		f, path := newTestFile(t)
		row := []byte("round-trip-me")
		_, err := f.Insert(row)
		require.NoError(t, err)

		want := NewPage(0)
		_, err = want.InsertRow(row)
		require.NoError(t, err)

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want.Encode(), got)
	})
}

func TestSqlFile_Scan(t *testing.T) {
	t.Run("an empty file yields nothing", func(t *testing.T) {
		f, _ := newTestFile(t)
		assert.Empty(t, scanAll(t, f))
	})

	t.Run("yields rows in page order across pages, with their RowIDs", func(t *testing.T) {
		f, _ := newTestFile(t)
		big := make([]byte, maxPageSize/2)
		_, err := f.Insert(append([]byte{'a'}, big...))
		require.NoError(t, err)
		_, err = f.Insert(append([]byte{'b'}, big...))
		require.NoError(t, err)
		_, err = f.Insert([]byte("c"))
		require.NoError(t, err)

		rows := scanAll(t, f)

		require.Len(t, rows, 3)
		assert.Equal(t, RowID{Page: 0, Slot: 0}, rows[0].ID)
		assert.Equal(t, RowID{Page: 1, Slot: 0}, rows[1].ID)
		assert.Equal(t, RowID{Page: 1, Slot: 1}, rows[2].ID)
		assert.Equal(t, byte('a'), rows[0].Bytes[0])
		assert.Equal(t, byte('b'), rows[1].Bytes[0])
		assert.Equal(t, []byte("c"), rows[2].Bytes)
	})

	t.Run("stopping early stops the scan", func(t *testing.T) {
		f, _ := newTestFile(t)
		for _, row := range []string{"a", "b", "c"} {
			_, err := f.Insert([]byte(row))
			require.NoError(t, err)
		}

		seen := 0
		for _, err := range f.Scan() {
			require.NoError(t, err)
			seen++
			if seen == 2 {
				break
			}
		}

		assert.Equal(t, 2, seen)
	})

	t.Run("a corrupt page ends the scan with its error, after the good pages", func(t *testing.T) {
		f, path := newTestFile(t)
		_, err := f.Insert(make([]byte, maxPageSize/2))
		require.NoError(t, err)
		_, err = f.Insert(make([]byte, maxPageSize/2)) // page 1
		require.NoError(t, err)

		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = raw.WriteAt([]byte{0xFF}, maxPageSize+maxPageSize-1) // last byte of page 1
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		var rows int
		var scanErr error
		for _, err := range f.Scan() {
			if err != nil {
				scanErr = err
				break
			}
			rows++
		}

		assert.Equal(t, 1, rows, "page 0's row is still delivered")
		assert.ErrorContains(t, scanErr, "page 1")
		assert.ErrorContains(t, scanErr, "checksum mismatch")
	})

	t.Run("a page stored at the wrong position is reported", func(t *testing.T) {
		f, path := newTestFile(t)
		_, err := f.Insert(make([]byte, maxPageSize/2))
		require.NoError(t, err)
		_, err = f.Insert(make([]byte, maxPageSize/2))
		require.NoError(t, err)

		// Overwrite page 1 with a perfectly valid copy of page 0.
		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		copy(contents[maxPageSize:], contents[:maxPageSize])
		require.NoError(t, os.WriteFile(path, contents, 0644))

		var scanErr error
		for _, err := range f.Scan() {
			if err != nil {
				scanErr = err
				break
			}
		}

		assert.ErrorContains(t, scanErr, "header says page 0")
	})
}

func TestSqlFile_Delete(t *testing.T) {
	t.Run("a deleted row disappears from scans and the others keep their RowIDs", func(t *testing.T) {
		f, _ := newTestFile(t)
		var ids []RowID
		for _, row := range []string{"one", "two", "three"} {
			id, err := f.Insert([]byte(row))
			require.NoError(t, err)
			ids = append(ids, id)
		}

		require.NoError(t, f.Delete(ids[1]))

		rows := scanAll(t, f)
		require.Len(t, rows, 2)
		assert.Equal(t, ids[0], rows[0].ID)
		assert.Equal(t, ids[2], rows[1].ID)
		assert.Equal(t, []string{"one", "three"}, rowStrings(rows))
	})

	t.Run("deleting from a page that is not the tail", func(t *testing.T) {
		f, _ := newTestFile(t)
		first, err := f.Insert(make([]byte, maxPageSize/2))
		require.NoError(t, err)
		_, err = f.Insert(make([]byte, maxPageSize/2)) // page 1: first is now not the tail
		require.NoError(t, err)

		require.NoError(t, f.Delete(first))

		rows := scanAll(t, f)
		require.Len(t, rows, 1)
		assert.Equal(t, RowID{Page: 1, Slot: 0}, rows[0].ID)
	})

	t.Run("a later insert does not reuse the deleted slot", func(t *testing.T) {
		f, _ := newTestFile(t)
		old, err := f.Insert([]byte("old"))
		require.NoError(t, err)
		require.NoError(t, f.Delete(old))

		fresh, err := f.Insert([]byte("new"))
		require.NoError(t, err)

		assert.Equal(t, RowID{Page: 0, Slot: 1}, fresh)
	})

	t.Run("the delete is on disk, so it survives a reopen", func(t *testing.T) {
		f, path := newTestFile(t)
		id, err := f.Insert([]byte("gone"))
		require.NoError(t, err)
		_, err = f.Insert([]byte("kept"))
		require.NoError(t, err)
		require.NoError(t, f.Delete(id))
		require.NoError(t, f.Sync())
		require.NoError(t, f.Close())

		reopened, err := OpenFile(path)
		require.NoError(t, err)
		defer func() { _ = reopened.Close() }()

		assert.Equal(t, []string{"kept"}, rowStrings(scanAll(t, reopened)))
	})

	t.Run("an unknown or already-deleted RowID is an error", func(t *testing.T) {
		f, _ := newTestFile(t)
		id, err := f.Insert([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, f.Delete(id))

		assert.Error(t, f.Delete(id), "already deleted")
		assert.Error(t, f.Delete(RowID{Page: 0, Slot: 5}), "slot out of range")
		assert.Error(t, f.Delete(RowID{Page: 3, Slot: 0}), "page out of range")
		assert.Error(t, f.Delete(RowID{Page: -1, Slot: 0}), "negative page")
	})
}

func TestSqlFile_Sync(t *testing.T) {
	f, _ := newTestFile(t)
	_, err := f.Insert([]byte("x"))
	require.NoError(t, err)

	assert.NoError(t, f.Sync())
}

// The heap's pages and a PageFile's pages are the same bytes: a heap file
// opens as a PageFile, and a heap page written through a PageFile scans as rows.
func TestSqlFile_OnDiskFormatIsUnchanged(t *testing.T) {
	t.Run("a heap file opens and reads as a PageFile", func(t *testing.T) {
		f, path := newTestFile(t)
		_, err := f.Insert([]byte("one"))
		require.NoError(t, err)
		_, err = f.Insert(make([]byte, maxRowSize)) // too big to share a page: a second page
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

// A write that fails must not leave the cached tail page holding a change
// that never reached disk, or the next successful write would flush it.
func TestSqlFile_FailedWriteDropsTailCache(t *testing.T) {
	// failWrites points f at a read-only handle for the duration of fn, so
	// every write inside it fails.
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

	t.Run("insert", func(t *testing.T) {
		f, path := newTestFile(t)
		_, err := f.Insert([]byte("kept"))
		require.NoError(t, err)

		failWrites(t, f, path, func() {
			_, err := f.Insert([]byte("lost"))
			assert.Error(t, err)
		})

		id, err := f.Insert([]byte("next"))
		require.NoError(t, err)
		assert.Equal(t, RowID{Page: 0, Slot: 1}, id, "the failed row never took a slot")
		assert.Equal(t, []string{"kept", "next"}, rowStrings(scanAll(t, f)))
	})

	t.Run("delete", func(t *testing.T) {
		f, path := newTestFile(t)
		a, err := f.Insert([]byte("a"))
		require.NoError(t, err)
		_, err = f.Insert([]byte("b"))
		require.NoError(t, err)

		failWrites(t, f, path, func() {
			assert.Error(t, f.Delete(a))
		})

		_, err = f.Insert([]byte("c"))
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"}, rowStrings(scanAll(t, f)), "the failed delete was not flushed by a later write")
	})
}

// MaxRowSize is the limit callers outside the package use to reject an
// oversized row before writing anything: it must be exactly what Insert
// accepts.
func TestMaxRowSize(t *testing.T) {
	f, _ := newTestFile(t)

	_, err := f.Insert(make([]byte, MaxRowSize))
	require.NoError(t, err, "a row of exactly MaxRowSize fits on a page of its own")

	_, err = f.Insert(make([]byte, MaxRowSize+1))
	assert.Error(t, err, "one byte more is rejected")
}

// Select holds only the table's read lock, so several scans of one file can
// run at once; Scan must therefore only read. This is meaningful under -race.
func TestSqlFile_ScansMayRunConcurrently(t *testing.T) {
	f, _ := newTestFile(t)
	const n = 3000 // about 13 bytes a row with its slot, so a few pages
	for i := 0; i < n; i++ {
		_, err := f.Insert([]byte("row-" + strconv.Itoa(i)))
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count := 0
			for _, err := range f.Scan() {
				if !assert.NoError(t, err) {
					return
				}
				count++
			}
			assert.Equal(t, n, count)
		}()
	}
	wg.Wait()
}

func TestSqlFile_Get(t *testing.T) {
	t.Run("returns each row by the id Insert gave, before and after a reopen", func(t *testing.T) {
		f, path := newTestFile(t)
		const n = 3000 // about 13 bytes a row with its slot, so a few pages
		ids := make([]RowID, n)
		for i := 0; i < n; i++ {
			id, err := f.Insert([]byte("row-" + strconv.Itoa(i)))
			require.NoError(t, err)
			ids[i] = id
		}
		require.Greater(t, ids[n-1].Page, int32(0), "the rows must span several pages")

		for i, id := range ids {
			got, err := f.Get(id)
			require.NoError(t, err)
			require.Equal(t, "row-"+strconv.Itoa(i), string(got))
		}

		require.NoError(t, f.Close())
		reopened, err := OpenFile(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = reopened.Close() })
		for i, id := range ids {
			got, err := reopened.Get(id)
			require.NoError(t, err)
			require.Equal(t, "row-"+strconv.Itoa(i), string(got))
		}
	})

	t.Run("an empty row is a live row and comes back as an empty non-nil slice", func(t *testing.T) {
		f, _ := newTestFile(t)
		id, err := f.Insert([]byte{})
		require.NoError(t, err)

		got, err := f.Get(id)

		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("an address with no live row is ErrRowNotFound", func(t *testing.T) {
		f, _ := newTestFile(t)
		deleted, err := f.Insert([]byte("a"))
		require.NoError(t, err)
		_, err = f.Insert([]byte("b"))
		require.NoError(t, err)
		require.NoError(t, f.Delete(deleted))

		tests := []struct {
			name    string
			id      RowID
			wantMsg string
		}{
			{"a deleted row", deleted, "page 0 slot 0 is deleted"},
			{"a slot at the slot count", RowID{Page: 0, Slot: 2}, "page 0 slot 2 out of range [0, 2)"},
			{"a negative slot", RowID{Page: 0, Slot: -1}, "page 0 slot -1 out of range [0, 2)"},
			{"a page at NumPages", RowID{Page: 1, Slot: 0}, "page 1 out of range [0, 1)"},
			{"a negative page", RowID{Page: -1, Slot: 0}, "page -1 out of range [0, 1)"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				got, err := f.Get(tc.id)

				require.ErrorIs(t, err, ErrRowNotFound)
				assert.ErrorContains(t, err, tc.wantMsg)
				assert.Nil(t, got)
			})
		}
	})

	t.Run("any address in an empty file is ErrRowNotFound", func(t *testing.T) {
		f, _ := newTestFile(t)

		_, err := f.Get(RowID{Page: 0, Slot: 0})

		assert.ErrorIs(t, err, ErrRowNotFound)
	})

	t.Run("deleting a row leaves its neighbours readable", func(t *testing.T) {
		f, _ := newTestFile(t)
		a, err := f.Insert([]byte("a"))
		require.NoError(t, err)
		b, err := f.Insert([]byte("b"))
		require.NoError(t, err)
		c, err := f.Insert([]byte("c"))
		require.NoError(t, err)
		require.NoError(t, f.Delete(b))

		for id, want := range map[RowID]string{a: "a", c: "c"} {
			got, err := f.Get(id)
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		}
	})

	t.Run("a corrupt page is an error but not ErrRowNotFound", func(t *testing.T) {
		f, path := newTestFile(t)
		id, err := f.Insert([]byte("a"))
		require.NoError(t, err)

		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = raw.WriteAt([]byte{0xFF}, maxPageSize-1) // last byte of page 0
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		_, err = f.Get(id)

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrRowNotFound)
		assert.ErrorContains(t, err, "checksum mismatch")
	})

	t.Run("a damaged slot under a valid checksum is an error, not a stale address", func(t *testing.T) {
		f, path := newTestFile(t)
		id, err := f.Insert([]byte("abc"))
		require.NoError(t, err)

		// Stretch slot 0's length past the end of the page, then re-stamp the
		// checksum so that only the layout check can notice.
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		binary.LittleEndian.PutUint16(raw[pageHeaderSize+2:], 500)
		stampPage(raw[:maxPageSize], 0)
		require.NoError(t, os.WriteFile(path, raw, 0644))

		got, err := f.Get(id)

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrRowNotFound)
		assert.ErrorContains(t, err, "outside row area")
		assert.Nil(t, got)
	})

	t.Run("the returned bytes belong to the caller, even over a cached page file", func(t *testing.T) {
		// A cached page file hands out one shared, immutable buffer per page,
		// so a Get that returned a view into it would let the caller corrupt
		// the cache.
		inner, err := CreatePageFile(filepath.Join(t.TempDir(), "table.tbl"))
		require.NoError(t, err)
		cached, err := NewCachedPageFile(inner, 4)
		require.NoError(t, err)
		f := &sqlFile{pages: cached}
		t.Cleanup(func() { _ = f.Close() })
		id, err := f.Insert([]byte("abc"))
		require.NoError(t, err)

		first, err := f.Get(id)
		require.NoError(t, err)
		require.Equal(t, "abc", string(first))
		first[0] = 'X'
		second, err := f.Get(id)

		require.NoError(t, err)
		assert.Equal(t, "abc", string(second))
	})
}

// Get holds only the table's read lock, like Scan, so Gets and Scans of one
// file can run at once. This is meaningful under -race.
func TestSqlFile_GetsMayRunConcurrentlyWithScans(t *testing.T) {
	f, path := newTestFile(t)
	const n = 3000
	ids := make([]RowID, n)
	for i := 0; i < n; i++ {
		id, err := f.Insert([]byte("row-" + strconv.Itoa(i)))
		require.NoError(t, err)
		ids[i] = id
	}
	// Reopen so the tail cache is empty: a Get that loaded it would then write
	// shared state, which the race detector catches.
	require.NoError(t, f.Close())
	f, err := OpenFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < n; i += 8 {
				got, err := f.Get(ids[i])
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, "row-"+strconv.Itoa(i), string(got))
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		count := 0
		for _, err := range f.Scan() {
			if !assert.NoError(t, err) {
				return
			}
			count++
		}
		assert.Equal(t, n, count)
	}()
	wg.Wait()
}

// countingPages counts the pages read through it.
type countingPages struct {
	PageFile
	reads int
}

func (c *countingPages) ReadPage(n int32) ([]byte, error) {
	c.reads++
	return c.PageFile.ReadPage(n)
}

// threePages inserts nine 5000-byte rows, three to a page, so ids[0:3] are on
// page 0, ids[3:6] on page 1 and ids[6:9] on page 2.
func threePages(t *testing.T, f File) ([]RowID, [][]byte) {
	t.Helper()
	ids := make([]RowID, 9)
	rows := make([][]byte, 9)
	for i := range ids {
		rows[i] = bytes.Repeat([]byte{byte('a' + i)}, 5000)
		id, err := f.Insert(rows[i])
		require.NoError(t, err)
		ids[i] = id
	}
	require.Equal(t, int32(0), ids[2].Page)
	require.Equal(t, int32(1), ids[3].Page)
	require.Equal(t, int32(2), ids[8].Page)
	return ids, rows
}

func TestSqlFile_NumPages(t *testing.T) {
	f, _ := newTestFile(t)
	assert.Equal(t, int32(0), f.NumPages())

	threePages(t, f)

	assert.Equal(t, int32(3), f.NumPages())
}

func TestSqlFile_GetMany(t *testing.T) {
	t.Run("returns the rows Get returns, in the order of ids, before and after a reopen", func(t *testing.T) {
		f, path := newTestFile(t)
		ids, rows := threePages(t, f)
		reversed := []RowID{ids[8], ids[0], ids[4], ids[3]}

		got, err := f.GetMany(reversed)
		require.NoError(t, err)
		require.Equal(t, [][]byte{rows[8], rows[0], rows[4], rows[3]}, got)

		require.NoError(t, f.Close())
		reopened, err := OpenFile(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = reopened.Close() })
		got, err = reopened.GetMany(ids)
		require.NoError(t, err)
		assert.Equal(t, rows, got)
	})

	t.Run("reads each run of ids on one page once", func(t *testing.T) {
		inner, err := CreatePageFile(filepath.Join(t.TempDir(), "table.tbl"))
		require.NoError(t, err)
		pages := &countingPages{PageFile: inner}
		f := &sqlFile{pages: pages}
		t.Cleanup(func() { _ = f.Close() })
		ids, _ := threePages(t, f)

		tests := []struct {
			name  string
			ids   []RowID
			reads int
		}{
			{"all nine, sorted", ids, 3},
			{"two on one page and one on another", []RowID{ids[0], ids[1], ids[3]}, 2},
			{"a page revisited after another is read again", []RowID{ids[0], ids[3], ids[1]}, 3},
			{"the same id twice", []RowID{ids[4], ids[4]}, 1},
			{"no ids", nil, 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				pages.reads = 0

				got, err := f.GetMany(tc.ids)

				require.NoError(t, err)
				assert.Len(t, got, len(tc.ids))
				assert.NotNil(t, got, "an empty result is an empty slice")
				assert.Equal(t, tc.reads, pages.reads)
			})
		}
	})

	t.Run("an address with no live row fails the whole call, wherever it sits", func(t *testing.T) {
		f, _ := newTestFile(t)
		ids, _ := threePages(t, f)
		require.NoError(t, f.Delete(ids[4]))
		bad := map[string]RowID{
			"a deleted row":       ids[4],
			"a slot out of range": {Page: 1, Slot: 9},
			"a page out of range": {Page: 3, Slot: 0},
			"a negative page":     {Page: -1, Slot: 0},
		}
		for name, id := range bad {
			for position, list := range map[string][]RowID{
				"first":  {id, ids[0], ids[8]},
				"middle": {ids[0], id, ids[8]},
				"last":   {ids[0], ids[8], id},
			} {
				t.Run(name+" "+position, func(t *testing.T) {
					got, err := f.GetMany(list)

					require.ErrorIs(t, err, ErrRowNotFound)
					assert.Nil(t, got)
				})
			}
		}
	})

	t.Run("a corrupt page is an error but not ErrRowNotFound", func(t *testing.T) {
		f, path := newTestFile(t)
		ids, _ := threePages(t, f)
		raw, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = raw.WriteAt([]byte{0xFF}, maxPageSize*2-1) // last byte of page 1
		require.NoError(t, err)
		require.NoError(t, raw.Close())

		got, err := f.GetMany([]RowID{ids[0], ids[3]})

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrRowNotFound)
		assert.ErrorContains(t, err, "checksum mismatch")
		assert.Nil(t, got)
	})

	t.Run("the returned bytes belong to the caller, even over a cached page file", func(t *testing.T) {
		inner, err := CreatePageFile(filepath.Join(t.TempDir(), "table.tbl"))
		require.NoError(t, err)
		cached, err := NewCachedPageFile(inner, 4)
		require.NoError(t, err)
		f := &sqlFile{pages: cached}
		t.Cleanup(func() { _ = f.Close() })
		id, err := f.Insert([]byte("abc"))
		require.NoError(t, err)

		first, err := f.GetMany([]RowID{id})
		require.NoError(t, err)
		first[0][0] = 'X'
		second, err := f.GetMany([]RowID{id})

		require.NoError(t, err)
		assert.Equal(t, "abc", string(second[0]))
	})
}

// GetMany holds only the table's read lock, like Get and Scan. Meaningful
// under -race.
func TestSqlFile_GetManyMayRunConcurrentlyWithGetsAndScans(t *testing.T) {
	f, path := newTestFile(t)
	ids, rows := threePages(t, f)
	require.NoError(t, f.Close())
	f, err := OpenFile(path) // reopened: the tail cache is empty
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				got, err := f.GetMany(ids)
				if assert.NoError(t, err) {
					assert.Equal(t, rows, got)
				}
				_, err = f.Get(ids[i%9])
				assert.NoError(t, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, err := range f.Scan() {
			assert.NoError(t, err)
		}
	}()
	wg.Wait()
}
