package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
	"os"
	"slices"
	"sync"
)

type Table interface {
	InsertValues(columns []string, values [][]parser.Token) error
	Select(columns []string, where parser.Expression) (Response, error)
	AlterColumns(action parser.AlterAction) error
	Delete(where parser.Expression) error
	Update(assignments []parser.Assignment, where parser.Expression) error
	// Rename records the new name in the catalog, then sets it.
	Rename(name string) error
	// Close releases the table's file. It is idempotent.
	Close() error
	// Drop closes the table's file and deletes it. It is idempotent.
	Drop() error
}

type SqlTable struct {
	mu      sync.RWMutex
	Name    string
	Columns []parser.ColumnDefinition

	// path and file are the table's heap file: the only copy of its rows.
	// Every statement reads and writes it under mu, and none holds a whole
	// table in memory.
	path   string
	file   storage.File
	fileID int64

	// store records the table's schema and file in the catalog, and its files
	// allocator names a replacement file when a schema change rewrites rows.
	store    *catalogStore
	database string

	// idx caches indexes of the file; see tableIndexes. noIndexes turns them
	// off so tests can compare behaviour with and without.
	idx       tableIndexes
	noIndexes bool

	// indexDefs are the CREATE INDEX definitions of the table, guarded by mu.
	indexDefs []indexDef
}

// Rules when for a table schema:
//   - Not yet supported the multi-column primary key
//   - There is only 1 primary key column in a table, and the table can have multiple columns with constrants (NOT NULL & UNIQUE)
//   - Columns do not specify with contraint NOT NULL will default be NULLABLE
//   - A table can not have duplicated columns
//
// The schema is validated before anything is created on disk, so a rejected
// schema never leaves a file or directory behind. The table's file is a new,
// empty file in database's directory.
func NewTable(name string, columns []parser.ColumnDefinition, store *catalogStore, database string) (*SqlTable, error) {
	validated := make([]parser.ColumnDefinition, 0, len(columns))
	for _, column := range columns {
		if err := validateNewColumn(name, validated, column); err != nil {
			return nil, err
		}
		validated = append(validated, column)
	}

	// The catalog row is encoded before any file exists, so a schema too
	// large for a page fails without leaving anything behind.
	id := store.files.nextFileID()
	row, err := encodeTableRow(database, name, id, validated)
	if err != nil {
		return nil, err
	}
	tf, err := createTableFile(store.files, database, id)
	if err != nil {
		return nil, err
	}
	if err := store.addTable(database, name, row); err != nil {
		discardFile(tf)
		return nil, err
	}

	return &SqlTable{
		Name: name, Columns: columns,
		path: tf.path, file: tf.file, fileID: tf.id, store: store, database: database,
	}, nil
}

// tableFile is a table's heap file together with the id its name carries.
type tableFile struct {
	id   int64
	path string
	file storage.File
}

// createTableFile makes a new, empty table file in database's directory,
// creating the directory first if it is missing: names that differ only by
// case share one directory on a case-insensitive filesystem, so dropping one
// can remove a directory another database still uses. The file is created
// exclusively, so it can never adopt an old file's rows.
func createTableFile(files *fileAllocator, database string, id int64) (tableFile, error) {
	if err := files.makeDir(files.databaseDir(database)); err != nil {
		return tableFile{}, fmt.Errorf("creating directory for database %q: %w", database, err)
	}

	path := files.tablePath(database, id)
	file, err := storage.CreateFile(path)
	if err != nil {
		return tableFile{}, err
	}
	return tableFile{id: id, path: path, file: file}, nil
}

// validateNewColumn checks column against a table's existing columns for
// the invariants a table's column set must maintain: no duplicate name,
// at most one PRIMARY KEY, and a DEFAULT (if any) that coerces against
// its own type — validated eagerly so a broken DEFAULT fails here rather
// than deferring to whoever triggers the first INSERT. Shared by NewTable
// (existing grows as it validates each column) and ADD COLUMN (existing
// is the table's current columns).
func validateNewColumn(tableName string, existing []parser.ColumnDefinition, column parser.ColumnDefinition) error {
	for _, e := range existing {
		if e.Name == column.Name {
			return fmt.Errorf("column name %q already exists", column.Name)
		}
	}

	if column.IsPrimaryKey() {
		for _, e := range existing {
			if e.IsPrimaryKey() {
				return fmt.Errorf("table %s has more than one primary key column", tableName)
			}
		}
	}

	if def, ok := column.DefaultValue(); ok {
		if _, err := coerceValue(column.DataType, def); err != nil {
			return fmt.Errorf("column %q: invalid default value: %v", column.Name, err)
		}
	}

	return nil
}

// InsertValues appends rows to the table's file, coercing each value against
// its column's DataType and enforcing NOT NULL/PRIMARY KEY/UNIQUE.
func (t *SqlTable) InsertValues(columns []string, values [][]parser.Token) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Phase 1: resolve the statement-level mapping once — identical for
	// every row in this batch, so it's wasted work (and a correctness
	// risk) to redo it per row.
	targetColumns := columns
	if len(targetColumns) == 0 {
		targetColumns = make([]string, len(t.Columns))
		for i, col := range t.Columns {
			targetColumns[i] = col.Name
		}
	}

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	targetIndices := make([]int, len(targetColumns))
	provided := make([]bool, len(t.Columns))
	seen := make(map[string]bool, len(targetColumns))
	for i, name := range targetColumns {
		idx, ok := columnIndex[name]
		if !ok {
			return fmt.Errorf("unknown column %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate column %q in column list", name)
		}
		seen[name] = true
		targetIndices[i] = idx
		provided[idx] = true
	}

	// Fillers for columns not covered by the insert's column list, resolved
	// once — safe to reuse across rows because DefaultConstraint.DefaultValue
	// is a static literal Token here, not a dynamic expression. Coerced
	// once here too, for the same reason.
	fillers := make(map[int]any)
	for i, col := range t.Columns {
		if provided[i] {
			continue
		}
		if def, ok := col.DefaultValue(); ok {
			coerced, err := coerceValue(col.DataType, def)
			if err != nil {
				return fmt.Errorf("column %q: %v", col.Name, err)
			}
			fillers[i] = coerced
			continue
		}
		if col.RequiresValue() {
			return fmt.Errorf("field %q doesn't have a default value", col.Name)
		}
		fillers[i] = nil
	}

	uniqueColumns := make([]int, 0)
	for i, col := range t.Columns {
		if col.IsPrimaryKey() || col.IsUnique() {
			uniqueColumns = append(uniqueColumns, i)
		}
	}

	// Phase 2: build every row into a fresh buffer, stopping at the first row
	// that cannot be built. That error is held back rather than returned: a
	// duplicate key in an earlier row has always been reported before a later
	// row's error, because rows used to be checked one at a time, and still is.
	newRows := make([][]any, 0, len(values))
	var rowErr error
	for _, row := range values {
		rowValues, err := t.buildRow(row, targetColumns, targetIndices, fillers)
		if err != nil {
			rowErr = err
			break
		}
		newRows = append(newRows, rowValues)
	}

	// Phase 3: PRIMARY KEY/UNIQUE, against the rows already in the file and
	// against earlier rows of this batch.
	if err := t.checkUniqueness(newRows, uniqueColumns); err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}

	// Phase 4: write the whole batch; nothing is written unless every row is
	// good, so a failure never leaves the table half-inserted.
	return t.appendToFile(newRows)
}

// buildRow validates one row of an INSERT, coerces its values against their
// columns' DataTypes, and fills in the columns the insert did not name.
func (t *SqlTable) buildRow(row []parser.Token, targetColumns []string, targetIndices []int, fillers map[int]any) ([]any, error) {
	if len(row) != len(targetColumns) {
		return nil, fmt.Errorf("expected %d values, got %d", len(targetColumns), len(row))
	}

	rowValues := make([]any, len(t.Columns))
	for j, tok := range row {
		idx := targetIndices[j]
		coerced, err := coerceValue(t.Columns[idx].DataType, tok)
		if err != nil {
			return nil, fmt.Errorf("column %q: %v", t.Columns[idx].Name, err)
		}
		rowValues[idx] = coerced
	}
	for idx, filler := range fillers {
		rowValues[idx] = filler // already coerced once, in phase 1
	}
	return rowValues, nil
}

// checkUniqueness reports the first PRIMARY KEY/UNIQUE violation among
// newRows, checked in row order against the rows already in the table and
// against the earlier rows of the batch, and rejects a string too long to be
// indexed at the point where a duplicate would be reported. A column with an
// index is checked by looking each value up; a column without one (indexes
// are off, or it could not be indexed) streams the file once against the
// statement's own key values, so memory is proportional to the statement.
// NULLs never conflict.
func (t *SqlTable) checkUniqueness(newRows [][]any, uniqueColumns []int) error {
	candidates := make(map[int]map[any]bool, len(uniqueColumns))
	anyCandidate := false
	for _, idx := range uniqueColumns {
		set := make(map[any]bool, len(newRows))
		for _, row := range newRows {
			if v := row[idx]; v != nil {
				set[v] = true
				anyCandidate = true
			}
		}
		candidates[idx] = set
	}
	if !anyCandidate {
		return nil
	}

	trees, err := t.indexes()
	if err != nil {
		return err
	}
	scanned := make(map[int]map[any]bool)
	for _, idx := range uniqueColumns {
		if trees[idx] == nil {
			scanned[idx] = candidates[idx]
		}
	}
	seen := make(map[int]map[any]bool, len(uniqueColumns))
	if len(scanned) > 0 {
		if seen, err = t.existingKeys(scanned); err != nil {
			return err
		}
	}
	for _, idx := range uniqueColumns {
		if seen[idx] == nil {
			seen[idx] = make(map[any]bool)
		}
	}

	for _, row := range newRows {
		for _, idx := range uniqueColumns {
			value := row[idx]
			if value == nil {
				continue // multiple NULLs are allowed in a UNIQUE column
			}
			if err := checkKeyLength(t.Columns[idx].Name, value); err != nil {
				return err
			}
			duplicate := seen[idx][value]
			if tree := trees[idx]; tree != nil && !duplicate {
				key, err := index.EncodeKey(value)
				if err != nil {
					return err
				}
				_, duplicate = tree.Get(key)
			}
			if duplicate {
				return fmt.Errorf("duplicate entry %v for column %q", value, t.Columns[idx].Name)
			}
			seen[idx][value] = true
		}
	}
	return nil
}

// existingKeys streams the table's file once and returns, per unique column,
// which of the candidate values some existing row already holds.
func (t *SqlTable) existingKeys(candidates map[int]map[any]bool) (map[int]map[any]bool, error) {
	if t.file == nil {
		return nil, fmt.Errorf("table %q is closed", t.Name)
	}

	found := make(map[int]map[any]bool, len(candidates))
	for idx := range candidates {
		found[idx] = make(map[any]bool)
	}
	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return nil, err
		}
		for idx, wanted := range candidates {
			if v := row[idx]; v != nil && wanted[v] {
				found[idx][v] = true
			}
		}
	}
	return found, nil
}

// appendToFile writes rows to the table's file with a single fsync and then
// records them in the table's indexes. Every row is encoded and size-checked
// before the first write, so a bad batch writes nothing; only an I/O error can
// leave it partly written, and the engine has no WAL to undo that. Such an
// error discards the indexes, so they are rebuilt from whatever the file now
// holds. Callers hold t.mu.
func (t *SqlTable) appendToFile(rows [][]any) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	encoded, err := encodeRows(t.Columns, rows)
	if err != nil {
		return err
	}

	ids := make([]storage.RowID, 0, len(encoded))
	for _, b := range encoded {
		id, err := t.file.Insert(b)
		if err != nil {
			t.dropIndexes()
			return err
		}
		ids = append(ids, id)
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexInserted(ids, rows)
	return nil
}

// encodeRows encodes every row with columns and rejects any that cannot fit
// on a page, before the caller writes anything.
func encodeRows(columns []parser.ColumnDefinition, rows [][]any) ([][]byte, error) {
	encoded := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := encodeRowChecked(columns, row)
		if err != nil {
			return nil, err
		}
		encoded[i] = b
	}
	return encoded, nil
}

// encodeRowChecked encodes one row and rejects it if it cannot fit on a page.
func encodeRowChecked(columns []parser.ColumnDefinition, row []any) ([]byte, error) {
	b, err := EncodeRow(columns, row)
	if err != nil {
		return nil, err
	}
	if len(b) > storage.MaxRowSize {
		return nil, fmt.Errorf("row too large: %d bytes exceeds the %d-byte limit", len(b), storage.MaxRowSize)
	}
	return b, nil
}

// rebuildFile builds a replacement for the table's file: a new file in the
// table's database directory holding every live row of the current file, each
// passed through transform and encoded with columns, then fsynced. Rows are
// streamed one at a time, so memory does not grow with the table. Nothing
// about the table changes, and on any failure the partial file is removed.
// Callers hold t.mu, and t.Columns must still be the schema of the current
// file, which is what its rows are decoded with.
func (t *SqlTable) rebuildFile(id int64, columns []parser.ColumnDefinition, transform func(row []any) []any) (tableFile, error) {
	if t.file == nil {
		return tableFile{}, fmt.Errorf("table %q is closed", t.Name)
	}

	tf, err := createTableFile(t.store.files, t.database, id)
	if err != nil {
		return tableFile{}, err
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			discardFile(tf)
			return tableFile{}, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			discardFile(tf)
			return tableFile{}, err
		}
		encoded, err := encodeRowChecked(columns, transform(row))
		if err != nil {
			discardFile(tf)
			return tableFile{}, err
		}
		if _, err := tf.file.Insert(encoded); err != nil {
			discardFile(tf)
			return tableFile{}, err
		}
	}
	if err := tf.file.Sync(); err != nil {
		discardFile(tf)
		return tableFile{}, err
	}
	return tf, nil
}

// rebuildAndRecord streams the table into a new file in the new schema and
// records that file and schema in the catalog. Nothing about the table
// changes: callers commit t.Columns and adopt the file only after this
// returns, and on any failure the new file is gone.
func (t *SqlTable) rebuildAndRecord(columns []parser.ColumnDefinition, transform func(row []any) []any) (tableFile, error) {
	id := t.store.files.nextFileID()
	row, err := encodeTableRow(t.database, t.Name, id, columns)
	if err != nil {
		return tableFile{}, err
	}
	tf, err := t.rebuildFile(id, columns, transform)
	if err != nil {
		return tableFile{}, err
	}
	if err := t.store.replaceTable(t.database, t.Name, t.Name, row); err != nil {
		discardFile(tf)
		return tableFile{}, err
	}
	return tf, nil
}

// countRows counts the live rows of the table's file, stopping once it has
// seen limit of them.
func (t *SqlTable) countRows(limit int) (int, error) {
	if t.file == nil {
		return 0, fmt.Errorf("table %q is closed", t.Name)
	}

	n := 0
	for _, err := range t.file.Scan() {
		if err != nil {
			return 0, err
		}
		n++
		if n >= limit {
			break
		}
	}
	return n, nil
}

// adoptFile makes file the table's file and deletes the old one. Callers have
// already committed the new Columns and hold t.mu: if the old file
// cannot be removed the change is still applied and the old file is a
// harmless orphan, so the error says so.
func (t *SqlTable) adoptFile(tf tableFile) error {
	oldPath := t.path
	closeErr := t.closeLocked()

	t.file, t.path, t.fileID = tf.file, tf.path, tf.id

	if err := os.Remove(oldPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("column change applied, but removing the old table file %q failed: %w", oldPath, err))
	}
	return closeErr
}

// discardFile closes and deletes a file that was never adopted.
func discardFile(tf tableFile) {
	_ = tf.file.Close()
	_ = os.Remove(tf.path)
}

// Select performs a full-table scan of the table's file, a page at a time,
// keeping rows where matches (a nil where matches every row), then projects
// each matching row down to columns ("*" expands to every column in schema
// order). Only the matching, projected rows are held in memory. Rows come
// back in the file's order: insertion order, except that an updated row has
// moved to the end. A where that pins or bounds a PRIMARY KEY/UNIQUE column
// with a literal and cannot fail on any row reads just the matching rows
// through the index instead (one row for an equality, at most maxRangeRows for
// a range), in the same order.
func (t *SqlTable) Select(columns []string, where parser.Expression) (Response, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	outputColumns := columns
	if len(outputColumns) == 1 && outputColumns[0] == "*" {
		outputColumns = make([]string, len(t.Columns))
		for i, col := range t.Columns {
			outputColumns[i] = col.Name
		}
	}

	outputIndices := make([]int, len(outputColumns))
	for i, name := range outputColumns {
		idx, ok := columnIndex[name]
		if !ok {
			return Response{}, fmt.Errorf("unknown column %q", name)
		}
		outputIndices[i] = idx
	}

	if t.file == nil {
		return Response{}, fmt.Errorf("table %q is closed", t.Name)
	}

	rows := make([][]string, 0)
	project := func(row []any) {
		projected := make([]string, len(outputIndices))
		for i, idx := range outputIndices {
			projected[i] = stringifyValue(row[idx])
		}
		rows = append(rows, projected)
	}

	// A WHERE that pins or bounds a PRIMARY KEY/UNIQUE column is served
	// from the index; see lookupRows.
	fetched, usable, err := t.lookupRows(where, columnIndex, nil)
	if err != nil {
		return Response{}, err
	}
	if usable {
		for _, f := range fetched {
			project(f.row)
		}
		return Response{Columns: outputColumns, Rows: rows}, nil
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return Response{}, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return Response{}, err
		}
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return Response{}, err
		}
		if !matched {
			continue
		}
		project(row)
	}

	return Response{Columns: outputColumns, Rows: rows}, nil
}

// AlterColumns applies an ADD/DROP/RENAME COLUMN action. RENAME TO is
// handled at the Database level instead, since it changes the map key
// SqlDatabase.Table is keyed by — it never reaches here.
func (t *SqlTable) AlterColumns(action parser.AlterAction) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch a := action.(type) {
	case parser.AddColumnAction:
		return t.addColumn(a.Column)
	case parser.DropColumnAction:
		return t.dropColumn(a.Column)
	case parser.RenameColumnAction:
		return t.renameColumn(a.OldName, a.NewName)
	default:
		return fmt.Errorf("unsupported alter action, got %T", action)
	}
}

// addColumn appends column to the schema and backfills every existing
// row with its value, streaming the file into a replacement in the new
// layout. Reuses the exact same coercion/uniqueness rules InsertValues
// already enforces: a NOT NULL column with no default, or a
// PRIMARY KEY/UNIQUE column with a default backfilled onto 2+ rows,
// fails via the same checks INSERT uses — no new special-casing needed.
func (t *SqlTable) addColumn(column parser.ColumnDefinition) error {
	if err := validateNewColumn(t.Name, t.Columns, column); err != nil {
		return err
	}

	var filler any
	if def, ok := column.DefaultValue(); ok {
		coerced, err := coerceValue(column.DataType, def)
		if err != nil {
			return fmt.Errorf("column %q: %v", column.Name, err)
		}
		filler = coerced
	} else if column.RequiresValue() {
		return fmt.Errorf("field %q doesn't have a default value", column.Name)
	}

	if column.IsPrimaryKey() || column.IsUnique() {
		if err := checkKeyLength(column.Name, filler); err != nil {
			return err
		}
	}

	// One value backfilled onto two or more rows would hold the same key twice.
	if (column.IsPrimaryKey() || column.IsUnique()) && filler != nil {
		rows, err := t.countRows(2)
		if err != nil {
			return err
		}
		if rows >= 2 {
			return fmt.Errorf("duplicate entry %v for column %q", filler, column.Name)
		}
	}

	// Build the replacement file before touching the table, so a failure
	// (a row that no longer fits on a page, an I/O error) changes nothing.
	newColumns := append(append([]parser.ColumnDefinition{}, t.Columns...), column)
	tf, err := t.rebuildAndRecord(newColumns, func(row []any) []any {
		return append(row, filler)
	})
	if err != nil {
		return err
	}

	t.Columns = newColumns
	return t.adoptFile(tf)
}

// dropColumn removes column at its schema index by streaming the file
// into a replacement without that column's value in any row — the schema
// and every stored row must stay aligned, or every subsequent read of a
// row would be reading the wrong column's value.
func (t *SqlTable) dropColumn(name string) error {
	idx := -1
	for i, col := range t.Columns {
		if col.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("unknown column %q", name)
	}
	if len(t.Columns) == 1 {
		return fmt.Errorf("cannot drop the only column")
	}

	newColumns := append(append([]parser.ColumnDefinition{}, t.Columns[:idx]...), t.Columns[idx+1:]...)
	tf, err := t.rebuildAndRecord(newColumns, func(row []any) []any {
		return append(row[:idx:idx], row[idx+1:]...)
	})
	if err != nil {
		return err
	}

	t.Columns = newColumns
	return t.adoptFile(tf)
}

// renameColumn doesn't touch the file — rows are positional, not keyed by
// name, so a rename is purely a schema-metadata change.
func (t *SqlTable) renameColumn(oldName, newName string) error {
	idx := -1
	for i, col := range t.Columns {
		if col.Name == oldName {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("unknown column %q", oldName)
	}

	for _, col := range t.Columns {
		if col.Name == newName {
			return fmt.Errorf("column name %q already exists", newName)
		}
	}

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	// The caller's column slice is never edited in place: the new schema is
	// recorded in the catalog first and only then becomes the table's.
	newColumns := slices.Clone(t.Columns)
	newColumns[idx].Name = newName
	row, err := encodeTableRow(t.database, t.Name, t.fileID, newColumns)
	if err != nil {
		return err
	}
	if err := t.store.replaceTable(t.database, t.Name, t.Name, row); err != nil {
		return err
	}
	t.Columns = newColumns
	return nil
}

// Delete removes every row matching where (a nil where matches every row,
// so a bare DELETE FROM wipes the table) by tombstoning it in the table's
// file; see deleteFromFile.
func (t *SqlTable) Delete(where parser.Expression) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	return t.deleteFromFile(where, columnIndex)
}

// deleteFromFile tombstones every live row of the table's file that matches
// where, then fsyncs once, then removes the rows from the table's indexes. A
// where that pins or bounds a key is served from the index instead of a scan.
// The matching rows are collected before the first tombstone is written, so
// an evaluation error touches nothing, and a row is never tombstoned while
// the scan is still reading its page; only an I/O error can leave the delete
// partly applied, and the engine has no WAL to undo that. Such an error
// discards the indexes. Callers hold t.mu.
func (t *SqlTable) deleteFromFile(where parser.Expression, columnIndex map[string]int) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	// A where that pins or bounds a key is served from the index. That can build the
	// indexes, so whether there is an index to update is decided afterwards.
	fetched, usable, err := t.lookupRows(where, columnIndex, nil)
	if err != nil {
		return err
	}

	// Only the key values of the matching rows are kept, and only if there is
	// an index to update.
	trackKeys := t.builtIndexes() != nil
	keyCols := t.keyColumns()
	var matched []storage.RowID
	var keys [][]any
	if usable {
		for _, f := range fetched {
			matched = append(matched, f.id)
			if trackKeys {
				keys = append(keys, keyValuesAt(f.row, keyCols))
			}
		}
	} else {
		for row, err := range t.file.Scan() {
			if err != nil {
				return err
			}
			decoded, err := DecodeRow(t.Columns, row.Bytes)
			if err != nil {
				return err
			}
			isMatch, err := evalWhere(where, decoded, columnIndex, t.Columns)
			if err != nil {
				return err
			}
			if isMatch {
				matched = append(matched, row.ID)
				if trackKeys {
					keys = append(keys, keyValuesAt(decoded, keyCols))
				}
			}
		}
	}

	for _, id := range matched {
		if err := t.file.Delete(id); err != nil {
			t.dropIndexes()
			return err
		}
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexDeleted(keys)
	return nil
}

// resolvedAssignment is an Assignment resolved to its schema index and
// coerced value, computed once per Update call rather than per row — the
// same reasoning as InsertValues's fillers: a SET column = literal value is
// static across every matched row.
type resolvedAssignment struct {
	index int
	value any
}

// Update applies assignments to every row matching where (a nil where
// matches every row, same as Delete), enforcing PRIMARY KEY/UNIQUE on any
// assigned column. NOT NULL is a non-issue here: the parser only accepts a
// STRING or NUMBER literal on the right of SET, so an assignment can never
// produce NULL. The matching rows are found by one scan of the table's file, or
// by one index lookup when where pins or bounds a key and cannot fail. A
// unique column is in conflict when a row that is not being updated already
// holds the assigned value: an indexed column looks the value up, and a column
// without an index notices it during the scan. Every new version is encoded
// and size-checked before anything is written, and the new versions are
// inserted before the old ones are tombstoned, so a failure partway through (a
// coercion error, a uniqueness violation, a row too large for a page) leaves
// the table untouched, and a crash between the two writes can duplicate a row
// but never lose one. An I/O error during the writes discards the indexes;
// otherwise every updated row is re-pointed at its new RowID.
func (t *SqlTable) Update(assignments []parser.Assignment, where parser.Expression) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	resolved := make([]resolvedAssignment, len(assignments))
	for i, a := range assignments {
		idx, ok := columnIndex[a.Column]
		if !ok {
			return fmt.Errorf("unknown column %q", a.Column)
		}
		coerced, err := coerceValue(t.Columns[idx].DataType, a.Value)
		if err != nil {
			return fmt.Errorf("column %q: %v", t.Columns[idx].Name, err)
		}
		resolved[i] = resolvedAssignment{index: idx, value: coerced}
	}

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	// The PRIMARY KEY/UNIQUE columns being assigned, in assignment order. A
	// SET column = literal is static, so every matched row gets the same value.
	uniqueColumns := make([]int, 0)
	assigned := make(map[int]any, len(resolved))
	for _, a := range resolved {
		assigned[a.index] = a.value
		col := t.Columns[a.index]
		if col.IsPrimaryKey() || col.IsUnique() {
			uniqueColumns = append(uniqueColumns, a.index)
		}
	}

	// A where that pins or bounds a key is served from the index, but only if every
	// unique column being assigned has a tree: the conflict check for a column
	// without one needs the scan of the other rows. That can build the
	// indexes, so whether there is an index to update is decided afterwards.
	fetched, usable, err := t.lookupRows(where, columnIndex, uniqueColumns)
	if err != nil {
		return err
	}
	var trees map[int]*index.Tree
	if len(uniqueColumns) > 0 {
		if trees, err = t.indexes(); err != nil {
			return err
		}
	}
	// If the indexes are built, remember each matched row's old key values so
	// they can be removed after the write.
	trackKeys := t.builtIndexes() != nil
	keyCols := t.keyColumns()

	var oldIDs []storage.RowID
	var oldKeys [][]any
	var updated [][]any
	conflicts := make(map[int]bool, len(uniqueColumns))
	if usable {
		for _, f := range fetched {
			oldIDs = append(oldIDs, f.id)
			if trackKeys {
				oldKeys = append(oldKeys, keyValuesAt(f.row, keyCols))
			}
			for _, a := range resolved {
				f.row[a.index] = a.value
			}
			updated = append(updated, f.row)
		}
	} else {
		for fileRow, err := range t.file.Scan() {
			if err != nil {
				return err
			}
			row, err := DecodeRow(t.Columns, fileRow.Bytes)
			if err != nil {
				return err
			}
			matched, err := evalWhere(where, row, columnIndex, t.Columns)
			if err != nil {
				return err
			}
			if !matched {
				// A column with an index is checked after the scan; one
				// without is checked here. A matched row's own old value is
				// deliberately excluded, so re-assigning a column back to its
				// current value never self-conflicts.
				for _, idx := range uniqueColumns {
					if trees[idx] == nil && row[idx] != nil && row[idx] == assigned[idx] {
						conflicts[idx] = true
					}
				}
				continue
			}
			oldIDs = append(oldIDs, fileRow.ID)
			if trackKeys {
				oldKeys = append(oldKeys, keyValuesAt(row, keyCols))
			}
			for _, a := range resolved {
				row[a.index] = a.value
			}
			updated = append(updated, row)
		}
	}

	if len(updated) > 0 {
		// An UPDATE that matches no row stores nothing, so only now is a long
		// value a problem.
		for _, idx := range uniqueColumns {
			if err := checkKeyLength(t.Columns[idx].Name, assigned[idx]); err != nil {
				return err
			}
		}
		for _, idx := range uniqueColumns {
			if tree := trees[idx]; tree != nil && assigned[idx] != nil {
				key, err := index.EncodeKey(assigned[idx])
				if err != nil {
					return err
				}
				if holder, found := tree.Get(key); found && !slices.Contains(oldIDs, holder) {
					conflicts[idx] = true
				}
			}
			if conflicts[idx] {
				return fmt.Errorf("duplicate entry %v for column %q", assigned[idx], t.Columns[idx].Name)
			}
		}
		// Two matched rows would both receive the assigned value; the old
		// row-by-row check hit that on the second row, on the first unique
		// column in assignment order.
		if len(updated) > 1 && len(uniqueColumns) > 0 {
			idx := uniqueColumns[0]
			return fmt.Errorf("duplicate entry %v for column %q", assigned[idx], t.Columns[idx].Name)
		}
	}

	encoded, err := encodeRows(t.Columns, updated)
	if err != nil {
		return err
	}
	newIDs := make([]storage.RowID, 0, len(encoded))
	for _, b := range encoded {
		id, err := t.file.Insert(b)
		if err != nil {
			t.dropIndexes()
			return err
		}
		newIDs = append(newIDs, id)
	}
	for _, id := range oldIDs {
		if err := t.file.Delete(id); err != nil {
			t.dropIndexes()
			return err
		}
	}
	if err := t.file.Sync(); err != nil {
		t.dropIndexes()
		return err
	}
	t.indexMoved(oldKeys, updated, newIDs)
	return nil
}

// Rename records the table's new name in the catalog and then sets it; called
// by Database.RenameTable before it re-keys the owning database's table map,
// so a failed catalog write leaves the table under its old name.
func (t *SqlTable) Rename(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// A closed table is a dropped one, whose name may belong to a new table
	// by now: it must not touch the catalog row stored under that name.
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}
	row, err := encodeTableRow(t.database, name, t.fileID, t.Columns)
	if err != nil {
		return err
	}
	if err := t.store.replaceTable(t.database, t.Name, name, row); err != nil {
		return err
	}
	t.Name = name
	return nil
}

func (t *SqlTable) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeLocked()
}

func (t *SqlTable) closeLocked() error {
	t.dropIndexes()
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

func (t *SqlTable) Drop() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	closeErr := t.closeLocked()
	if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("removing table file %q: %w", t.path, err))
	}
	return closeErr
}
