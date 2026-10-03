package engine

import (
	"fmt"
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

func TestTable_FileIDMatchesItsFile(t *testing.T) {
	table, err := newTempTable(t, "users", idColumn)
	require.NoError(t, err)

	assert.Equal(t, fmt.Sprintf("%d.tbl", table.fileID), filepath.Base(table.path))
}

func TestTable_AlterGivesTheTableANewFileID(t *testing.T) {
	table, err := newTempTable(t, "users", idColumn)
	require.NoError(t, err)
	before := table.fileID

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}},
	}))

	assert.Greater(t, table.fileID, before)
	assert.Equal(t, fmt.Sprintf("%d.tbl", table.fileID), filepath.Base(table.path))
}
