package storage

import (
	"os"
	"path/filepath"
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

// A write that fails must not leave the cached tail page holding a change
// that never reached disk, or the next successful write would flush it.
func TestSqlFile_FailedWriteDropsTailCache(t *testing.T) {
	// failWrites points f at a read-only handle for the duration of fn, so
	// every write inside it fails.
	failWrites := func(t *testing.T, f File, path string, fn func()) {
		t.Helper()
		sf := f.(*sqlFile)
		readWrite := sf.file
		readOnly, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = readOnly.Close() }()

		sf.file = readOnly
		fn()
		sf.file = readWrite
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
