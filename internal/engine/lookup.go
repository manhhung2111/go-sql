package engine

import (
	"bytes"
	"errors"
	"slices"

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

// rangePlan is a half-open range of encoded keys on one key column, as
// Tree.Range takes it. A nil lo or hi is unbounded.
type rangePlan struct {
	column int
	lo, hi []byte
}

// lookupPlans is what a WHERE offers an index: the keys it pins with an
// equality and the ranges it bounds with an ordering operator.
type lookupPlans struct {
	equalities []lookupPlan
	ranges     []rangePlan
}

// planLookup returns what where offers an index, if it can be served from an
// index at all. That is only so when evaluating where cannot fail on any row:
// the scan evaluates it on every row, so an error in any part of it surfaces as
// soon as the table has one row, and a lookup that skipped the other rows would
// hide it. Such a where is an AND chain of comparisons that each have one column
// and one literal, where the column exists, the operator is one the scan
// supports, the literal coerces to a value of the column's type (NULL does not),
// and a boolean column is compared with = or != only. Anything else (OR, two
// columns, an unknown column, a literal that does not coerce, NULL) scans.
//
// An = on a key column is an equality. A <, <=, > or >= on a key column bounds
// that column's range; bounds on one column combine into the tightest range. A
// != or a condition on another column constrains nothing but is still checked.
func (t *SqlTable) planLookup(where parser.Expression, columnIndex map[string]int) lookupPlans {
	var conjuncts []*parser.ComparisonExpression
	if !collectConjuncts(where, &conjuncts) {
		return lookupPlans{}
	}
	var plans lookupPlans
	for _, c := range conjuncts {
		column, op, value, ok := t.columnAgainstLiteral(c, columnIndex)
		if !ok {
			return lookupPlans{}
		}
		if !t.isKeyColumn(column) {
			continue
		}
		switch op {
		case parser.EQ:
			plans.equalities = append(plans.equalities, lookupPlan{column: column, value: value})
		case parser.GT, parser.GTE, parser.LT, parser.LTE:
			key, err := index.EncodeKey(value)
			if err != nil {
				return lookupPlans{}
			}
			plans.addBound(column, op, key)
		}
	}
	return plans
}

// addBound tightens the range on column with "column op key". The key that
// comes right after key in byte order is key followed by a zero byte, for the
// fixed-width integer encodings and for strings alike, so > and <= use that.
func (p *lookupPlans) addBound(column int, op parser.TokenType, key []byte) {
	i := slices.IndexFunc(p.ranges, func(r rangePlan) bool { return r.column == column })
	if i < 0 {
		p.ranges = append(p.ranges, rangePlan{column: column})
		i = len(p.ranges) - 1
	}
	r := &p.ranges[i]
	after := append(slices.Clone(key), 0)
	switch op {
	case parser.GTE:
		r.lo = tighterLower(r.lo, key)
	case parser.GT:
		r.lo = tighterLower(r.lo, after)
	case parser.LT:
		r.hi = tighterUpper(r.hi, key)
	case parser.LTE:
		r.hi = tighterUpper(r.hi, after)
	}
}

// tighterLower returns the greater of two lower bounds; nil is unbounded.
func tighterLower(current, bound []byte) []byte {
	if current == nil || bytes.Compare(bound, current) > 0 {
		return bound
	}
	return current
}

// tighterUpper returns the smaller of two upper bounds; nil is unbounded.
func tighterUpper(current, bound []byte) []byte {
	if current == nil || bytes.Compare(bound, current) < 0 {
		return bound
	}
	return current
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
// in either order, to the column, the operator as if the column were on the
// left (so 5 < id is id > 5), and the literal coerced to the column's type.
func (t *SqlTable) columnAgainstLiteral(c *parser.ComparisonExpression, columnIndex map[string]int) (column int, op parser.TokenType, value any, ok bool) {
	var name string
	var literal parser.Token
	op = c.Operator
	switch leftIsColumn, rightIsColumn := c.Left.Type == parser.IDENT, c.Right.Type == parser.IDENT; {
	case leftIsColumn && !rightIsColumn:
		name, literal = c.Left.Value, c.Right
	case rightIsColumn && !leftIsColumn:
		name, literal = c.Right.Value, c.Left
		switch op {
		case parser.LT:
			op = parser.GT
		case parser.LTE:
			op = parser.GTE
		case parser.GT:
			op = parser.LT
		case parser.GTE:
			op = parser.LTE
		}
	default:
		return 0, 0, nil, false
	}
	column, known := columnIndex[name]
	if !known {
		return 0, 0, nil, false
	}
	switch op {
	case parser.EQ, parser.NEQ, parser.LT, parser.LTE, parser.GT, parser.GTE:
	default:
		return 0, 0, nil, false // the scan reports "unsupported comparison operator"
	}
	if _, isBool := t.Columns[column].DataType.(parser.BooleanDataType); isBool && op != parser.EQ && op != parser.NEQ {
		return 0, 0, nil, false
	}
	// A NULL literal does not coerce, so it never reaches a lookup.
	value, err := coerceValue(t.Columns[column].DataType, literal)
	if err != nil {
		return 0, 0, nil, false
	}
	return column, op, value, true
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
	plans := t.planLookup(where, columnIndex).equalities
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
