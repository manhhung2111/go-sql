package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFileAllocator(t *testing.T) {
	t.Run("a data directory with nothing in it starts at id 1", func(t *testing.T) {
		dir := t.TempDir()
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		assert.Equal(t, filepath.Join(dir, "data", "shop", "1.tbl"), a.newTablePath("shop"))
	})

	t.Run("ids are shared across databases and never repeat", func(t *testing.T) {
		dir := t.TempDir()
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		first := a.newTablePath("a")
		second := a.newTablePath("b")
		third := a.newTablePath("a")

		assert.Equal(t, filepath.Join(dir, "data", "a", "1.tbl"), first)
		assert.Equal(t, filepath.Join(dir, "data", "b", "2.tbl"), second)
		assert.Equal(t, filepath.Join(dir, "data", "a", "3.tbl"), third)
	})

	t.Run("continues after the highest id already on disk", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "data", "a"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "data", "b"), 0o755))
		for _, name := range []string{
			"a/5.tbl", "b/9.tbl", "a/7.tbl",
			"a/notes.txt",  // not a table file
			"a/x.tbl",      // not a numeric id
			"a/12.tbl.tmp", // a temp file is not a table file
			"readme",       // a plain file directly under data/ is not a database
		} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "data", name), nil, 0o644))
		}

		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		assert.Equal(t, filepath.Join(dir, "data", "a", "10.tbl"), a.newTablePath("a"))
	})

	t.Run("an empty data directory path is rejected", func(t *testing.T) {
		_, err := newFileAllocator("")

		assert.EqualError(t, err, "data directory is required")
	})

	t.Run("a data directory that cannot be scanned is an error", func(t *testing.T) {
		dir := t.TempDir()
		// "data" is a regular file, so it cannot be read as a directory.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), nil, 0o644))

		_, err := newFileAllocator(DataDir(dir))

		assert.Error(t, err)
	})

	t.Run("concurrent callers get distinct paths", func(t *testing.T) {
		a, err := newFileAllocator(DataDir(t.TempDir()))
		require.NoError(t, err)

		const n = 100
		paths := make(chan string, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				paths <- a.newTablePath("shop")
			}()
		}
		wg.Wait()
		close(paths)

		seen := make(map[string]bool, n)
		for p := range paths {
			seen[p] = true
		}
		assert.Len(t, seen, n)
	})
}

func TestValidateDatabaseName(t *testing.T) {
	for _, name := range []string{"shop", "Shop_2", "日本"} {
		assert.NoError(t, validateDatabaseName(name), name)
	}

	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "../x", "a\x00b"} {
		assert.ErrorContains(t, validateDatabaseName(name), "invalid database name", "%q", name)
	}
}

func TestFileAllocator_makeDir(t *testing.T) {
	t.Run("creates every missing level", func(t *testing.T) {
		dir := t.TempDir()
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		require.NoError(t, a.makeDir(a.databaseDir("shop")))

		info, err := os.Stat(filepath.Join(dir, "data", "shop"))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("is idempotent", func(t *testing.T) {
		a, err := newFileAllocator(DataDir(t.TempDir()))
		require.NoError(t, err)

		require.NoError(t, a.makeDir(a.databaseDir("shop")))
		require.NoError(t, a.makeDir(a.databaseDir("shop")))
	})

	t.Run("fails when a path component is a file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "data"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "data", "shop"), []byte("x"), 0o644))
		a, err := newFileAllocator(DataDir(dir))
		require.NoError(t, err)

		assert.Error(t, a.makeDir(a.databaseDir("shop")))
	})
}
