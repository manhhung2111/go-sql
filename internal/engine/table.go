package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
	"os"
	"sync"
)

type Table interface {
	InsertValues(columns []string, values [][]parser.Token) error
	Select(columns []string, where parser.Expression) (Response, error)
	AlterColumns(action parser.AlterAction) error
	Delete(where parser.Expression) error
	Update(assignments []parser.Assignment, where parser.Expression) error
	Rename(name string)
	// Close releases the table's file. It is idempotent.
	Close() error
	// Drop closes the table's file and deletes it. It is idempotent.
	Drop() error
}

type SqlTable struct {
	mu      sync.RWMutex
	Name    string
	Columns []parser.ColumnDefinition
	Rows    [][]any

	// path and file are the table's heap file. INSERT and ALTER ADD/DROP
	// COLUMN keep it a copy of Rows; the other statements and every read
	// still use Rows alone until they are migrated.
	path string
	file storage.File

	// files and database let the table allocate a replacement file when a
	// schema change rewrites its rows.
	files    *fileAllocator
	database string
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
func NewTable(name string, columns []parser.ColumnDefinition, files *fileAllocator, database string) (*SqlTable, error) {
	validated := make([]parser.ColumnDefinition, 0, len(columns))
	for _, column := range columns {
		if err := validateNewColumn(name, validated, column); err != nil {
			return nil, err
		}
		validated = append(validated, column)
	}

	path, file, err := createTableFile(files, database)
	if err != nil {
		return nil, err
	}

	return &SqlTable{
		Name: name, Columns: columns, Rows: make([][]any, 0),
		path: path, file: file, files: files, database: database,
	}, nil
}

// createTableFile makes a new, empty table file in database's directory,
// creating the directory first if it is missing: names that differ only by
// case share one directory on a case-insensitive filesystem, so dropping one
// can remove a directory another database still uses. The file is created
// exclusively, so it can never adopt an old file's rows.
func createTableFile(files *fileAllocator, database string) (string, storage.File, error) {
	if err := os.MkdirAll(files.databaseDir(database), 0o755); err != nil {
		return "", nil, fmt.Errorf("creating directory for database %q: %w", database, err)
	}

	path := files.newTablePath(database)
	file, err := storage.CreateFile(path)
	if err != nil {
		return "", nil, err
	}
	return path, file, nil
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

// InsertValues appends rows to the table, coercing each value against its
// column's DataType and enforcing NOT NULL/PRIMARY KEY/UNIQUE.
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

	// Seed a uniqueness set per PRIMARY KEY/UNIQUE column from existing
	// rows, once. Checking-and-inserting into the same set as each new
	// row is processed catches a duplicate against existing data and a
	// duplicate within this same batch (e.g. VALUES (1), (1)) uniformly.
	uniqueColumns := make([]int, 0)
	for i, col := range t.Columns {
		if col.IsPrimaryKey() || col.IsUnique() {
			uniqueColumns = append(uniqueColumns, i)
		}
	}
	uniqueValues := make(map[int]map[any]bool, len(uniqueColumns))
	for _, idx := range uniqueColumns {
		set := make(map[any]bool, len(t.Rows))
		for _, existingRow := range t.Rows {
			if v := existingRow[idx]; v != nil {
				set[v] = true
			}
		}
		uniqueValues[idx] = set
	}

	// Phase 2: validate, coerce, and check constraints for every row into
	// a fresh buffer; only commit to t.Rows once the whole batch is known
	// good, so a failure partway through a multi-row INSERT never leaves
	// the table half-inserted.
	newRows := make([][]any, len(values))
	for i, row := range values {
		if len(row) != len(targetColumns) {
			return fmt.Errorf("expected %d values, got %d", len(targetColumns), len(row))
		}

		rowValues := make([]any, len(t.Columns))
		for j, tok := range row {
			idx := targetIndices[j]
			coerced, err := coerceValue(t.Columns[idx].DataType, tok)
			if err != nil {
				return fmt.Errorf("column %q: %v", t.Columns[idx].Name, err)
			}
			rowValues[idx] = coerced
		}
		for idx, filler := range fillers {
			rowValues[idx] = filler // already coerced once, in phase 1
		}

		for _, idx := range uniqueColumns {
			value := rowValues[idx]
			if value == nil {
				continue // multiple NULLs are allowed in a UNIQUE column
			}
			if uniqueValues[idx][value] {
				return fmt.Errorf("duplicate entry %v for column %q", value, t.Columns[idx].Name)
			}
			uniqueValues[idx][value] = true
		}

		newRows[i] = rowValues
	}

	// Phase 3: mirror the batch onto the table's file before committing it
	// to Rows, so a failed write leaves Rows untouched.
	if err := t.appendToFile(newRows); err != nil {
		return err
	}

	t.Rows = append(t.Rows, newRows...)
	return nil
}

// appendToFile writes rows to the table's file with a single fsync. Every
// row is encoded and size-checked before the first write, so a bad batch
// writes nothing; only an I/O error can leave it partly written, and the
// engine has no WAL to undo that. Callers hold t.mu.
func (t *SqlTable) appendToFile(rows [][]any) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	encoded, err := encodeRows(t.Columns, rows)
	if err != nil {
		return err
	}

	for _, b := range encoded {
		if _, err := t.file.Insert(b); err != nil {
			return err
		}
	}
	return t.file.Sync()
}

// encodeRows encodes every row with columns and rejects any that cannot fit
// on a page, before the caller writes anything.
func encodeRows(columns []parser.ColumnDefinition, rows [][]any) ([][]byte, error) {
	encoded := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := EncodeRow(columns, row)
		if err != nil {
			return nil, err
		}
		if len(b) > storage.MaxRowSize {
			return nil, fmt.Errorf("row too large: %d bytes exceeds the %d-byte limit", len(b), storage.MaxRowSize)
		}
		encoded[i] = b
	}
	return encoded, nil
}

// newFileHolding builds a complete replacement for the table's file: a new
// file in the table's database directory holding rows encoded with columns,
// fsynced. Nothing about the table changes. Every row is encoded and
// size-checked before the file is created, and on any later failure the
// partial file is removed, so a failed call leaves the directory as it was.
// Callers hold t.mu.
func (t *SqlTable) newFileHolding(columns []parser.ColumnDefinition, rows [][]any) (string, storage.File, error) {
	if t.file == nil {
		return "", nil, fmt.Errorf("table %q is closed", t.Name)
	}

	encoded, err := encodeRows(columns, rows)
	if err != nil {
		return "", nil, err
	}

	path, file, err := createTableFile(t.files, t.database)
	if err != nil {
		return "", nil, err
	}
	for _, b := range encoded {
		if _, err := file.Insert(b); err != nil {
			discardFile(path, file)
			return "", nil, err
		}
	}
	if err := file.Sync(); err != nil {
		discardFile(path, file)
		return "", nil, err
	}
	return path, file, nil
}

// adoptFile makes file the table's file and deletes the old one. Callers have
// already committed the new Columns and Rows and hold t.mu: if the old file
// cannot be removed the change is still applied and the old file is a
// harmless orphan, so the error says so.
func (t *SqlTable) adoptFile(path string, file storage.File) error {
	oldPath := t.path
	closeErr := t.closeLocked()

	t.file, t.path = file, path

	if err := os.Remove(oldPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("column change applied, but removing the old table file %q failed: %w", oldPath, err))
	}
	return closeErr
}

// discardFile closes and deletes a file that was never adopted.
func discardFile(path string, file storage.File) {
	_ = file.Close()
	_ = os.Remove(path)
}

// Select performs a full-table scan, keeping rows where matches (a nil
// where matches every row), then projects each matching row down to
// columns ("*" expands to every column in schema order).
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

	rows := make([][]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return Response{}, err
		}
		if !matched {
			continue
		}

		projected := make([]string, len(outputIndices))
		for i, idx := range outputIndices {
			projected[i] = stringifyValue(row[idx])
		}
		rows = append(rows, projected)
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
// row with its value. Reuses the exact same coercion/uniqueness rules
// InsertValues already enforces: a NOT NULL column with no default, or a
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

	checkUnique := column.IsPrimaryKey() || column.IsUnique()
	seen := make(map[any]bool, len(t.Rows))

	newRows := make([][]any, len(t.Rows))
	for i, row := range t.Rows {
		if checkUnique && filler != nil {
			if seen[filler] {
				return fmt.Errorf("duplicate entry %v for column %q", filler, column.Name)
			}
			seen[filler] = true
		}
		newRows[i] = append(append([]any{}, row...), filler)
	}

	// Build the replacement file before touching the table, so a failure
	// (a row that no longer fits on a page, an I/O error) changes nothing.
	newColumns := append(append([]parser.ColumnDefinition{}, t.Columns...), column)
	path, file, err := t.newFileHolding(newColumns, newRows)
	if err != nil {
		return err
	}

	t.Columns = newColumns
	t.Rows = newRows
	return t.adoptFile(path, file)
}

// dropColumn removes column at its schema index from both Columns and
// every row in Rows — the two must stay aligned, or every subsequent
// read of a row would be reading the wrong column's value.
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
	newRows := make([][]any, len(t.Rows))
	for i, row := range t.Rows {
		newRows[i] = append(append([]any{}, row[:idx]...), row[idx+1:]...)
	}

	path, file, err := t.newFileHolding(newColumns, newRows)
	if err != nil {
		return err
	}

	t.Columns = newColumns
	t.Rows = newRows
	return t.adoptFile(path, file)
}

// renameColumn doesn't touch Rows — rows are positional, not keyed by
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

	t.Columns[idx].Name = newName
	return nil
}

// Delete removes every row matching where (a nil where matches every row,
// so a bare DELETE FROM wipes the table). Rows are collected into a fresh
// buffer of survivors and only committed via one final assignment, so an
// error partway through (e.g. an unknown column in where) leaves t.Rows
// completely untouched rather than partially compacted.
func (t *SqlTable) Delete(where parser.Expression) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	columnIndex := make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		columnIndex[col.Name] = i
	}

	kept := make([][]any, 0, len(t.Rows))
	for _, row := range t.Rows {
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return err
		}
		if !matched {
			kept = append(kept, row)
		}
	}

	// Mirror the delete onto the table's file before committing it to Rows,
	// so a failed write leaves Rows untouched.
	if err := t.deleteFromFile(where, columnIndex); err != nil {
		return err
	}

	t.Rows = kept
	return nil
}

// deleteFromFile tombstones every live row of the table's file that matches
// where, then fsyncs once. The matching rows are collected before the first
// tombstone is written, so an evaluation error touches nothing, and a row is
// never tombstoned while the scan is still reading its page; only an I/O error
// can leave the delete partly applied, and the engine has no WAL to undo that.
// The matches come from the file's own rows, not from Rows: the two can drift
// until every statement is migrated, and a row that exists in only one of them
// is simply not a match in the other. Callers hold t.mu, and have already
// evaluated where against Rows, so an invalid where was reported there first.
func (t *SqlTable) deleteFromFile(where parser.Expression, columnIndex map[string]int) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	var matched []storage.RowID
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
		}
	}

	for _, id := range matched {
		if err := t.file.Delete(id); err != nil {
			return err
		}
	}
	return t.file.Sync()
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
// produce NULL. Rows are built into a fresh buffer and only committed via
// one final assignment, so a failure partway through (e.g. a coercion
// error or a uniqueness violation) leaves the table completely untouched.
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

	// Determine which rows match, once, so the uniqueness seed below can
	// be built from exactly the rows that are staying unchanged.
	matched := make([]bool, len(t.Rows))
	for i, row := range t.Rows {
		m, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return err
		}
		matched[i] = m
	}

	// Seed a uniqueness set per PRIMARY KEY/UNIQUE column touched by an
	// assignment, from every row NOT being updated — those values stay
	// occupied. A matched row's own old value is deliberately excluded, so
	// re-assigning a column back to its current value never self-conflicts.
	uniqueColumns := make([]int, 0)
	for _, a := range resolved {
		col := t.Columns[a.index]
		if col.IsPrimaryKey() || col.IsUnique() {
			uniqueColumns = append(uniqueColumns, a.index)
		}
	}
	uniqueValues := make(map[int]map[any]bool, len(uniqueColumns))
	for _, idx := range uniqueColumns {
		set := make(map[any]bool, len(t.Rows))
		for i, row := range t.Rows {
			if matched[i] {
				continue
			}
			if v := row[idx]; v != nil {
				set[v] = true
			}
		}
		uniqueValues[idx] = set
	}

	newRows := make([][]any, len(t.Rows))
	for i, row := range t.Rows {
		if !matched[i] {
			newRows[i] = row
			continue
		}

		updated := append([]any{}, row...)
		for _, a := range resolved {
			updated[a.index] = a.value
		}

		for _, idx := range uniqueColumns {
			value := updated[idx]
			if value == nil {
				continue
			}
			if uniqueValues[idx][value] {
				return fmt.Errorf("duplicate entry %v for column %q", value, t.Columns[idx].Name)
			}
			uniqueValues[idx][value] = true
		}

		newRows[i] = updated
	}

	// Mirror the update onto the table's file before committing it to Rows,
	// so a failed write leaves Rows untouched.
	if err := t.updateFile(resolved, where, columnIndex); err != nil {
		return err
	}

	t.Rows = newRows
	return nil
}

// updateFile writes the update to the table's file: every live row matching
// where gets a new version, appended with the assignments applied, and its
// old version is tombstoned, then one fsync. The matching rows are collected
// and every new version is encoded and size-checked before the first write,
// so a bad update writes nothing; only an I/O error can leave it partly
// applied, and the engine has no WAL to undo that. The new versions are
// inserted before the old ones are tombstoned, so a crash between the two can
// duplicate a row but never lose one. The matches come from the file's own
// rows, not from Rows, which can drift until every statement is migrated, and
// uniqueness is not re-checked here: Rows already did. Callers hold t.mu and
// have already evaluated where and the assignments against Rows, so an
// invalid statement was reported there first.
func (t *SqlTable) updateFile(assignments []resolvedAssignment, where parser.Expression, columnIndex map[string]int) error {
	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}

	var oldIDs []storage.RowID
	var updated [][]any
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
		if !isMatch {
			continue
		}
		for _, a := range assignments {
			decoded[a.index] = a.value
		}
		oldIDs = append(oldIDs, row.ID)
		updated = append(updated, decoded)
	}

	encoded, err := encodeRows(t.Columns, updated)
	if err != nil {
		return err
	}

	for _, b := range encoded {
		if _, err := t.file.Insert(b); err != nil {
			return err
		}
	}
	for _, id := range oldIDs {
		if err := t.file.Delete(id); err != nil {
			return err
		}
	}
	return t.file.Sync()
}

// Rename sets the table's own name; called by Database.RenameTable to
// keep it in sync after re-keying the owning database's table map.
func (t *SqlTable) Rename(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Name = name
}

func (t *SqlTable) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closeLocked()
}

func (t *SqlTable) closeLocked() error {
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
