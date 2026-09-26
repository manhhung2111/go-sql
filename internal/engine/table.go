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

		if columnNameSet.Contains(column.Name) {
			return nil, fmt.Errorf("column name %q already exists", column.Name)
		}
		columnNameSet.Add(column.Name)
	}

	return &SqlTable{Name: name, Columns: columns, Rows: make([][]any, 0)}, nil
}

// InsertValues appends rows to the table. Values stay as raw parser.Token
// (or nil for SQL NULL) — type coercion against each column's DataType,
// and PRIMARY KEY/UNIQUE enforcement against existing rows, are both
// deferred to a later pass.
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
	// is a static literal Token here, not a dynamic expression.
	fillers := make(map[int]any)
	for i, col := range t.Columns {
		if provided[i] {
			continue
		}
		if def, ok := col.DefaultValue(); ok {
			fillers[i] = def
			continue
		}
		if col.IsNotNull() {
			return fmt.Errorf("field %q doesn't have a default value", col.Name)
		}
		fillers[i] = nil
	}

	// Phase 2: validate and build every row into a fresh buffer; only
	// commit to t.Rows once the whole batch is known good, so a failure
	// partway through a multi-row INSERT never leaves the table
	// half-inserted.
	newRows := make([][]any, len(values))
	for i, row := range values {
		if len(row) != len(targetColumns) {
			return fmt.Errorf("expected %d values, got %d", len(targetColumns), len(row))
		}

		rowValues := make([]any, len(t.Columns))
		for j, tok := range row {
			rowValues[targetIndices[j]] = tok
		}
		for idx, filler := range fillers {
			rowValues[idx] = filler
		}
		newRows[i] = rowValues
	}

	t.Rows = append(t.Rows, newRows...)
	return nil
}
