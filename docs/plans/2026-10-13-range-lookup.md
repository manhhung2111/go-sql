# Range lookups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `SELECT`, `UPDATE` and `DELETE` whose `WHERE` bounds a PRIMARY KEY or UNIQUE column with `<`, `<=`, `>` or `>=` from the index when the range holds at most 100 rows, with the same results, errors and row order as the scan.

**Architecture:** `planLookup` also returns a `rangePlan` (a half-open range of encoded keys) per key column that has bounds, combining the bounds into the tightest range. `lookupRows` walks `Tree.Range`, gives up above `maxRangeRows`, sorts the row ids into file order (page, slot), fetches and re-checks the whole `WHERE` on each row. `Select`, `deleteFromFile` and `Update` already loop over the rows `lookupRows` returns, so they do not change.

**Tech Stack:** Go 1.26.4, `internal/index` (`Tree.Range`, `EncodeKey`), `internal/storage` (`File.Get`), testify.

**Spec:** `docs/specs/2026-10-13-range-lookup-design.md`

**Prerequisite:** part B (`docs/plans/2026-10-13-engine-equality-lookup-b.md`, PR #38) is merged: this plan changes its `lookup.go` and uses its `lookupRows`, `fetchedRow`, `eqCmp`, `andExpr`, `orExpr`, `flagTable` and the part A helpers.

## Global Constraints

- Go 1.26.4; run `go test -race ./...` (required when touching `internal/engine`).
- A range lookup follows part B's safety rule: only a `WHERE` that cannot fail at run time. A boolean column is never range-planned.
- Bounds become half-open key ranges: `>= v` is `lo = K`, `> v` is `lo = K` followed by a zero byte, `< v` is `hi = K`, `<= v` is `hi = K` followed by a zero byte. Bounds on one column combine to the tightest range. A reversed comparison (`5 < id`) means `id > 5`.
- An equality on a key column wins over a range.
- Rows are fetched in file order (sorted by page, then slot), so results, their order and the file an UPDATE writes equal the scan's.
- The walk gives up above `maxRangeRows` (100, a package variable) and the statement scans.
- The package already has a function named `cmp` in its tests, so non-test code here must not import the standard `cmp` package.
- Error messages: lowercase, no trailing punctuation. Run `gofmt -w internal/` after each edit.
- Tests use testify; every test is seen failing before the code that makes it pass.

## Review Focus

- Exact boundaries: `>` excludes the literal, `<=` includes it, a literal between keys falls on the right side, negative integers and strings sort correctly. Pinned in Tasks 1 and 2.
- The order of the rows and of an UPDATE's new versions equals the scan's after rows have moved to the end of the file. Pinned in Task 2.
- A range of exactly 100 rows is a lookup and 101 scans, with the same answer. Pinned in Task 2.
- A WHERE that could fail (unknown column, bad literal, boolean ordering) still fails the same way. Pinned in Task 2.
- A range on a column without a tree, or with a stale entry, scans and answers correctly. Pinned in Task 2.

---

### Task 1: Plan ranges

**Files:**
- Modify: `internal/engine/lookup.go`, `internal/engine/lookup_test.go`
- Create: `internal/engine/range_test.go`

**Interfaces:**
- Consumes: part B's `planLookup`, `collectConjuncts`, `columnAgainstLiteral`, `isKeyColumn`, `lookupPlan`; `index.EncodeKey`.
- Produces: `type rangePlan struct{ column int; lo, hi []byte }`, `type lookupPlans struct{ equalities []lookupPlan; ranges []rangePlan }`, `planLookup` returning `lookupPlans`, `(*lookupPlans).addBound`, `tighterLower`, `tighterUpper`, and `columnAgainstLiteral` returning `(column int, op parser.TokenType, value any, ok bool)`. `lookupRows` still uses only the equalities, so no statement changes in this task.

- [ ] **Step 1: Write the failing tests**

In `internal/engine/lookup_test.go`, change the four places that read `planLookup`'s result to read its `equalities`:

```go
assert.Equal(t, tc.want, table.planLookup(tc.where, columnIndex))
```

becomes

```go
assert.Equal(t, tc.want, table.planLookup(tc.where, columnIndex).equalities)
```

and in `TestPlanLookup_BooleanColumnsOnlyTakeEqualityOperators`, add `.equalities` after `columnIndex)` in these three lines:

```go
assert.Equal(t, want, table.planLookup(andExpr(byID, eqCmp("flag", numTok("1"))), columnIndex), "= on a boolean is fine")
```

becomes

```go
assert.Equal(t, want, table.planLookup(andExpr(byID, eqCmp("flag", numTok("1"))), columnIndex).equalities, "= on a boolean is fine")
```

```go
assert.Equal(t, want, table.planLookup(andExpr(byID, cmp(identTok("flag"), parser.NEQ, numTok("1"))), columnIndex), "so is !=")
```

becomes

```go
assert.Equal(t, want, table.planLookup(andExpr(byID, cmp(identTok("flag"), parser.NEQ, numTok("1"))), columnIndex).equalities, "so is !=")
```

```go
assert.Nil(t, table.planLookup(andExpr(byID, cmp(identTok("flag"), parser.GT, numTok("0"))), columnIndex),
```

becomes

```go
assert.Nil(t, table.planLookup(andExpr(byID, cmp(identTok("flag"), parser.GT, numTok("0"))), columnIndex).equalities,
```

Create `internal/engine/range_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
)

func keyOf(t *testing.T, v any) []byte {
	t.Helper()
	key, err := index.EncodeKey(v)
	require.NoError(t, err)
	return key
}

func after(key []byte) []byte { return append(append([]byte{}, key...), 0) }

func TestPlanLookup_Ranges(t *testing.T) {
	table := keyedTable(t)
	columnIndex := map[string]int{"id": colID, "email": colEmail, "name": colName}
	id := func(n int64) []byte { return keyOf(t, n) }
	idCmp := func(op parser.TokenType, n string) *parser.ComparisonExpression {
		return cmp(identTok("id"), op, numTok(n))
	}
	tests := []struct {
		name  string
		where parser.Expression
		want  []rangePlan
	}{
		{">=", idCmp(parser.GTE, "3"), []rangePlan{{colID, id(3), nil}}},
		{">", idCmp(parser.GT, "3"), []rangePlan{{colID, after(id(3)), nil}}},
		{"<", idCmp(parser.LT, "3"), []rangePlan{{colID, nil, id(3)}}},
		{"<=", idCmp(parser.LTE, "3"), []rangePlan{{colID, nil, after(id(3))}}},
		{"reversed: 3 < id is id > 3", cmp(numTok("3"), parser.LT, identTok("id")), []rangePlan{{colID, after(id(3)), nil}}},
		{"reversed: 3 >= id is id <= 3", cmp(numTok("3"), parser.GTE, identTok("id")), []rangePlan{{colID, nil, after(id(3))}}},
		{"a pair of bounds", andExpr(idCmp(parser.GTE, "3"), idCmp(parser.LT, "10")), []rangePlan{{colID, id(3), id(10)}}},
		{"the greater lower bound wins", andExpr(andExpr(idCmp(parser.GT, "3"), idCmp(parser.GTE, "5")), idCmp(parser.GT, "4")),
			[]rangePlan{{colID, id(5), nil}}},
		{"the smaller upper bound wins", andExpr(andExpr(idCmp(parser.LT, "10"), idCmp(parser.LTE, "7")), idCmp(parser.LT, "8")),
			[]rangePlan{{colID, nil, after(id(7))}}},
		{"an empty range is still a plan", andExpr(idCmp(parser.GT, "9"), idCmp(parser.LT, "3")), []rangePlan{{colID, after(id(9)), id(3)}}},
		{"a negative integer", idCmp(parser.GTE, "-5"), []rangePlan{{colID, id(-5), nil}}},
		{"a string", cmp(identTok("email"), parser.GT, strTok("m")), []rangePlan{{colEmail, []byte("m\x00"), nil}}},
		{"the empty string as a lower bound", cmp(identTok("email"), parser.GTE, strTok("")), []rangePlan{{colEmail, []byte{}, nil}}},
		{"beside an equality on the same column", andExpr(eqCmp("id", numTok("3")), idCmp(parser.GT, "1")), []rangePlan{{colID, after(id(1)), nil}}},
		{"beside a condition on another column", andExpr(idCmp(parser.GTE, "3"), eqCmp("name", strTok("x"))), []rangePlan{{colID, id(3), nil}}},
		{"beside a != on the same column", andExpr(idCmp(parser.GTE, "3"), cmp(identTok("id"), parser.NEQ, numTok("5"))), []rangePlan{{colID, id(3), nil}}},
		{"two key columns, in order of appearance", andExpr(idCmp(parser.GTE, "3"), cmp(identTok("email"), parser.LT, strTok("z"))),
			[]rangePlan{{colID, id(3), nil}, {colEmail, nil, []byte("z")}}},
		{"a column that is not a key", cmp(identTok("name"), parser.GT, strTok("a")), nil},
		{"only !=", cmp(identTok("id"), parser.NEQ, numTok("3")), nil},
		{"OR", orExpr(idCmp(parser.GT, "3"), idCmp(parser.LT, "1")), nil},
		{"an unknown column anywhere", andExpr(idCmp(parser.GTE, "3"), eqCmp("nosuch", numTok("1"))), nil},
		{"a literal that does not coerce anywhere", andExpr(idCmp(parser.GTE, "3"), eqCmp("name", numTok("3"))), nil},
		{"a string against an INT key", cmp(identTok("id"), parser.GT, strTok("x")), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, table.planLookup(tc.where, columnIndex).ranges)
		})
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go vet ./internal/engine/ 2>&1 | head`
Expected: FAIL to compile: `table.planLookup(...).equalities undefined` (it returns a slice) and `undefined: rangePlan`.

- [ ] **Step 3: Implement**

In `internal/engine/lookup.go`, replace the import block's start

```text
import (
	"errors"

	"manhhung2111/go-sql/internal/index"
```

with:

```text
import (
	"bytes"
	"errors"
	"slices"

	"manhhung2111/go-sql/internal/index"
```

Replace the whole of `planLookup`, including its doc comment, with the following (it adds the two types, `addBound`, `tighterLower` and `tighterUpper` after it is defined):

```go
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
```

Replace the whole of `columnAgainstLiteral`, including its doc comment, with:

```go
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
```

In `lookupRows`, change

```text
	plans := t.planLookup(where, columnIndex)
	if len(plans) == 0 {
```

to:

```text
	plans := t.planLookup(where, columnIndex).equalities
	if len(plans) == 0 {
```

- [ ] **Step 4: Run the whole engine suite**

Run: `gofmt -w internal/ && go vet ./internal/... && go test -race -count=1 ./internal/engine/`
Expected: PASS: the new `TestPlanLookup_Ranges` (23 cases) and every existing test, since ranges are planned but not yet used.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/lookup.go internal/engine/lookup_test.go internal/engine/range_test.go
git commit -m "feat(engine): plan the key ranges a WHERE bounds" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Fetch a range's rows, in file order

**Files:**
- Modify: `internal/engine/lookup.go`
- Test: `internal/engine/range_test.go`, `internal/engine/lookup_test.go`, `internal/engine/tableindex_test.go`

**Interfaces:**
- Consumes: Task 1's `lookupPlans.ranges`; `Tree.Range`; part B's `lookupRows`, `fetchedRow`.
- Produces: `maxRangeRows`, `(*SqlTable).rangeRows`, `(*SqlTable).fetchMatching`, and a `lookupRows` that serves ranges. `Select`, `Delete` and `Update` use it unchanged.

- [ ] **Step 1: Write the failing tests**

Two existing tests used a range as an example of something that scans, which is no longer true. In `internal/engine/lookup_test.go`, in `TestLookup_WhatItCannotServeStillScans`, change

```text
		{"a range", cmp(identTok("id"), parser.GTE, numTok("3"))},
```

to:

```text
		{"a range on a column that is not a key", cmp(identTok("name"), parser.GTE, strTok("n3"))},
```

In `internal/engine/tableindex_test.go`, in `TestTableIndexes_AMissingEntryDiscardsThem`, change

```text
			return table.Delete(cmp(identTok("id"), parser.GTE, numTok("3")))
```

to:

```text
			return table.Delete(cmp(identTok("name"), parser.GTE, strTok("n3")))
```

and the comment above those cases

```text
		// These WHEREs scan. A key lookup would not notice the missing entry:
		// it would find nothing and stop.
```

to:

```text
		// These WHEREs scan. A key lookup would not notice the missing entry:
		// it would find nothing and stop. (A condition on a column that is not a
		// key never uses an index.)
```

In `internal/engine/range_test.go`, replace the import block with:

```go
import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
)
```

and append:

```go
// intKeys is a keyedTable whose rows have the given ids, emails "e<id>" padded
// to two digits for non-negative ids, and the name "n". Its indexes are built.
func intKeys(t *testing.T, ids ...int) *SqlTable {
	t.Helper()
	table := keyedTable(t)
	rows := make([][]parser.Token, len(ids))
	for i, id := range ids {
		rows[i] = tokRow(numTok(fmt.Sprint(id)), strTok(fmt.Sprintf("e%03d", id+100)), strTok("n"))
	}
	require.NoError(t, table.InsertValues(nil, rows))
	_, err := table.indexes()
	require.NoError(t, err)
	return table
}

func sequence(from, to int) []int {
	ids := []int{}
	for id := from; id <= to; id++ {
		ids = append(ids, id)
	}
	return ids
}

// idsOf returns the first column of every row of a SELECT id.
func idsOf(t *testing.T, table *SqlTable, where parser.Expression) [][]string {
	t.Helper()
	resp, err := table.Select([]string{"id"}, where)
	require.NoError(t, err)
	return resp.Rows
}

func TestRangeLookup_BoundsAreExact(t *testing.T) {
	table := intKeys(t, sequence(1, 20)...)
	cf := countOn(table)
	ops := []struct {
		name  string
		op    parser.TokenType
		holds func(id, lit int) bool
	}{
		{">", parser.GT, func(id, lit int) bool { return id > lit }},
		{">=", parser.GTE, func(id, lit int) bool { return id >= lit }},
		{"<", parser.LT, func(id, lit int) bool { return id < lit }},
		{"<=", parser.LTE, func(id, lit int) bool { return id <= lit }},
	}
	for _, lit := range []int{0, 1, 7, 20, 21} {
		for _, o := range ops {
			t.Run(fmt.Sprintf("id %s %d", o.name, lit), func(t *testing.T) {
				want := [][]string{}
				for id := 1; id <= 20; id++ {
					if o.holds(id, lit) {
						want = append(want, []string{fmt.Sprint(id)})
					}
				}
				cf.scans.Store(0)
				cf.gets.Store(0)

				got := idsOf(t, table, cmp(identTok("id"), o.op, numTok(fmt.Sprint(lit))))

				assert.Equal(t, want, got)
				assert.EqualValues(t, 0, cf.scans.Load(), "no scan")
				assert.EqualValues(t, len(want), cf.gets.Load(), "one get per row in the range")
			})
		}
	}

	t.Run("reversed operands", func(t *testing.T) {
		assert.Equal(t, [][]string{{"18"}, {"19"}, {"20"}}, idsOf(t, table, cmp(numTok("17"), parser.LT, identTok("id"))))
	})
	t.Run("a pair of bounds", func(t *testing.T) {
		assert.Equal(t, [][]string{{"5"}, {"6"}, {"7"}}, idsOf(t, table, andExpr(
			cmp(identTok("id"), parser.GTE, numTok("5")), cmp(identTok("id"), parser.LT, numTok("8")))))
	})
	t.Run("an empty range", func(t *testing.T) {
		cf.gets.Store(0)
		assert.Equal(t, [][]string{}, idsOf(t, table, andExpr(
			cmp(identTok("id"), parser.GT, numTok("9")), cmp(identTok("id"), parser.LT, numTok("3")))))
		assert.EqualValues(t, 0, cf.gets.Load())
	})
}

func TestRangeLookup_StringBoundsAreExact(t *testing.T) {
	table := intKeys(t, sequence(1, 20)...) // emails "e101" .. "e120"
	cf := countOn(table)
	emails := []string{}
	for id := 1; id <= 20; id++ {
		emails = append(emails, fmt.Sprintf("e%03d", id+100))
	}
	ops := []struct {
		name  string
		op    parser.TokenType
		holds func(a, b string) bool
	}{
		{">", parser.GT, func(a, b string) bool { return a > b }},
		{">=", parser.GTE, func(a, b string) bool { return a >= b }},
		{"<", parser.LT, func(a, b string) bool { return a < b }},
		{"<=", parser.LTE, func(a, b string) bool { return a <= b }},
	}
	for _, lit := range []string{"e105", "e105a", "", "a", "zzz"} {
		for _, o := range ops {
			t.Run(fmt.Sprintf("email %s %q", o.name, lit), func(t *testing.T) {
				want := [][]string{}
				for id, email := range emails {
					if o.holds(email, lit) {
						want = append(want, []string{fmt.Sprint(id + 1)})
					}
				}
				cf.scans.Store(0)

				got := idsOf(t, table, cmp(identTok("email"), o.op, strTok(lit)))

				assert.Equal(t, want, got)
				assert.EqualValues(t, 0, cf.scans.Load(), "no scan")
			})
		}
	}
}

func TestRangeLookup_NegativeIntegers(t *testing.T) {
	table := intKeys(t, sequence(-3, 3)...)
	cf := countOn(table)
	tests := []struct {
		name  string
		where parser.Expression
		want  []string
	}{
		{"id >= -1", cmp(identTok("id"), parser.GTE, numTok("-1")), []string{"-1", "0", "1", "2", "3"}},
		{"id < -1", cmp(identTok("id"), parser.LT, numTok("-1")), []string{"-3", "-2"}},
		{"id > -3 AND id <= 0", andExpr(cmp(identTok("id"), parser.GT, numTok("-3")), cmp(identTok("id"), parser.LTE, numTok("0"))), []string{"-2", "-1", "0"}},
		{"id > 3", cmp(identTok("id"), parser.GT, numTok("3")), []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf.scans.Store(0)
			want := [][]string{}
			for _, id := range tc.want {
				want = append(want, []string{id})
			}

			assert.Equal(t, want, idsOf(t, table, tc.where))
			assert.EqualValues(t, 0, cf.scans.Load())
		})
	}
}

func TestRangeLookup_GivesUpOnABigRange(t *testing.T) {
	table := intKeys(t, sequence(0, 149)...)
	cf := countOn(table)

	t.Run("exactly the cap is a lookup", func(t *testing.T) {
		got := idsOf(t, table, andExpr(cmp(identTok("id"), parser.GTE, numTok("0")), cmp(identTok("id"), parser.LT, numTok("100"))))

		assert.Len(t, got, 100)
		assert.EqualValues(t, 0, cf.scans.Load())
		assert.EqualValues(t, 100, cf.gets.Load())
	})
	t.Run("one more row scans", func(t *testing.T) {
		cf.scans.Store(0)
		cf.gets.Store(0)

		got := idsOf(t, table, andExpr(cmp(identTok("id"), parser.GTE, numTok("0")), cmp(identTok("id"), parser.LTE, numTok("100"))))

		assert.Len(t, got, 101, "the answer is the same")
		assert.EqualValues(t, 1, cf.scans.Load())
		assert.EqualValues(t, 0, cf.gets.Load())
	})
}

func TestRangeLookup_DeleteAndUpdateTouchOnlyTheRange(t *testing.T) {
	table := intKeys(t, sequence(1, 20)...)
	cf := countOn(table)
	band := andExpr(cmp(identTok("id"), parser.GTE, numTok("5")), cmp(identTok("id"), parser.LTE, numTok("9")))

	require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("hit")}}, band))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 5, cf.gets.Load())
	assertIndexesMatchHeap(t, table)

	cf.scans.Store(0)
	cf.gets.Store(0)
	require.NoError(t, table.Delete(band))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 5, cf.gets.Load())
	assert.Len(t, fileRows(t, table), 15)
	assertIndexesMatchHeap(t, table)
}

// The scan returns rows by page and slot, so an updated row, which moves to the
// end, comes last. A range lookup must give the same order and write the same
// file.
func TestRangeLookup_RowsComeBackInFileOrder(t *testing.T) {
	indexed, plain := intKeys(t, sequence(1, 8)...), intKeys(t, sequence(1, 8)...)
	plain.noIndexes = true
	for _, table := range []*SqlTable{indexed, plain} {
		require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("moved")}}, eqCmp("id", numTok("2"))))
		require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("moved")}}, eqCmp("id", numTok("5"))))
	}
	cf := countOn(indexed)
	where := andExpr(cmp(identTok("id"), parser.GTE, numTok("1")), cmp(identTok("id"), parser.LTE, numTok("6")))

	got := idsOf(t, indexed, where)

	assert.Equal(t, [][]string{{"1"}, {"3"}, {"4"}, {"6"}, {"2"}, {"5"}}, got, "the moved rows are last")
	assert.Equal(t, idsOf(t, plain, where), got)
	assert.EqualValues(t, 0, cf.scans.Load(), "and it was a lookup")

	for _, table := range []*SqlTable{indexed, plain} {
		require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("again")}}, where))
	}
	assert.Equal(t, fileRows(t, plain), fileRows(t, indexed), "the same file")
}

func TestRangeLookup_TheWholeWhereIsStillChecked(t *testing.T) {
	table := builtKeyedTable(t) // ids 1..5, names n1..n5
	cf := countOn(table)

	t.Run("a condition on another column", func(t *testing.T) {
		resp, err := table.Select([]string{"id"}, andExpr(
			andExpr(cmp(identTok("id"), parser.GTE, numTok("1")), cmp(identTok("id"), parser.LTE, numTok("5"))),
			eqCmp("name", strTok("n3"))))

		require.NoError(t, err)
		assert.Equal(t, [][]string{{"3"}}, resp.Rows)
		assert.EqualValues(t, 5, cf.gets.Load(), "all five rows of the range were read")
	})
	t.Run("a bound on a second key column", func(t *testing.T) {
		resp, err := table.Select([]string{"id"}, andExpr(
			cmp(identTok("id"), parser.GTE, numTok("2")), cmp(identTok("email"), parser.LT, strTok("u4@x.com"))))

		require.NoError(t, err)
		assert.Equal(t, [][]string{{"2"}, {"3"}}, resp.Rows)
	})
	t.Run("an equality wins over a range", func(t *testing.T) {
		cf.gets.Store(0)

		resp, err := table.Select([]string{"id"}, andExpr(eqCmp("id", numTok("3")), cmp(identTok("id"), parser.GT, numTok("1"))))

		require.NoError(t, err)
		assert.Equal(t, [][]string{{"3"}}, resp.Rows)
		assert.EqualValues(t, 1, cf.gets.Load())
	})
}

func TestRangeLookup_SameAnswersAndErrorsAsTheScan(t *testing.T) {
	t.Run("strings and numbers", func(t *testing.T) {
		indexed, plain := builtKeyedTable(t), builtKeyedTable(t)
		plain.noIndexes = true
		for _, key := range []string{"2", "99"} {
			band := cmp(identTok("id"), parser.GTE, numTok(key))
			wheres := map[string]parser.Expression{
				"an unknown column in another conjunct": andExpr(band, eqCmp("nosuch", numTok("1"))),
				"a number against a string column":      andExpr(band, eqCmp("name", numTok("3"))),
				"a string against the key":              cmp(identTok("id"), parser.GT, strTok("x")),
				"a number out of range for the type":    cmp(identTok("id"), parser.LT, numTok("99999999999")),
				"two columns":                           andExpr(band, cmp(identTok("id"), parser.LT, identTok("name"))),
				"OR with an unknown column":             orExpr(band, eqCmp("nosuch", numTok("1"))),
				"a fine one, for comparison":            andExpr(band, cmp(identTok("name"), parser.NEQ, strTok("zzz"))),
			}
			for name, where := range wheres {
				t.Run(name+" from "+key, func(t *testing.T) {
					gotResp, gotErr := indexed.Select([]string{"*"}, where)
					wantResp, wantErr := plain.Select([]string{"*"}, where)

					assert.Equal(t, errText(wantErr), errText(gotErr))
					assert.Equal(t, wantResp, gotResp)
				})
			}
		}
	})

	t.Run("a boolean ordering comparison", func(t *testing.T) {
		indexed, plain := flagTable(t), flagTable(t)
		plain.noIndexes = true
		for _, key := range []string{"1", "2", "99"} {
			band := cmp(identTok("id"), parser.GTE, numTok(key))
			for name, where := range map[string]parser.Expression{
				"ordering on a boolean": andExpr(band, cmp(identTok("flag"), parser.GT, numTok("0"))),
				"equality on a boolean": andExpr(band, eqCmp("flag", numTok("1"))),
			} {
				t.Run(name+" from "+key, func(t *testing.T) {
					gotResp, gotErr := indexed.Select([]string{"*"}, where)
					wantResp, wantErr := plain.Select([]string{"*"}, where)

					assert.Equal(t, errText(wantErr), errText(gotErr))
					assert.Equal(t, wantResp, gotResp)
				})
			}
		}
	})
}

func TestRangeLookup_ADegradedColumnStillScans(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "same@x.com", "A"})
	writeRaw(t, table, []any{int64(2), "same@x.com", "B"}) // an interrupted UPDATE left two rows with one email
	cf := countOn(table)

	got := idsOf(t, table, cmp(identTok("email"), parser.GT, strTok("a")))

	assert.Equal(t, [][]string{{"1"}, {"2"}}, got)
	assert.GreaterOrEqual(t, cf.scans.Load(), int64(1))
	assert.EqualValues(t, 0, cf.gets.Load())
}

func TestRangeLookup_AStaleEntryFallsBackToTheScan(t *testing.T) {
	table := builtKeyedTable(t)
	key := keyOf(t, int64(3))
	id, found := table.builtIndexes()[colID].Get(key)
	require.True(t, found)
	require.NoError(t, table.file.Delete(id)) // behind the index's back

	got := idsOf(t, table, andExpr(cmp(identTok("id"), parser.GTE, numTok("2")), cmp(identTok("id"), parser.LTE, numTok("4"))))

	assert.Equal(t, [][]string{{"2"}, {"4"}}, got)
	assert.Nil(t, table.builtIndexes(), "the wrong index is discarded")
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/engine/ -run 'RangeLookup_' 2>&1 | grep -E '^(\s*--- FAIL|FAIL|ok)' | head -20`
Expected: FAIL. The bounds tests, `GivesUpOnABigRange` (the cap case), `DeleteAndUpdateTouchOnlyTheRange`, `RowsComeBackInFileOrder` and `TheWholeWhereIsStillChecked` report a scan where none is expected. `AStaleEntryFallsBackToTheScan` fails because nothing discards the indexes yet. `SameAnswersAndErrorsAsTheScan` and `ADegradedColumnStillScans` already pass: the scan is still the only path.

- [ ] **Step 3: Implement**

In `internal/engine/lookup.go`, replace the whole of `lookupRows`, including its doc comment, with the following, which also adds `maxRangeRows`, `rangeRows` and `fetchMatching`:

```go
// maxRangeRows is the most rows a range lookup fetches. Each fetch reads a
// page, while a scan reads each page once, so a range that holds more rows than
// this is left to the scan. It is a variable so a test can lower it.
var maxRangeRows = 100

// lookupRows serves where from an index when it can. usable is false when the
// statement must scan instead; otherwise rows holds the rows that satisfy
// where, in file order (the scan's order). A key pinned with = yields at most
// one row; a bounded range yields at most maxRangeRows. needTrees are columns
// that must have a tree for a lookup to be enough (an UPDATE's assigned unique
// columns). It may build the indexes, or discard them if one disagrees with the
// file. Callers hold t.mu.
func (t *SqlTable) lookupRows(where parser.Expression, columnIndex map[string]int, needTrees []int) (rows []fetchedRow, usable bool, err error) {
	plans := t.planLookup(where, columnIndex)
	if len(plans.equalities) == 0 && len(plans.ranges) == 0 {
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
			t.dropIndexes()
			return nil, false, nil
		}
		if !matched {
			return nil, true, nil
		}
		return []fetchedRow{{id: id, row: row}}, true, nil
	}
	for _, plan := range plans.ranges {
		if tree := trees[plan.column]; tree != nil {
			return t.rangeRows(tree, plan, where, columnIndex)
		}
	}
	return nil, false, nil
}

// rangeRows fetches the rows whose key lies in plan's range and that satisfy
// where, in file order. It reports not usable if the range holds more than
// maxRangeRows rows or an entry is stale.
func (t *SqlTable) rangeRows(tree *index.Tree, plan rangePlan, where parser.Expression, columnIndex map[string]int) ([]fetchedRow, bool, error) {
	var ids []storage.RowID
	for _, id := range tree.Range(plan.lo, plan.hi) {
		if len(ids) == maxRangeRows {
			return nil, false, nil
		}
		ids = append(ids, id)
	}
	// The scan returns rows by page and slot; so must we, or results and the
	// order of an UPDATE's new versions would depend on the access path.
	slices.SortFunc(ids, func(a, b storage.RowID) int {
		if a.Page != b.Page {
			if a.Page < b.Page {
				return -1
			}
			return 1
		}
		return a.Slot - b.Slot
	})

	var rows []fetchedRow
	for _, id := range ids {
		row, matched, stale, err := t.fetchMatching(id, where, columnIndex)
		if err != nil {
			return nil, false, err
		}
		if stale {
			t.dropIndexes()
			return nil, false, nil
		}
		if matched {
			rows = append(rows, fetchedRow{id: id, row: row})
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
	if row, err = DecodeRow(t.Columns, b); err != nil {
		return nil, false, false, err
	}
	matched, err = evalWhere(where, row, columnIndex, t.Columns)
	if err != nil {
		return nil, false, false, err
	}
	return row, matched, false, nil
}
```

- [ ] **Step 4: Run the whole engine suite**

Run: `gofmt -w internal/ && go vet ./internal/... && go test -race -count=1 ./internal/engine/`
Expected: PASS: the new tests and every existing test, including the part A and B differential tests, whose `WHERE`s now sometimes take the range path.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/lookup.go internal/engine/range_test.go internal/engine/lookup_test.go internal/engine/tableindex_test.go
git commit -m "feat(engine): answer a bounded key range from the index" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Differential coverage, documentation, mutation checks and a measurement

**Files:**
- Modify: `internal/engine/indexdiff_test.go`, `CLAUDE.md`
- Modify (temporarily, then reverted): `internal/engine/lookup.go`
- Create (temporarily, then deleted): `internal/engine/measure_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: a differential test whose `WHERE` generator also draws range shapes, plus a run with `maxRangeRows` lowered to 2; the `CLAUDE.md` update.

- [ ] **Step 1: Extend the differential test**

In `internal/engine/indexdiff_test.go`, in `randomWhere`, change `switch rng.Intn(14) {` to `switch rng.Intn(20) {`, and replace the `default` case

```go
	default:
		return cmp(identTok("id"), parser.EQ, strTok("oops"))
	}
}
```

with:

```go
	case 13:
		return cmp(identTok("id"), parser.GT, id())
	case 14:
		return cmp(identTok("id"), parser.LTE, id())
	case 15:
		return binaryExpr(cmp(identTok("id"), parser.GTE, id()), parser.AND, cmp(identTok("id"), parser.LT, id()))
	case 16:
		return cmp(identTok("email"), parser.GT, strTok(randEmail(rng)))
	case 17:
		return binaryExpr(cmp(identTok("id"), parser.GTE, id()), parser.AND, cmp(identTok("name"), parser.EQ, strTok(randName(rng))))
	case 18:
		return cmp(id(), parser.LT, identTok("id"))
	default:
		return cmp(identTok("id"), parser.EQ, strTok("oops"))
	}
}
```

Then replace the whole of `TestIndexedTableMatchesAnUnindexedOne` (from its doc comment to the end of the file) with:

```go
// compareWithUnindexed runs a long seeded mix of statements on a table with
// indexes and on one without, and requires identical results, errors and files
// after every statement.
func compareWithUnindexed(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	indexed, plain := keyedTable(t), keyedTable(t)
	plain.noIndexes = true

	for op := 0; op < 400; op++ {
		statement := randomStatement(rng)

		gotResp, gotErr := statement(indexed)
		wantResp, wantErr := statement(plain)

		require.Equal(t, errText(wantErr), errText(gotErr), "op %d", op)
		require.Equal(t, wantResp, gotResp, "op %d", op)
		require.Equal(t, fileRows(t, plain), fileRows(t, indexed), "op %d", op)
		if indexed.builtIndexes() != nil {
			assertIndexesMatchHeap(t, indexed)
		}
	}
	assert.NotNil(t, indexed.builtIndexes(), "the run must have used the indexes")
}

// With and without indexes, the same statements must give the same results,
// the same errors and the same file, whatever the mix.
func TestIndexedTableMatchesAnUnindexedOne(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) { compareWithUnindexed(t, seed) })
	}
}

// With the cap lowered to two rows most ranges are too big and scan: the
// answers must not change.
func TestIndexedTableMatchesWhenRangesExceedTheCap(t *testing.T) {
	saved := maxRangeRows
	maxRangeRows = 2
	t.Cleanup(func() { maxRangeRows = saved })
	for seed := int64(7); seed <= 8; seed++ {
		compareWithUnindexed(t, seed)
	}
}
```

- [ ] **Step 2: Run it**

Run: `gofmt -w internal/ && go test -race -count=1 ./internal/engine/ -run IndexedTableMatches -v 2>&1 | grep -E '^(\s*--- |FAIL|ok|PASS)'`
Expected: PASS for the three seeds and for the lowered-cap run. (It passes on the first run because the implementation is complete; Step 4 is what proves the tests can fail.) If it fails, the message names the operation number: fix the code, not the test, and ledger a ruling.

- [ ] **Step 3: Update `CLAUDE.md`**

In the Storage paragraph, replace

> so WHERE errors are exactly what the scan reports. A lookup trusts that the index has every key whose row exists.

with:

> so WHERE errors are exactly what the scan reports. A bounded range on a key column (`<`, `<=`, `>`, `>=`, alone or as a pair) is a lookup too when it holds at most 100 rows: the rows are fetched in file order, so results, their order and the file an UPDATE writes are the scan's; a bigger range scans. A lookup trusts that the index has every key whose row exists.

- [ ] **Step 4: Mutation checks**

For each row, make the one edit, run `go test -count=1 ./internal/engine/` (about 25 seconds), confirm a test fails (not a build failure), then restore with `git checkout internal/engine/lookup.go`.

| # | Edit | A test that must fail |
| --- | --- | --- |
| 1 | `addBound`: make `>` behave as `>=` (use `key`, not `after`, for `parser.GT`) | `TestPlanLookup_Ranges` |
| 2 | `addBound`: make `<=` behave as `<` | `TestPlanLookup_Ranges` and `TestRangeLookup_BoundsAreExact` |
| 3 | `rangeRows`: skip the sort of the row ids | `TestRangeLookup_RowsComeBackInFileOrder` |
| 4 | `rangeRows`: delete the `maxRangeRows` check | `TestRangeLookup_GivesUpOnABigRange` |
| 5 | `rangeRows`: keep every fetched row (`if matched || true`) | `TestRangeLookup_TheWholeWhereIsStillChecked` |
| 6 | `tighterLower`: keep the smaller lower bound | `TestPlanLookup_Ranges` |
| 7 | `lookupRows`: range a column with no tree (drop the `tree != nil` check) | `TestRangeLookup_ADegradedColumnStillScans` |
| 8 | `columnAgainstLiteral`: do not flip the operator when the literal is on the left | `TestPlanLookup_Ranges` and `TestRangeLookup_BoundsAreExact` (reversed operands) |
| 9 | `tighterUpper`: keep the larger upper bound | `TestPlanLookup_Ranges` |

Expected: all nine fail. A mutant that survives means a missing test: add one that fails for it, then continue.

- [ ] **Step 5: Measure (one-off, not committed)**

Create `internal/engine/measure_test.go` with:

```go
package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// TestMeasureRanges is a one-off measurement, not part of the suite:
// run it with `go test ./internal/engine/ -run MeasureRanges -v -count=1`.
func TestMeasureRanges(t *testing.T) {
	const rows = 100000
	build := func(noIndexes bool) *SqlTable {
		table := keyedTable(t)
		table.noIndexes = noIndexes
		for start := 0; start < rows; start += 1000 {
			batch := make([][]parser.Token, 0, 1000)
			for i := start; i < start+1000; i++ {
				batch = append(batch, tokRow(numTok(fmt.Sprint(i)), strTok(fmt.Sprintf("user%d@example.com", i)), strTok("name")))
			}
			require.NoError(t, table.InsertValues(nil, batch))
		}
		return table
	}
	band := func(from, width int) parser.Expression {
		return andExpr(cmp(identTok("id"), parser.GTE, numTok(fmt.Sprint(from))), cmp(identTok("id"), parser.LT, numTok(fmt.Sprint(from+width))))
	}
	measure := func(label string, n int, run func(i int) error) {
		start := time.Now()
		for i := 0; i < n; i++ {
			require.NoError(t, run(i))
		}
		t.Logf("%-44s %9.3f ms/op  (%d ops)", label, float64(time.Since(start))/float64(n)/1e6, n)
	}
	for _, mode := range []struct {
		name      string
		noIndexes bool
		ops       int
	}{{"scan   (indexes off)", true, 20}, {"lookup (indexes on)", false, 100}} {
		table := build(mode.noIndexes)
		_, err := table.Select([]string{"id"}, band(0, 1)) // builds the indexes when they are on
		require.NoError(t, err)
		for _, width := range []int{10, 100, 1000} {
			measure(fmt.Sprintf("%s SELECT a range of %d rows", mode.name, width), mode.ops, func(i int) error {
				_, err := table.Select([]string{"*"}, band(i*997%(rows-width), width))
				return err
			})
		}
		measure(mode.name+" UPDATE a range of 10 rows", mode.ops, func(i int) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("changed")}}, band(i*991%(rows-10), 10))
		})
	}
}
```

Run: `go test ./internal/engine/ -run MeasureRanges -v -count=1 2>&1 | grep -E 'ms/op|ok|FAIL'`
Expected: `ms/op` lines for a range `SELECT` of 10, 100 and 1000 rows and a 10-row `UPDATE`, scan against lookup, on a 100,000-row table. The 1000-row range is over the cap, so both rows of that pair scan. Copy the lines into the PR description, then delete the file (`rm internal/engine/measure_test.go`) and confirm `git status --short` does not list it.

- [ ] **Step 6: Run everything**

Run: `git status --short` (expected: only `CLAUDE.md` and `internal/engine/indexdiff_test.go`), then `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: every package passes and `gofmt` and `go vet` print nothing.

- [ ] **Step 7: Commit**

```bash
git add CLAUDE.md internal/engine/indexdiff_test.go
git commit -m "test(engine): compare range lookups with scans, including past the cap" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
