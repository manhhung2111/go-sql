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

// rangePlan is a half-open range of encoded keys on one column, as Tree.Range
// takes it. A nil lo or hi is unbounded. secondary names the CREATE INDEX whose
// tree the range is over; it is empty for a PRIMARY KEY/UNIQUE column's own
// tree.
type rangePlan struct {
	column    int
	secondary string
	lo, hi    []byte
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
			t.planSecondary(&plans, column, op, value)
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

// planSecondary adds what "column op value" offers the first CREATE INDEX on
// column, if there is one. Every entry of a value ends in its row id, so an
// equality is the range that holds all of the value's entries, and the bounds
// of an ordering operator are the ends of that range.
func (t *SqlTable) planSecondary(plans *lookupPlans, column int, op parser.TokenType, value any) {
	def, ok := t.secondaryFor(column)
	if !ok {
		return
	}
	lo, hi, err := index.SecondaryEqualRange(value)
	if err != nil {
		return
	}
	switch op {
	case parser.EQ:
		plans.addSecondaryBounds(column, def.name, lo, hi)
	case parser.GTE:
		plans.addSecondaryBounds(column, def.name, lo, nil)
	case parser.GT:
		plans.addSecondaryBounds(column, def.name, hi, nil)
	case parser.LT:
		plans.addSecondaryBounds(column, def.name, nil, lo)
	case parser.LTE:
		plans.addSecondaryBounds(column, def.name, nil, hi)
	}
}

// addSecondaryBounds tightens the range over the named index with a lower and
// an upper bound; nil leaves that side alone.
func (p *lookupPlans) addSecondaryBounds(column int, name string, lower, upper []byte) {
	i := slices.IndexFunc(p.ranges, func(r rangePlan) bool { return r.column == column && r.secondary == name })
	if i < 0 {
		p.ranges = append(p.ranges, rangePlan{column: column, secondary: name})
		i = len(p.ranges) - 1
	}
	r := &p.ranges[i]
	if lower != nil {
		r.lo = tighterLower(r.lo, lower)
	}
	if upper != nil {
		r.hi = tighterUpper(r.hi, upper)
	}
}

// secondaryFor returns the first CREATE INDEX on column.
func (t *SqlTable) secondaryFor(column int) (indexDef, bool) {
	at := slices.IndexFunc(t.indexDefs, func(d indexDef) bool { return d.column == t.Columns[column].Name })
	if at < 0 {
		return indexDef{}, false
	}
	return t.indexDefs[at], true
}

// addBound tightens the range on column with "column op key". The key that
// comes right after key in byte order is key followed by a zero byte, for the
// fixed-width integer encodings and for strings alike, so > and <= use that.
func (p *lookupPlans) addBound(column int, op parser.TokenType, key []byte) {
	i := slices.IndexFunc(p.ranges, func(r rangePlan) bool { return r.column == column && r.secondary == "" })
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

// rangeCostFactor is how many pages a scan may read per page a range lookup
// reads before the lookup stops being worth it: a lookup's reads are scattered
// and a scan's are sequential. A range is a lookup while its rows lie on at
// most 1/rangeCostFactor of the file's pages. It is a variable so a test can
// set it to 0, which makes every range a lookup.
var rangeCostFactor = 2

// lookupRows serves where from an index when it can. usable is false when the
// statement must scan instead; otherwise rows holds the rows that satisfy
// where, in file order (the scan's order). A key pinned with = yields at most
// one row; a bounded range on a PRIMARY KEY/UNIQUE column or on a column with a
// CREATE INDEX yields its rows when they lie on few enough pages (see
// rangeCostFactor). needTrees are columns that must have a tree for a
// lookup to be enough (an UPDATE's assigned unique columns). It may build the
// indexes, or discard them if one disagrees with the file. Callers hold t.mu.
func (t *SqlTable) lookupRows(where parser.Expression, columnIndex map[string]int, needTrees []int) (rows []fetchedRow, usable bool, err error) {
	plans := t.planLookup(where, columnIndex)
	if len(plans.equalities) == 0 && len(plans.ranges) == 0 {
		return nil, false, nil
	}
	set, err := t.indexSet()
	if err != nil {
		return nil, false, err
	}
	trees := set.unique
	for _, column := range needTrees {
		if trees[column] == nil {
			return nil, false, nil
		}
	}
	for _, plan := range plans.equalities {
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
		row, matched, stale, err := t.fetchMatching(id, where, columnIndex)
		if err != nil {
			return nil, false, err
		}
		if stale {
			// The index points at a row that is not there: it is wrong, so
			// drop it and let the caller scan.
			t.distrustIndexes()
			return nil, false, nil
		}
		if !matched {
			return nil, true, nil
		}
		return []fetchedRow{{id: id, row: row}}, true, nil
	}
	// A range on a unique tree is preferred; then one on a secondary tree.
	for _, plan := range plans.ranges {
		if plan.secondary == "" {
			if tree := trees[plan.column]; tree != nil {
				return t.rangeRows(tree, plan, where, columnIndex)
			}
		}
	}
	for _, plan := range plans.ranges {
		if plan.secondary != "" {
			if tree := set.secondary[plan.secondary]; tree != nil {
				return t.rangeRows(tree, plan, where, columnIndex)
			}
		}
	}
	return nil, false, nil
}

// collectRangeIDs walks plan's range and returns the row ids in it. ok is false
// as soon as they lie on more than 1/rangeCostFactor of filePages pages; ids
// then holds only what was found before giving up, so a refused range costs a
// bounded part of the walk.
func collectRangeIDs(tree *index.Tree, plan rangePlan, filePages int) (ids []storage.RowID, ok bool) {
	seen := make(map[int32]struct{})
	for _, id := range tree.Range(plan.lo, plan.hi) {
		seen[id.Page] = struct{}{}
		if rangeCostFactor*len(seen) > filePages {
			return ids, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

// rangeRows fetches the rows whose key lies in plan's range and that satisfy
// where, in file order. It reports not usable if the rows lie on more than
// 1/rangeCostFactor of the file's pages, or an entry is stale.
func (t *SqlTable) rangeRows(tree *index.Tree, plan rangePlan, where parser.Expression, columnIndex map[string]int) ([]fetchedRow, bool, error) {
	ids, ok := collectRangeIDs(tree, plan, int(t.file.NumPages()))
	if !ok {
		return nil, false, nil
	}
	if len(ids) == 0 {
		return nil, true, nil
	}
	// The scan returns rows by page and slot; so must we, or results and the
	// order of an UPDATE's new versions would depend on the access path. It
	// also puts the rows of one page next to each other for GetMany.
	slices.SortFunc(ids, func(a, b storage.RowID) int {
		if a.Page != b.Page {
			if a.Page < b.Page {
				return -1
			}
			return 1
		}
		return a.Slot - b.Slot
	})

	encoded, err := t.file.GetMany(ids)
	if errors.Is(err, storage.ErrRowNotFound) {
		t.distrustIndexes()
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rows []fetchedRow
	for i, b := range encoded {
		row, matched, err := t.matchRow(b, where, columnIndex)
		if err != nil {
			return nil, false, err
		}
		if matched {
			rows = append(rows, fetchedRow{id: ids[i], row: row})
		}
	}
	return rows, true, nil
}

// fetchMatching reads the row at id and reports whether it satisfies where.
// stale is true when the file has no row there, which means the index is wrong.
func (t *SqlTable) fetchMatching(id storage.RowID, where parser.Expression, columnIndex map[string]int) (row []any, matched, stale bool, err error) {
	b, err := t.file.Get(id)
	if errors.Is(err, storage.ErrRowNotFound) {
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	row, matched, err = t.matchRow(b, where, columnIndex)
	return row, matched, false, err
}

// matchRow decodes an encoded row and reports whether it satisfies where.
func (t *SqlTable) matchRow(b []byte, where parser.Expression, columnIndex map[string]int) (row []any, matched bool, err error) {
	if row, err = DecodeRow(t.Columns, b); err != nil {
		return nil, false, err
	}
	matched, err = evalWhere(where, row, columnIndex, t.Columns)
	if err != nil {
		return nil, false, err
	}
	return row, matched, nil
}
