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

// catalogStore owns the three system files and remembers where each live row
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
	indexes   storage.File
	indexRows map[indexKey]storage.RowID
	// staleIndexRows are index rows that should be gone but whose tombstone, or
	// the sync after it, failed. Nothing in memory points at them, so no sweep
	// would find them, and once a table of that name has the column startup
	// cannot tell them from live rows: they are retried before the next index
	// operation, which is refused while the retry fails.
	staleIndexRows []storage.RowID
	closed         bool
}

func newCatalogStore(files *fileAllocator, databases, tables, indexes storage.File) *catalogStore {
	return &catalogStore{
		files:     files,
		databases: databases,
		tables:    tables,
		indexes:   indexes,
		dbRows:    make(map[string]storage.RowID),
		tableRows: make(map[tableKey]storage.RowID),
		indexRows: make(map[indexKey]storage.RowID),
	}
}

// openCatalogStore opens <data_dir>/sys/sys_databases.tbl, sys_tables.tbl and
// sys_indexes.tbl, creating whichever is missing. It does not read their rows.
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
	indexes, err := openOrCreate(filepath.Join(dir, sysIndexesFile))
	if err != nil {
		_ = databases.Close()
		_ = tables.Close()
		return nil, err
	}
	return newCatalogStore(files, databases, tables, indexes), nil
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

// Close closes the three system files. It is idempotent.
func (s *catalogStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.databases.Close(), s.tables.Close(), s.indexes.Close())
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
	errs = append(errs, s.tombstoneIndexRowsLocked(func(k indexKey) bool { return k.database == database }))
	return errors.Join(errs...)
}

func (s *catalogStore) addTable(database, name string, row []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.tombstoneIndexRowsLocked(func(k indexKey) bool { return k.database == database && k.table == name }); err != nil {
		return err
	}
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

const sysIndexesFile = "sys_indexes.tbl"

var sysIndexesColumns = []parser.ColumnDefinition{
	{Name: "db", DataType: parser.TextDataType{}},
	{Name: "tbl", DataType: parser.TextDataType{}},
	{Name: "name", DataType: parser.TextDataType{}},
	{Name: "col", DataType: parser.TextDataType{}},
}

type indexKey struct{ database, table, name string }

// indexRecord is one decoded sys_indexes row.
type indexRecord struct {
	database, table, name, column string
}

func encodeIndexRow(r indexRecord) ([]byte, error) {
	row, err := encodeRowChecked(sysIndexesColumns, []any{r.database, r.table, r.name, r.column})
	if err != nil {
		return nil, fmt.Errorf("index %q: %w", r.name, err)
	}
	return row, nil
}

func decodeIndexRecord(b []byte) (indexRecord, error) {
	values, err := DecodeRow(sysIndexesColumns, b)
	if err != nil {
		return indexRecord{}, fmt.Errorf("decoding sys_indexes row: %w", err)
	}
	database, ok1 := values[0].(string)
	table, ok2 := values[1].(string)
	name, ok3 := values[2].(string)
	column, ok4 := values[3].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return indexRecord{}, errors.New("decoding sys_indexes row: a column is NULL")
	}
	return indexRecord{database, table, name, column}, nil
}

// addIndex makes a definition durable; the row is the commit point.
func (s *catalogStore) addIndex(database, table, name, column string) error {
	row, err := encodeIndexRow(indexRecord{database, table, name, column})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.retryStaleLocked(); err != nil {
		return err
	}
	id, err := insertSynced(s.indexes, row)
	if err != nil {
		return err
	}
	s.indexRows[indexKey{database, table, name}] = id
	return nil
}

func (s *catalogStore) removeIndex(database, table, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.retryStaleLocked(); err != nil {
		return err
	}
	key := indexKey{database, table, name}
	id, ok := s.indexRows[key]
	if !ok {
		return fmt.Errorf("index %q has no catalog row", name)
	}
	if err := deleteSynced(s.indexes, id); err != nil {
		return err
	}
	delete(s.indexRows, key)
	return nil
}

// retryStaleLocked tombstones the rows in staleIndexRows. It keeps the whole
// list until every row is gone and the sync has succeeded.
func (s *catalogStore) retryStaleLocked() error {
	if len(s.staleIndexRows) == 0 {
		return nil
	}
	var errs []error
	for _, id := range s.staleIndexRows {
		if _, err := s.indexes.Get(id); errors.Is(err, storage.ErrRowNotFound) {
			continue
		}
		if err := s.indexes.Delete(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := s.indexes.Sync(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("removing stale index rows: %w", err)
	}
	s.staleIndexRows = nil
	return nil
}

// tombstoneBestEffortLocked tombstones ids and syncs. A row it cannot remove, or
// every row if the sync fails, goes on the stale list to be retried.
func (s *catalogStore) tombstoneBestEffortLocked(ids []storage.RowID) {
	var gone []storage.RowID
	for _, id := range ids {
		if err := s.indexes.Delete(id); err != nil {
			s.staleIndexRows = append(s.staleIndexRows, id)
			continue
		}
		gone = append(gone, id)
	}
	if len(gone) > 0 {
		if err := s.indexes.Sync(); err != nil {
			s.staleIndexRows = append(s.staleIndexRows, gone...)
		}
	}
}

// tombstoneIndexRowsLocked tombstones every known index row for which match is
// true. A row whose tombstone fails stays in the map so a later sweep retries it.
func (s *catalogStore) tombstoneIndexRowsLocked(match func(indexKey) bool) error {
	if err := s.retryStaleLocked(); err != nil {
		return err
	}
	var errs []error
	var done []storage.RowID
	for key, id := range s.indexRows {
		if !match(key) {
			continue
		}
		if err := s.indexes.Delete(id); err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", key.name, err))
			continue
		}
		delete(s.indexRows, key)
		done = append(done, id)
	}
	if len(done) > 0 {
		if err := s.indexes.Sync(); err != nil {
			errs = append(errs, err)
			s.staleIndexRows = append(s.staleIndexRows, done...)
		}
	}
	return errors.Join(errs...)
}

// removeTableIndexes tombstones every index row of one table.
func (s *catalogStore) removeTableIndexes(database, table string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tombstoneIndexRowsLocked(func(k indexKey) bool { return k.database == database && k.table == table })
}

// stagedIndexRows is the new copy of a table's index rows, written before the
// table row changes. old holds the rows to tombstone once it has.
type stagedIndexRows struct {
	database, table, newTable string
	old                       map[indexKey]storage.RowID
	added                     map[indexKey]storage.RowID
}

// stageIndexRows writes copies of table's index rows under newTable, with each
// column renamed by renames (a nil map renames none), and syncs them. The
// originals stay until commitStaged. Rows left under newTable by an earlier,
// dropped table are swept first, so they never attach to this one.
func (s *catalogStore) stageIndexRows(database, table, newTable string, renames map[string]string) (*stagedIndexRows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Rows left under newTable by an earlier, dropped table are swept first,
	// also when this table has no index rows to copy.
	if newTable != table {
		if err := s.tombstoneIndexRowsLocked(func(k indexKey) bool { return k.database == database && k.table == newTable }); err != nil {
			return nil, err
		}
	}
	if err := s.retryStaleLocked(); err != nil {
		return nil, err
	}
	staged := &stagedIndexRows{database: database, table: table, newTable: newTable,
		old: map[indexKey]storage.RowID{}, added: map[indexKey]storage.RowID{}}
	for key, id := range s.indexRows {
		if key.database == database && key.table == table {
			staged.old[key] = id
		}
	}
	if len(staged.old) == 0 {
		return staged, nil
	}
	cleanup := func() {
		ids := make([]storage.RowID, 0, len(staged.added))
		for _, id := range staged.added {
			ids = append(ids, id)
		}
		s.tombstoneBestEffortLocked(ids)
	}
	for key, id := range staged.old {
		b, err := s.indexes.Get(id)
		if err != nil {
			cleanup()
			return nil, err
		}
		record, err := decodeIndexRecord(b)
		if err != nil {
			cleanup()
			return nil, err
		}
		if renamed, ok := renames[record.column]; ok {
			record.column = renamed
		}
		record.table = newTable
		row, err := encodeIndexRow(record)
		if err != nil {
			cleanup()
			return nil, err
		}
		newID, err := s.indexes.Insert(row)
		if err != nil {
			cleanup()
			return nil, err
		}
		staged.added[indexKey{database, newTable, key.name}] = newID
	}
	if err := s.indexes.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	return staged, nil
}

// commitStaged tombstones the originals and makes the copies the live rows. A
// tombstone that fails is retried before the next index operation: until then
// the original is a duplicate of a live row or names a table that no longer
// exists, and the load-time check drops it only in the second case.
func (s *catalogStore) commitStaged(staged *stagedIndexRows) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]storage.RowID, 0, len(staged.old))
	for key, id := range staged.old {
		ids = append(ids, id)
		delete(s.indexRows, key)
	}
	s.tombstoneBestEffortLocked(ids)
	for key, id := range staged.added {
		s.indexRows[key] = id
	}
}

// abortStaged tombstones the copies; the originals are untouched.
func (s *catalogStore) abortStaged(staged *stagedIndexRows) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]storage.RowID, 0, len(staged.added))
	for _, id := range staged.added {
		ids = append(ids, id)
	}
	s.tombstoneBestEffortLocked(ids)
}
