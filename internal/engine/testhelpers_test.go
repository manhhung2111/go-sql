package engine

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
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
	db, err := NewDatabase("testdb", newTestStore(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTempTable is NewTable over a temp-dir data directory, with the same
// (table, err) result so a call site only changes the function name. The
// table's file is closed when the test ends.
func newTempTable(t *testing.T, name string, columns []parser.ColumnDefinition) (*SqlTable, error) {
	t.Helper()
	table, err := NewTable(name, columns, newTestStore(t), "testdb")
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

// fileRows scans the table's file and decodes every live row with the
// table's current schema: the table's contents.
func fileRows(t *testing.T, table *SqlTable) [][]any {
	t.Helper()
	rows := make([][]any, 0)
	for row, err := range table.file.Scan() {
		require.NoError(t, err)
		decoded, err := DecodeRow(table.Columns, row.Bytes)
		require.NoError(t, err)
		rows = append(rows, decoded)
	}
	return rows
}

// assertTableRows checks the table holds exactly want, in file order: insertion
// order, except that an updated row has moved to the end.
func assertTableRows(t *testing.T, table *SqlTable, want ...[]any) {
	t.Helper()
	if want == nil {
		want = [][]any{}
	}
	assert.Equal(t, want, fileRows(t, table))
}

// assertTableRowsAnyOrder checks the table holds exactly want, in any order.
// An UPDATE writes the new version of a row at the end of the file, so after
// one the order is unspecified.
func assertTableRowsAnyOrder(t *testing.T, table *SqlTable, want ...[]any) {
	t.Helper()
	assert.ElementsMatch(t, want, fileRows(t, table))
}

// newTestStoreAt opens a catalog store over dir and closes it when the test ends.
func newTestStoreAt(t *testing.T, dir string) *catalogStore {
	t.Helper()
	files, err := newFileAllocator(DataDir(dir))
	require.NoError(t, err)
	store, err := openCatalogStore(files)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestStore(t *testing.T) *catalogStore { return newTestStoreAt(t, t.TempDir()) }

var errInjected = errors.New("injected i/o failure")

// failingFile wraps a storage.File and fails the operations a test turns on.
type failingFile struct {
	storage.File
	failInserts bool
	failSyncs   bool
	failDeletes int // fail this many Delete calls, then behave
}

func (f *failingFile) Insert(b []byte) (storage.RowID, error) {
	if f.failInserts {
		return storage.RowID{}, errInjected
	}
	return f.File.Insert(b)
}

func (f *failingFile) Sync() error {
	if f.failSyncs {
		return errInjected
	}
	return f.File.Sync()
}

func (f *failingFile) Delete(id storage.RowID) error {
	if f.failDeletes > 0 {
		f.failDeletes--
		return errInjected
	}
	return f.File.Delete(id)
}

// failSysTables and failSysDatabases wrap the store's system files so a test
// can make catalog writes fail.
func failSysTables(t *testing.T, store *catalogStore) *failingFile {
	t.Helper()
	ff := &failingFile{File: store.tables}
	store.tables = ff
	return ff
}

func failSysDatabases(t *testing.T, store *catalogStore) *failingFile {
	t.Helper()
	ff := &failingFile{File: store.databases}
	store.databases = ff
	return ff
}

func sysDatabaseNames(t *testing.T, store *catalogStore) []string {
	t.Helper()
	names := []string{}
	for row, err := range store.databases.Scan() {
		require.NoError(t, err)
		name, err := decodeDatabaseName(row.Bytes)
		require.NoError(t, err)
		names = append(names, name)
	}
	return names
}

func sysTableRecords(t *testing.T, store *catalogStore) []tableRecord {
	t.Helper()
	records := []tableRecord{}
	for row, err := range store.tables.Scan() {
		require.NoError(t, err)
		record, err := decodeTableRecord(row.Bytes)
		require.NoError(t, err)
		records = append(records, record)
	}
	return records
}

func mustEncodeTableRow(t *testing.T, database, name string, fileID int64, columns []parser.ColumnDefinition) []byte {
	t.Helper()
	row, err := encodeTableRow(database, name, fileID, columns)
	require.NoError(t, err)
	return row
}

func mustSchema(t *testing.T, columns []parser.ColumnDefinition) string {
	t.Helper()
	s, err := EncodeSchema(columns)
	require.NoError(t, err)
	return s
}

func mustExec(t *testing.T, e Engine, database, sql string) Response {
	t.Helper()
	statement, err := parser.NewParser(parser.NewLexer()).Parse(sql)
	require.NoError(t, err, sql)
	resp, err := e.Execute(statement, database)
	require.NoError(t, err, sql)
	return resp
}

func execError(t *testing.T, e Engine, database, sql string) error {
	t.Helper()
	statement, err := parser.NewParser(parser.NewLexer()).Parse(sql)
	require.NoError(t, err, sql)
	_, err = e.Execute(statement, database)
	return err
}

// restart closes old and opens a new catalog over the same data directory,
// the way a server restart does.
func restart(t *testing.T, dir string, old Catalog) (Catalog, Engine) {
	t.Helper()
	require.NoError(t, old.Close())
	c := newTestCatalogAt(t, dir)
	return c, NewEngine(c)
}
