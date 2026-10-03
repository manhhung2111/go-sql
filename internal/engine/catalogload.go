package engine

import (
	"errors"
	"fmt"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// loadedTable is one table read back from sys_tables, with its file open.
type loadedTable struct {
	name    string
	fileID  int64
	columns []parser.ColumnDefinition
	path    string
	file    storage.File
}

type loadedCatalog struct {
	// databases maps every live database to its tables (possibly none).
	databases map[string][]loadedTable
}

// survivor is a sys_tables row that is still live while the scan resolves
// duplicates.
type survivor struct {
	record tableRecord
	id     storage.RowID
	dead   bool
}

// load reads the system tables (spec 6.3 steps 3 to 5) and opens every
// surviving table's file. It never deletes a table file: that is cleanup's
// job, once every survivor is known. On any error every file it opened is
// closed again.
func (s *catalogStore) load() (*loadedCatalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Databases. A repeated name keeps the first row.
	var staleDatabases []storage.RowID
	for row, err := range s.databases.Scan() {
		if err != nil {
			return nil, fmt.Errorf("reading sys_databases: %w", err)
		}
		name, err := decodeDatabaseName(row.Bytes)
		if err != nil {
			return nil, err
		}
		if _, dup := s.dbRows[name]; dup {
			staleDatabases = append(staleDatabases, row.ID)
			continue
		}
		s.dbRows[name] = row.ID
	}

	// Tables. A later row displaces any current survivor that shares its
	// (db, name) or its file_id. Displacement is checked against survivors,
	// not against every earlier row: rows [a,f1] [b,f1] [a,f2] leave b on f1
	// and a on f2, and f1 must stay because b uses it.
	byName := map[tableKey]*survivor{}
	byID := map[int64]*survivor{}
	var all []*survivor
	var staleTables []storage.RowID
	displace := func(old *survivor) {
		if old == nil || old.dead {
			return
		}
		old.dead = true
		staleTables = append(staleTables, old.id)
		if key := (tableKey{old.record.database, old.record.name}); byName[key] == old {
			delete(byName, key)
		}
		if byID[old.record.fileID] == old {
			delete(byID, old.record.fileID)
		}
	}
	for row, err := range s.tables.Scan() {
		if err != nil {
			return nil, fmt.Errorf("reading sys_tables: %w", err)
		}
		record, err := decodeTableRecord(row.Bytes)
		if err != nil {
			return nil, err
		}
		if _, ok := s.dbRows[record.database]; !ok {
			staleTables = append(staleTables, row.ID)
			continue
		}
		key := tableKey{record.database, record.name}
		displace(byName[key])
		displace(byID[record.fileID])
		sv := &survivor{record: record, id: row.ID}
		byName[key], byID[record.fileID] = sv, sv
		all = append(all, sv)
	}

	// Tombstone the losers, after the scans so no page is rewritten while it
	// is being read.
	if err := tombstoneAll(s.databases, staleDatabases); err != nil {
		return nil, err
	}
	if err := tombstoneAll(s.tables, staleTables); err != nil {
		return nil, err
	}

	loaded := &loadedCatalog{databases: make(map[string][]loadedTable, len(s.dbRows))}
	for name := range s.dbRows {
		loaded.databases[name] = nil
	}
	closeAll := func() {
		for _, tables := range loaded.databases {
			for _, table := range tables {
				_ = table.file.Close()
			}
		}
	}
	for _, sv := range all {
		if sv.dead {
			continue
		}
		record := sv.record
		columns, err := DecodeSchema(record.schema)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("table %q in database %q: %w", record.name, record.database, err)
		}
		path := s.files.tablePath(record.database, record.fileID)
		file, err := storage.OpenFile(path)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("table %q in database %q: its file is missing or damaged: %w", record.name, record.database, err)
		}
		s.tableRows[tableKey{record.database, record.name}] = sv.id
		loaded.databases[record.database] = append(loaded.databases[record.database], loadedTable{
			name: record.name, fileID: record.fileID, columns: columns, path: path, file: file,
		})
	}
	return loaded, nil
}

func tombstoneAll(file storage.File, ids []storage.RowID) error {
	if len(ids) == 0 {
		return nil
	}
	var errs []error
	for _, id := range ids {
		if err := file.Delete(id); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("tombstoning stale catalog rows: %w", err)
	}
	return file.Sync()
}
