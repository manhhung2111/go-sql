package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func TestNewTable_CreatesItsFileInTheDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	files, err := newFileAllocator(DataDir(dir))
	require.NoError(t, err)

	// The database directory does not exist yet; creating the table makes it.
	table, err := NewTable("users", idColumn, files, "shop")
	require.NoError(t, err)
	t.Cleanup(func() { _ = table.Close() })

	assert.Equal(t, filepath.Join(dir, "data", "shop", "1.tbl"), table.path)
	_, statErr := os.Stat(table.path)
	assert.NoError(t, statErr)
}

func TestNewTable_RejectedSchemaCreatesNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	files, err := newFileAllocator(DataDir(dir))
	require.NoError(t, err)

	_, err = NewTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "id", DataType: parser.IntDataType{}},
	}, files, "shop")

	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "data"))
	assert.True(t, os.IsNotExist(statErr), "validation runs before any directory or file is created")
}
