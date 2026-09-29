package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFile(t *testing.T) {
	t.Run("creates the file if it does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")

		f, err := NewFile(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()

		_, err = os.Stat(path)
		assert.NoError(t, err)
	})

	t.Run("errors when the parent directory does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing-dir", "table.tbl")

		_, err := NewFile(path)
		assert.Error(t, err)
	})
}

func TestSqlFile_InsertRow(t *testing.T) {
	t.Run("Close with no inserts writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		f, err := NewFile(path)
		require.NoError(t, err)

		require.NoError(t, f.Close())

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Zero(t, info.Size())
	})

	t.Run("Close flushes the single in-progress page", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		f, err := NewFile(path)
		require.NoError(t, err)

		require.NoError(t, f.InsertRow([]byte("row-one")))
		require.NoError(t, f.InsertRow([]byte("row-two")))
		require.NoError(t, f.Close())

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.EqualValues(t, maxPageSize, info.Size())
	})

	t.Run("a row too large for any page is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		f, err := NewFile(path)
		require.NoError(t, err)

		err = f.InsertRow(make([]byte, maxRowSize+1))
		require.Error(t, err)

		// Rejected before ever touching a page — no wasted empty page
		// written to disk for a row that could never fit anywhere.
		require.NoError(t, f.Close())
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Zero(t, info.Size())
	})

	t.Run("a page that fills up flushes mid-stream, plus the rollover page on Close", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		f, err := NewFile(path)
		require.NoError(t, err)

		rowSize := maxPageSize / 2
		require.NoError(t, f.InsertRow(make([]byte, rowSize))) // fills most of page 0
		require.NoError(t, f.InsertRow(make([]byte, rowSize))) // doesn't fit: rolls over to page 1
		require.NoError(t, f.Close())                          // flushes page 1

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.EqualValues(t, 2*maxPageSize, info.Size())
	})

	t.Run("flushed bytes on disk match Page.Encode for the same rows", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "table.tbl")
		f, err := NewFile(path)
		require.NoError(t, err)

		row := []byte("round-trip-me")
		require.NoError(t, f.InsertRow(row))
		require.NoError(t, f.Close())

		want := NewPage(0)
		_, err = want.InsertRow(row)
		require.NoError(t, err)

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want.Encode(), got)
	})
}
