package engine

import (
	"errors"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// lookupPlan names a key a WHERE pins: column is a PRIMARY KEY/UNIQUE column's
// schema index and value is the literal, coerced to the column's type.
type lookupPlan struct {
	column int
	value  any
}

// planLookup returns the keys that where pins with an equality, in the order
// they appear, if where can be served from an index at all. That is only so
// when evaluating where cannot fail on any row: the scan evaluates it on every
// row, so an error in any part of it surfaces as soon as the table has one row,
// and a lookup that skipped the other rows would hide it. Such a where is an
// AND chain of comparisons that each have one column and one literal, where the
// column exists, the literal coerces to a non-NULL value of the column's type,
// and a boolean column is compared with = or != only. Anything else (OR, two
// columns, an unknown column, a literal that does not coerce, NULL) scans.
func (t *SqlTable) planLookup(where parser.Expression, columnIndex map[string]int) []lookupPlan {
	var conjuncts []*parser.ComparisonExpression
	if !collectConjuncts(where, &conjuncts) {
		return nil
	}
	var plans []lookupPlan
	for _, c := range conjuncts {
		column, value, ok := t.columnAgainstLiteral(c, columnIndex)
		if !ok {
			return nil
		}
		if c.Operator == parser.EQ && t.isKeyColumn(column) {
			plans = append(plans, lookupPlan{column: column, value: value})
		}
	}
	return plans
}

// collectConjuncts flattens an AND chain into its comparisons. It reports
// false for anything else, including a nil expression.
func collectConjuncts(expr parser.Expression, out *[]*parser.ComparisonExpression) bool {
	switch e := expr.(type) {
	case *parser.BinaryExpression:
		return e.Operator == parser.AND && collectConjuncts(e.Left, out) && collectConjuncts(e.Right, out)
	case *parser.ComparisonExpression:
		*out = append(*out, e)
		return true
	default:
		return false
	}
}

// columnAgainstLiteral resolves a comparison of one column with one literal,
// in either order, to the column and the literal coerced to its type.
func (t *SqlTable) columnAgainstLiteral(c *parser.ComparisonExpression, columnIndex map[string]int) (column int, value any, ok bool) {
	var name string
	var literal parser.Token
	switch leftIsColumn, rightIsColumn := c.Left.Type == parser.IDENT, c.Right.Type == parser.IDENT; {
	case leftIsColumn && !rightIsColumn:
		name, literal = c.Left.Value, c.Right
	case rightIsColumn && !leftIsColumn:
		name, literal = c.Right.Value, c.Left
	default:
		return 0, nil, false
	}
	column, known := columnIndex[name]
	if !known {
		return 0, nil, false
	}
	if _, isBool := t.Columns[column].DataType.(parser.BooleanDataType); isBool && c.Operator != parser.EQ && c.Operator != parser.NEQ {
		return 0, nil, false
	}
	value, err := coerceValue(t.Columns[column].DataType, literal)
	if err != nil || value == nil {
		return 0, nil, false
	}
	return column, value, true
}

func (t *SqlTable) isKeyColumn(column int) bool {
	col := t.Columns[column]
	return col.IsPrimaryKey() || col.IsUnique()
}

// fetchedRow is a row read by RowID.
type fetchedRow struct {
	id  storage.RowID
	row []any
}

// lookupRows serves where from an index when it can. usable is false when the
// statement must scan instead; otherwise rows holds the one row that satisfies
// where, or none. needTrees are columns that must have a tree for a lookup to
// be enough (an UPDATE's assigned unique columns). It may build the indexes,
// or discard them if one disagrees with the file. Callers hold t.mu.
func (t *SqlTable) lookupRows(where parser.Expression, columnIndex map[string]int, needTrees []int) (rows []fetchedRow, usable bool, err error) {
	plans := t.planLookup(where, columnIndex)
	if len(plans) == 0 {
		return nil, false, nil
	}
	trees, err := t.indexes()
	if err != nil {
		return nil, false, err
	}
	for _, column := range needTrees {
		if trees[column] == nil {
			return nil, false, nil
		}
	}
	for _, plan := range plans {
		tree := trees[plan.column]
		if tree == nil {
			continue
		}
		key, err := index.EncodeKey(plan.value)
		if err != nil {
			return nil, false, err
		}
		id, found := tree.Get(key)
		if !found {
			return nil, true, nil
		}
		b, err := t.file.Get(id)
		if errors.Is(err, storage.ErrRowNotFound) {
			// The index points at a row that is not there: it is wrong, so
			// drop it and let the caller scan.
			t.dropIndexes()
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		row, err := DecodeRow(t.Columns, b)
		if err != nil {
			return nil, false, err
		}
		matched, err := evalWhere(where, row, columnIndex, t.Columns)
		if err != nil {
			return nil, false, err
		}
		if !matched {
			return nil, true, nil
		}
		return []fetchedRow{{id: id, row: row}}, true, nil
	}
	return nil, false, nil
}
