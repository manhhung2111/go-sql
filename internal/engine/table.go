package engine

import (
	"fmt"
	"manhhung2111/go-sql/internal/parser"
	"sync"

	mapset "github.com/deckarep/golang-set/v3"
)

type Table interface {
	InsertValues(columns []string, values [][]parser.Token) error
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
	primaryKeyColumns := 0
	columnNameSet := mapset.NewSet[string]()

	for _, column := range columns {
		if column.IsPrimaryKey() {
			primaryKeyColumns++
		}

		if primaryKeyColumns > 1 {
			return nil, fmt.Errorf("table %s has more than one primary key column", name)
		}

		// Validate DEFAULT against its own column's type now, so a broken
		// DEFAULT fails at CREATE TABLE time rather than deferring to
		// whoever triggers the first INSERT.
		if def, ok := column.DefaultValue(); ok {
			if _, err := coerceValue(column.DataType, def); err != nil {
				return nil, fmt.Errorf("column %q: invalid default value: %v", column.Name, err)
			}
		}

		if columnNameSet.Contains(column.Name) {
			return nil, fmt.Errorf("column name %q already exists", column.Name)
		}
		columnNameSet.Add(column.Name)
	}

	return &SqlTable{Name: name, Columns: columns, Rows: make([][]any, 0)}, nil
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
