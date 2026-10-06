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
