package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// newTestCatalogAt builds a catalog over dir and closes it when the test
// ends, so open file handles never pile up across the suite.
func newTestCatalogAt(t *testing.T, dir string) Catalog {
	t.Helper()
	catalog, err := NewCatalog(DataDir(dir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	return catalog
}

func newTestCatalog(t *testing.T) Catalog {
	t.Helper()
	return newTestCatalogAt(t, t.TempDir())
}

func newTestDatabase(t *testing.T) Database {
	t.Helper()
	files, err := newFileAllocator(DataDir(t.TempDir()))
	require.NoError(t, err)
	db, err := NewDatabase("testdb", files)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTempTable is NewTable over a temp-dir file, with the same (table, err)
// result so a call site only changes the function name. The table's file is
// closed when the test ends.
func newTempTable(t *testing.T, name string, columns []parser.ColumnDefinition) (*SqlTable, error) {
	t.Helper()
	table, err := NewTable(name, columns, filepath.Join(t.TempDir(), "1.tbl"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = table.Close() })
	return table, nil
}

// tableFiles lists the file names in a database's directory, sorted.
func tableFiles(t *testing.T, dir, database string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "data", database))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}
