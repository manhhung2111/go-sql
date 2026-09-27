package engine

import (
	"fmt"
	"manhhung2111/go-sql/internal/parser"
	"sync"
)

type Table interface {
	InsertValues(columns []string, values [][]parser.Token) error
	Select(columns []string, where parser.Expression) (Response, error)
	AlterColumns(action parser.AlterAction) error
	Delete(where parser.Expression) error
	Rename(name string)
}

type SqlTable struct {
	mu      sync.RWMutex
	Name    string
	Columns []parser.ColumnDefinition
	Rows    [][]any
}

// Rules when for a table schema:
//   - Not yet supported the multi-column primary key
//   - There is only 1 primary key column in a table, and the table can have multiple columns with constrants (NOT NULL & UNIQUE)
//   - Columns do not specify with contraint NOT NULL will default be NULLABLE
//   - A table can not have duplicated columns
func NewTable(name string, columns []parser.ColumnDefinition) (*SqlTable, error) {
	validated := make([]parser.ColumnDefinition, 0, len(columns))
	for _, column := range columns {
		if err := validateNewColumn(name, validated, column); err != nil {
			return nil, err
		}
		validated = append(validated, column)
	}

	return &SqlTable{Name: name, Columns: columns, Rows: make([][]any, 0)}, nil
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

	t.Rows = append(t.Rows, newRows...)
	return nil
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

	t.Columns = append(t.Columns, column)
	t.Rows = newRows
	return nil
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

	t.Columns = newColumns
	t.Rows = newRows
	return nil
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

	t.Rows = kept
	return nil
}

// Rename sets the table's own name; called by Database.RenameTable to
// keep it in sync after re-keying the owning database's table map.
func (t *SqlTable) Rename(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Name = name
}
