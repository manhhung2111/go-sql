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
	// afterRead, when set, runs once after the wrapped read returns and before
	// its result is handed back, to interleave another operation with the read.
	afterRead func()
}

func (c *countingPageFile) ReadPage(n int32) ([]byte, error) {
	c.reads.Add(1)
	buf, err := c.PageFile.ReadPage(n)
	if hook := c.afterRead; hook != nil {
		c.afterRead = nil
		hook()
	}
	return buf, err
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

	t.Run("a failed write of one page does not evict another", func(t *testing.T) {
		cache, counter, _ := newCachedTest(t, 1, 2)
		_, err := cache.ReadPage(0)
		require.NoError(t, err)

		counter.failWrites = true
		assert.Error(t, cache.WritePage(1, filledPage(9)))
		counter.failWrites = false

		_, err = cache.ReadPage(0)
		require.NoError(t, err)
		assert.Equal(t, int32(1), counter.reads.Load(), "page 0 is still cached")
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

// A read that started before a write finished must not install its older bytes
// over the write's: the cache stays correct even if the owner does not exclude
// them.
func TestCachedPageFile_StaleReadDoesNotOverwriteNewerWrite(t *testing.T) {
	cache, counter, _ := newCachedTest(t, 4, 1)
	counter.afterRead = func() {
		require.NoError(t, cache.WritePage(0, filledPage(9)))
	}

	_, err := cache.ReadPage(0) // reads the old page, then the write lands, then it tries to install

	require.NoError(t, err)
	got, err := cache.ReadPage(0)
	require.NoError(t, err)
	assert.Equal(t, byte(9), got[maxPageSize-1], "the newer write wins")
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
