package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

const (
	sysDirName       = "sys"
	sysDatabasesFile = "sys_databases.tbl"
	sysTablesFile    = "sys_tables.tbl"
)

// The catalog's own tables have hard-coded schemas: they are the bootstrap
// that every other schema is read through.
var (
	sysDatabasesColumns = []parser.ColumnDefinition{
		{Name: "name", DataType: parser.TextDataType{}},
	}
	sysTablesColumns = []parser.ColumnDefinition{
		{Name: "db", DataType: parser.TextDataType{}},
		{Name: "name", DataType: parser.TextDataType{}},
		{Name: "file_id", DataType: parser.BigIntDataType{}},
		{Name: "schema", DataType: parser.TextDataType{}},
	}
)

type tableKey struct{ database, name string }

// tableRecord is one decoded sys_tables row.
type tableRecord struct {
	database string
	name     string
	fileID   int64
	schema   string
}

// catalogStore owns the two system files and remembers where each live row
// is, so a row can be tombstoned without scanning. mu is the last lock in the
// engine's lock order: it is held only around one catalog write, and nothing
// in here calls back into a database or table.
type catalogStore struct {
	mu        sync.Mutex
	files     *fileAllocator
	databases storage.File
	tables    storage.File
	dbRows    map[string]storage.RowID
	tableRows map[tableKey]storage.RowID
	closed    bool
}

func newCatalogStore(files *fileAllocator, databases, tables storage.File) *catalogStore {
	return &catalogStore{
		files:     files,
		databases: databases,
		tables:    tables,
		dbRows:    make(map[string]storage.RowID),
		tableRows: make(map[tableKey]storage.RowID),
	}
}

// openCatalogStore opens <data_dir>/sys/sys_databases.tbl and sys_tables.tbl,
// creating whichever is missing. It does not read their rows.
func openCatalogStore(files *fileAllocator) (*catalogStore, error) {
	dir := filepath.Join(string(files.dataDir), sysDirName)
	if err := files.makeDir(dir); err != nil {
		return nil, fmt.Errorf("creating catalog directory: %w", err)
	}
	databases, err := openOrCreate(filepath.Join(dir, sysDatabasesFile))
	if err != nil {
		return nil, err
	}
	tables, err := openOrCreate(filepath.Join(dir, sysTablesFile))
	if err != nil {
		_ = databases.Close()
		return nil, err
	}
	return newCatalogStore(files, databases, tables), nil
}

func openOrCreate(path string) (storage.File, error) {
	_, err := os.Stat(path)
	if err == nil {
		return storage.OpenFile(path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return storage.CreateFile(path)
}

// Close closes both files. It is idempotent.
func (s *catalogStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.databases.Close(), s.tables.Close())
}

// encodeTableRow builds a sys_tables row. Callers do this before creating any
// file, so a schema too large for a page fails before anything exists on disk.
func encodeTableRow(database, name string, fileID int64, columns []parser.ColumnDefinition) ([]byte, error) {
	schema, err := EncodeSchema(columns)
	if err != nil {
		return nil, fmt.Errorf("table %q: %w", name, err)
	}
	row, err := encodeRowChecked(sysTablesColumns, []any{database, name, fileID, schema})
	if err != nil {
		return nil, fmt.Errorf("schema of table %q is too large to store: %w", name, err)
	}
	return row, nil
}

func decodeDatabaseName(b []byte) (string, error) {
	values, err := DecodeRow(sysDatabasesColumns, b)
	if err != nil {
		return "", fmt.Errorf("decoding sys_databases row: %w", err)
	}
	name, ok := values[0].(string)
	if !ok {
		return "", errors.New("decoding sys_databases row: database name is NULL")
	}
	return name, nil
}

func decodeTableRecord(b []byte) (tableRecord, error) {
	values, err := DecodeRow(sysTablesColumns, b)
	if err != nil {
		return tableRecord{}, fmt.Errorf("decoding sys_tables row: %w", err)
	}
	database, ok1 := values[0].(string)
	name, ok2 := values[1].(string)
	fileID, ok3 := values[2].(int64)
	schema, ok4 := values[3].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return tableRecord{}, errors.New("decoding sys_tables row: a column is NULL")
	}
	return tableRecord{database: database, name: name, fileID: fileID, schema: schema}, nil
}

// insertSynced makes row durable. If the sync fails the row is tombstoned on a
// best-effort basis, so a change the caller was told failed is not left behind.
func insertSynced(file storage.File, row []byte) (storage.RowID, error) {
	id, err := file.Insert(row)
	if err != nil {
		return storage.RowID{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Delete(id)
		return storage.RowID{}, err
	}
	return id, nil
}

func deleteSynced(file storage.File, id storage.RowID) error {
	if err := file.Delete(id); err != nil {
		return err
	}
	return file.Sync()
}

// addDatabase records a new database. Any stale sys_tables rows still under
// this name (left by a DROP DATABASE whose table-row tombstones failed) are
// tombstoned first, so the new database never inherits the old one's tables.
func (s *catalogStore) addDatabase(name string) error {
	row, err := encodeRowChecked(sysDatabasesColumns, []any{name})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.tombstoneTablesLocked(name); err != nil {
		return err
	}
	id, err := insertSynced(s.databases, row)
	if err != nil {
		return err
	}
	s.dbRows[name] = id
	return nil
}

// removeDatabase tombstones the database's row. It is the commit point of
// DROP DATABASE: once it returns nil the database is gone after a restart.
func (s *catalogStore) removeDatabase(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.dbRows[name]
	if !ok {
		return fmt.Errorf("database %q has no catalog row", name)
	}
	if err := deleteSynced(s.databases, id); err != nil {
		return err
	}
	delete(s.dbRows, name)
	return nil
}

// removeDatabaseTables tombstones every table row of database. Rows whose
// tombstone fails stay in the index so addDatabase can sweep them later.
func (s *catalogStore) removeDatabaseTables(database string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tombstoneTablesLocked(database)
}

func (s *catalogStore) tombstoneTablesLocked(database string) error {
	var errs []error
	removed := false
	for key, id := range s.tableRows {
		if key.database != database {
			continue
		}
		if err := s.tables.Delete(id); err != nil {
			errs = append(errs, fmt.Errorf("table %q: %w", key.name, err))
			continue
		}
		delete(s.tableRows, key)
		removed = true
	}
	if removed {
		if err := s.tables.Sync(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *catalogStore) addTable(database, name string, row []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := insertSynced(s.tables, row)
	if err != nil {
		return err
	}
	s.tableRows[tableKey{database, name}] = id
	return nil
}

func (s *catalogStore) removeTable(database, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := tableKey{database, name}
	id, ok := s.tableRows[key]
	if !ok {
		return fmt.Errorf("table %q has no catalog row", name)
	}
	if err := deleteSynced(s.tables, id); err != nil {
		return err
	}
	delete(s.tableRows, key)
	return nil
}

// replaceTable swaps a table's row for newRow (a rename, or a new file id and
// schema). The new row is made durable before the old one is tombstoned: with
// one sync a power loss could keep the tombstone but not the insert, and the
// table would vanish from the catalog. If the tombstone fails, the new row is
// tombstoned best-effort so the catalog does not apply a change the caller
// was told failed.
func (s *catalogStore) replaceTable(database, oldName, newName string, newRow []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldKey := tableKey{database, oldName}
	oldID, ok := s.tableRows[oldKey]
	if !ok {
		return fmt.Errorf("table %q has no catalog row", oldName)
	}
	newID, err := insertSynced(s.tables, newRow)
	if err != nil {
		return err
	}
	if err := deleteSynced(s.tables, oldID); err != nil {
		_ = deleteSynced(s.tables, newID)
		return err
	}
	delete(s.tableRows, oldKey)
	s.tableRows[tableKey{database, newName}] = newID
	return nil
}
