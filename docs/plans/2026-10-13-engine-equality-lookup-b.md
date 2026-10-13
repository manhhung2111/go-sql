# Engine equality lookup (part B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `SELECT`, `UPDATE` and `DELETE` whose `WHERE` pins a PRIMARY KEY or UNIQUE column to a literal from the table's index: one tree lookup and one `File.Get` instead of a scan, with results and errors identical to the scan.

**Architecture:** A new `lookup.go` recognises a `WHERE` that can be served from an index (`planLookup`: an AND chain of column-against-literal comparisons that cannot fail on any row) and fetches the one candidate row (`lookupRows`). `Select`, `deleteFromFile` and `Update` try it first and fall back to their existing scan when it reports `usable == false`. A stale entry (`File.Get` says `ErrRowNotFound`) discards the indexes and falls back too.

**Tech Stack:** Go 1.26.4, `internal/index`, `internal/storage` (`File.Get`, `ErrRowNotFound`), testify.

**Spec:** `docs/specs/2026-10-13-engine-equality-lookup-design.md`

**Prerequisite:** part A (`docs/plans/2026-10-12-engine-index-wiring-a.md`, PR #37) is merged: this plan uses its `indexes()`, `builtIndexes()`, `dropIndexes()`, `keyColumns()`, `keyValuesAt`, `countOn`, `builtKeyedTable`, `keyedTable`, `writeRaw`, `assertIndexesMatchHeap` and the differential test.

## Global Constraints

- Go 1.26.4; run `go test -race ./...` (required when touching `internal/engine`).
- A lookup is used only when the whole `WHERE` cannot fail at run time: an AND chain of comparisons that each have one column and one literal, the column exists, the literal coerces to a non-NULL value of the column's type, and a boolean column is compared with `=` or `!=` only. Everything else scans exactly as before, so results and errors do not change.
- The key comes from the first `=` on a PRIMARY KEY/UNIQUE column that has a tree; the whole `WHERE` is then evaluated on the one fetched row.
- `UPDATE` uses a lookup only if every unique column it assigns has a tree.
- `File.Get` returning `ErrRowNotFound` discards the indexes and falls back to the scan. Any other `Get` error is returned.
- Whether there is an index to update (`trackKeys`) is decided AFTER `lookupRows`, because it can build the indexes.
- Lock order is unchanged. `Select` keeps the read lock; `lookupRows` may build or discard the indexes under it.
- Error messages: lowercase, no trailing punctuation. Run `gofmt -w internal/` after each edit.
- Tests use testify; every test is seen failing before the code that makes it pass.

## Review Focus

- A `WHERE` that could fail (`id = 5 AND nosuch = 1`, a number against a string column, a boolean ordering operator) must give the scan's error even when the key is absent. Pinned in Task 1 (`TestPlanLookup`) and Task 2 (`SameAnswersAndErrorsAsTheScan`).
- A point statement that is the first to need the indexes builds them inside `lookupRows` and must still update them afterwards. Pinned in Task 3.
- `id = 3 AND name = 'wrong'` finds key 3 but must return and change nothing. Pinned in Tasks 2 and 3.
- An `UPDATE` assigning a column that has no tree must still scan. Pinned in Task 3.
- A stale entry must cost speed, not correctness. Pinned in Task 3. A lookup cannot notice an index that is missing a key whose row exists; write maintenance and the differential test guard that.

---

### Task 1: Recognise a lookup (`planLookup`)

**Files:**
- Create: `internal/engine/lookup.go`
- Test: `internal/engine/lookup_test.go` (new)

**Interfaces:**
- Consumes: `coerceValue`, `parser.ComparisonExpression`, `parser.BinaryExpression`, `SqlTable.Columns`.
- Produces: `type lookupPlan struct{ column int; value any }`, `(*SqlTable).planLookup(where parser.Expression, columnIndex map[string]int) []lookupPlan`, `collectConjuncts`, `(*SqlTable).columnAgainstLiteral`, `(*SqlTable).isKeyColumn`; test helpers `eqCmp`, `andExpr`, `orExpr`, `flagTable`.

- [ ] **Step 1: Write the failing tests**

Create `internal/engine/lookup_test.go`:

```go
package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func eqCmp(column string, literal parser.Token) *parser.ComparisonExpression {
	return cmp(identTok(column), parser.EQ, literal)
}

func andExpr(l, r parser.Expression) parser.Expression { return binaryExpr(l, parser.AND, r) }

func orExpr(l, r parser.Expression) parser.Expression  { return binaryExpr(l, parser.OR, r) }

// flagTable has a PRIMARY KEY (id) and a BOOLEAN column that is not a key.
func flagTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "flags", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "flag", DataType: parser.BooleanDataType{}},
	})
	require.NoError(t, err)
	writeRaw(t, table, []any{int64(1), true})
	writeRaw(t, table, []any{int64(2), false})
	writeRaw(t, table, []any{int64(3), nil})
	return table
}

func TestPlanLookup(t *testing.T) {
	table := keyedTable(t)
	columnIndex := map[string]int{"id": colID, "email": colEmail, "name": colName}
	null := parser.Token{Type: parser.NULL, Value: "NULL"}
	tests := []struct {
		name  string
		where parser.Expression
		want  []lookupPlan
	}{
		{"no where", nil, nil},
		{"primary key equality", eqCmp("id", numTok("3")), []lookupPlan{{colID, int64(3)}}},
		{"reversed operands", cmp(numTok("3"), parser.EQ, identTok("id")), []lookupPlan{{colID, int64(3)}}},
		{"unique column", eqCmp("email", strTok("a@x.com")), []lookupPlan{{colEmail, "a@x.com"}}},
		{"AND with another condition", andExpr(eqCmp("id", numTok("3")), eqCmp("name", strTok("x"))), []lookupPlan{{colID, int64(3)}}},
		{"AND in either order", andExpr(eqCmp("name", strTok("x")), eqCmp("id", numTok("3"))), []lookupPlan{{colID, int64(3)}}},
		{"AND with a range", andExpr(eqCmp("id", numTok("3")), cmp(identTok("name"), parser.GT, strTok("a"))), []lookupPlan{{colID, int64(3)}}},
		{"nested AND with two keys", andExpr(andExpr(eqCmp("id", numTok("3")), eqCmp("name", strTok("x"))), eqCmp("email", strTok("e"))),
			[]lookupPlan{{colID, int64(3)}, {colEmail, "e"}}},
		{"not a key column", eqCmp("name", strTok("x")), nil},
		{"a range on the key", cmp(identTok("id"), parser.GTE, numTok("3")), nil},
		{"not equal on the key", cmp(identTok("id"), parser.NEQ, numTok("3")), nil},
		{"OR", orExpr(eqCmp("id", numTok("3")), eqCmp("id", numTok("4"))), nil},
		{"OR below an AND", andExpr(eqCmp("id", numTok("3")), orExpr(eqCmp("name", strTok("a")), eqCmp("name", strTok("b")))), nil},
		{"an unknown column anywhere", andExpr(eqCmp("id", numTok("3")), eqCmp("nosuch", numTok("1"))), nil},
		{"a literal that does not coerce anywhere", andExpr(eqCmp("id", numTok("3")), eqCmp("name", numTok("3"))), nil},
		{"a string against an INT key", eqCmp("id", strTok("x")), nil},
		{"a number out of range for the type", eqCmp("id", numTok("99999999999")), nil},
		{"two columns", cmp(identTok("id"), parser.EQ, identTok("name")), nil},
		{"two literals", cmp(numTok("1"), parser.EQ, numTok("1")), nil},
		{"NULL", cmp(identTok("id"), parser.EQ, null), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, table.planLookup(tc.where, columnIndex))
		})
	}
}

func TestPlanLookup_BooleanColumnsOnlyTakeEqualityOperators(t *testing.T) {
	table := flagTable(t)
	columnIndex := map[string]int{"id": 0, "flag": 1}
	byID := eqCmp("id", numTok("1"))
	want := []lookupPlan{{0, int64(1)}}

	assert.Equal(t, want, table.planLookup(andExpr(byID, eqCmp("flag", numTok("1"))), columnIndex), "= on a boolean is fine")
	assert.Equal(t, want, table.planLookup(andExpr(byID, cmp(identTok("flag"), parser.NEQ, numTok("1"))), columnIndex), "so is !=")
	assert.Nil(t, table.planLookup(andExpr(byID, cmp(identTok("flag"), parser.GT, numTok("0"))), columnIndex),
		"an ordering operator on a boolean fails for every row that has a value")
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go vet ./internal/engine/ 2>&1 | head`
Expected: FAIL to compile: `undefined: lookupPlan` and `table.planLookup undefined`.

- [ ] **Step 3: Implement**

Create `internal/engine/lookup.go`:

```go
package engine

import "manhhung2111/go-sql/internal/parser"

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
```

- [ ] **Step 4: Run them to verify they pass**

Run: `gofmt -w internal/ && go test -race -count=1 ./internal/engine/ -run PlanLookup -v 2>&1 | grep -E '^(--- |FAIL|ok|PASS)'`
Expected: PASS for `TestPlanLookup` (all 19 cases) and `TestPlanLookup_BooleanColumnsOnlyTakeEqualityOperators`.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/lookup.go internal/engine/lookup_test.go
git commit -m "feat(engine): recognise a WHERE that an index can serve" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `lookupRows`, and `SELECT` through it

**Files:**
- Modify: `internal/engine/lookup.go`, `internal/engine/table.go`
- Test: `internal/engine/lookup_test.go`

**Interfaces:**
- Consumes: Task 1's `planLookup`; part A's `indexes`, `dropIndexes`; `storage.File.Get`, `storage.ErrRowNotFound`; `DecodeRow`, `evalWhere`.
- Produces: `type fetchedRow struct{ id storage.RowID; row []any }` and `(*SqlTable).lookupRows(where, columnIndex, needTrees []int) (rows []fetchedRow, usable bool, err error)`; `Select` that uses it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/engine/lookup_test.go`:

```go
func TestLookup_SelectByKeyReadsOneRow(t *testing.T) {
	table := builtKeyedTable(t)
	cf := countOn(table)
	tests := []struct {
		name  string
		where parser.Expression
		want  [][]string
		gets  int64
	}{
		{"primary key", eqCmp("id", numTok("3")), [][]string{{"3", "u3@x.com", "n3"}}, 1},
		{"unique column", eqCmp("email", strTok("u4@x.com")), [][]string{{"4", "u4@x.com", "n4"}}, 1},
		{"reversed operands", cmp(numTok("2"), parser.EQ, identTok("id")), [][]string{{"2", "u2@x.com", "n2"}}, 1},
		{"another condition that holds", andExpr(eqCmp("id", numTok("3")), eqCmp("name", strTok("n3"))), [][]string{{"3", "u3@x.com", "n3"}}, 1},
		{"another condition that fails", andExpr(eqCmp("id", numTok("3")), eqCmp("name", strTok("wrong"))), [][]string{}, 1},
		{"a range beside the key", andExpr(eqCmp("id", numTok("3")), cmp(identTok("id"), parser.GT, numTok("5"))), [][]string{}, 1},
		{"an absent key", eqCmp("id", numTok("99")), [][]string{}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf.scans.Store(0)
			cf.gets.Store(0)

			resp, err := table.Select([]string{"*"}, tc.where)

			require.NoError(t, err)
			assert.Equal(t, []string{"id", "email", "name"}, resp.Columns)
			assert.Equal(t, tc.want, resp.Rows)
			assert.EqualValues(t, 0, cf.scans.Load(), "no scan")
			assert.EqualValues(t, tc.gets, cf.gets.Load())
		})
	}
}

func TestLookup_WhatItCannotServeStillScans(t *testing.T) {
	table := builtKeyedTable(t)
	cf := countOn(table)
	tests := []struct {
		name  string
		where parser.Expression
	}{
		{"no where", nil},
		{"a range", cmp(identTok("id"), parser.GTE, numTok("3"))},
		{"a column that is not a key", eqCmp("name", strTok("n3"))},
		{"OR", orExpr(eqCmp("id", numTok("1")), eqCmp("id", numTok("2")))},
		{"not equal", cmp(identTok("id"), parser.NEQ, numTok("3"))},
		{"two columns", cmp(identTok("id"), parser.EQ, identTok("id"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf.scans.Store(0)
			cf.gets.Store(0)

			_, err := table.Select([]string{"*"}, tc.where)

			require.NoError(t, err)
			assert.EqualValues(t, 1, cf.scans.Load())
			assert.EqualValues(t, 0, cf.gets.Load())
		})
	}
}

func TestLookup_IsOffWhenIndexesAre(t *testing.T) {
	table := builtKeyedTable(t)
	table.noIndexes = true
	cf := countOn(table)

	resp, err := table.Select([]string{"*"}, eqCmp("id", numTok("3")))

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"3", "u3@x.com", "n3"}}, resp.Rows)
	assert.EqualValues(t, 1, cf.scans.Load())
	assert.EqualValues(t, 0, cf.gets.Load())
}

// A where that could fail must fail, or not, exactly as the scan does, for a
// key that is present and for one that is absent.
func TestLookup_SameAnswersAndErrorsAsTheScan(t *testing.T) {
	t.Run("strings and numbers", func(t *testing.T) {
		indexed, plain := builtKeyedTable(t), builtKeyedTable(t)
		plain.noIndexes = true
		for _, key := range []string{"3", "99"} {
			wheres := map[string]parser.Expression{
				"unknown column in another conjunct":    andExpr(eqCmp("id", numTok(key)), eqCmp("nosuch", numTok("1"))),
				"a number against a string column":     andExpr(eqCmp("id", numTok(key)), eqCmp("name", numTok("3"))),
				"a string against the key":              eqCmp("id", strTok("x")),
				"an ordering against the wrong type":    andExpr(eqCmp("id", numTok(key)), cmp(identTok("name"), parser.GT, numTok("1"))),
				"OR with an unknown column":             orExpr(eqCmp("id", numTok(key)), eqCmp("nosuch", numTok("1"))),
				"two columns":                           andExpr(eqCmp("id", numTok(key)), cmp(identTok("id"), parser.EQ, identTok("name"))),
				"a number out of range for the type":    eqCmp("id", numTok("99999999999")),
				"a fine one, for comparison":            andExpr(eqCmp("id", numTok(key)), cmp(identTok("name"), parser.NEQ, strTok("zzz"))),
			}
			for name, where := range wheres {
				t.Run(name+" key "+key, func(t *testing.T) {
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
		for _, key := range []string{"1", "2", "3", "99"} {
			wheres := map[string]parser.Expression{
				"ordering on a boolean":        andExpr(eqCmp("id", numTok(key)), cmp(identTok("flag"), parser.GT, numTok("0"))),
				"equality on a boolean":        andExpr(eqCmp("id", numTok(key)), eqCmp("flag", numTok("1"))),
				"not equal on a boolean":       andExpr(eqCmp("id", numTok(key)), cmp(identTok("flag"), parser.NEQ, numTok("1"))),
				"ordering alone, for contrast": cmp(identTok("flag"), parser.GT, numTok("0")),
			}
			for name, where := range wheres {
				t.Run(name+" key "+key, func(t *testing.T) {
					gotResp, gotErr := indexed.Select([]string{"*"}, where)
					wantResp, wantErr := plain.Select([]string{"*"}, where)

					assert.Equal(t, errText(wantErr), errText(gotErr))
					assert.Equal(t, wantResp, gotResp)
				})
			}
		}
	})
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/engine/ -run 'Lookup_' 2>&1 | grep -E '^(\s*--- FAIL|FAIL|ok)' | head`
Expected: FAIL. The `SelectByKeyReadsOneRow` cases report `no scan` (1 scan where 0 are expected) and a missing `Get`, and `SameAnswersAndErrorsAsTheScan` passes (the scan is still the only path) while `WhatItCannotServeStillScans` and `IsOffWhenIndexesAre` also pass: they pin behaviour that must not change.

- [ ] **Step 3: Implement**

In `internal/engine/lookup.go`, replace the import block with:

```go
import (
	"errors"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)
```

and append:

```go
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
```

In `internal/engine/table.go`, in `Select`:

Replace the end of its doc comment

```text
// back in the file's order: insertion order, except that an updated row has
// moved to the end.
```

with:

```text
// back in the file's order: insertion order, except that an updated row has
// moved to the end. A where that pins a PRIMARY KEY/UNIQUE column to a literal
// and cannot fail on any row reads just that row through the index instead.
```

and replace the body from `rows := make([][]string, 0)` to the final `return Response{...}, nil`:

```go
	rows := make([][]string, 0)
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

		projected := make([]string, len(outputIndices))
		for i, idx := range outputIndices {
			projected[i] = stringifyValue(row[idx])
		}
		rows = append(rows, projected)
	}

	return Response{Columns: outputColumns, Rows: rows}, nil
```

with:

```go
	rows := make([][]string, 0)
	project := func(row []any) {
		projected := make([]string, len(outputIndices))
		for i, idx := range outputIndices {
			projected[i] = stringifyValue(row[idx])
		}
		rows = append(rows, projected)
	}

	// A WHERE that pins a PRIMARY KEY/UNIQUE column to a literal is served
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
```

- [ ] **Step 4: Run the whole engine suite**

Run: `gofmt -w internal/ && go vet ./internal/... && go test -race -count=1 ./internal/engine/`
Expected: PASS: the new tests and every existing test, including the part A differential test, whose `WHERE`s now sometimes take the lookup.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/lookup.go internal/engine/table.go internal/engine/lookup_test.go
git commit -m "feat(engine): answer a SELECT that pins a key from the index" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `DELETE` and `UPDATE` through the index

**Files:**
- Modify: `internal/engine/table.go`
- Test: `internal/engine/lookup_test.go`, `internal/engine/tableindex_test.go`

**Interfaces:**
- Consumes: Task 2's `lookupRows` and `fetchedRow`; part A's `keyValuesAt`, `indexDeleted`, `indexMoved`, `trackKeys` pattern.
- Produces: `deleteFromFile` and `Update` that use a lookup when `usable`, and otherwise scan as before.

- [ ] **Step 1: Write the failing tests**

In `internal/engine/lookup_test.go`, replace the import block with:

```go
import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
)
```

and append:

```go
func TestLookup_DeleteByKeyTouchesOneRow(t *testing.T) {
	table := builtKeyedTable(t)
	cf := countOn(table)

	require.NoError(t, table.Delete(eqCmp("id", numTok("2"))))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 1, cf.gets.Load())
	assert.Len(t, fileRows(t, table), 4)
	assertIndexesMatchHeap(t, table)

	cf.gets.Store(0)
	require.NoError(t, table.Delete(andExpr(eqCmp("id", numTok("3")), eqCmp("name", strTok("wrong")))))
	assert.Len(t, fileRows(t, table), 4, "a key whose row fails the rest of the WHERE is left alone")
	assert.EqualValues(t, 1, cf.gets.Load())

	require.NoError(t, table.Delete(eqCmp("id", numTok("99"))))
	assert.EqualValues(t, 1, cf.gets.Load(), "an absent key reads nothing")
	assertIndexesMatchHeap(t, table)
}

func TestLookup_UpdateByKeyTouchesOneRow(t *testing.T) {
	table := builtKeyedTable(t)
	cf := countOn(table)

	require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("z")}}, eqCmp("id", numTok("2"))))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 1, cf.gets.Load())
	assertIndexesMatchHeap(t, table)
	cf.scans.Store(0) // the helper above scans to build a fresh copy
	resp, err := table.Select([]string{"name"}, eqCmp("id", numTok("2")))
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"z"}}, resp.Rows)

	err = table.Update([]parser.Assignment{{Column: "email", Value: strTok("u4@x.com")}}, eqCmp("id", numTok("2")))
	assert.EqualError(t, err, `duplicate entry u4@x.com for column "email"`)
	assert.EqualValues(t, 0, cf.scans.Load(), "the conflict was found through the index too")

	require.NoError(t, table.Update([]parser.Assignment{{Column: "email", Value: strTok("u2@x.com")}}, eqCmp("id", numTok("2"))),
		"re-assigning a row's own value is not a conflict")
	cf.gets.Store(0)
	require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("q")}}, eqCmp("id", numTok("99"))))
	assert.EqualValues(t, 0, cf.gets.Load())
	assertIndexesMatchHeap(t, table)
}

func TestLookup_UpdatingAColumnWithoutATreeStillScans(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "same@x.com", "A"})
	writeRaw(t, table, []any{int64(2), "same@x.com", "B"}) // an interrupted UPDATE left two rows with one email
	writeRaw(t, table, []any{int64(3), "c@x.com", "C"})
	cf := countOn(table)

	err := table.Update([]parser.Assignment{{Column: "email", Value: strTok("same@x.com")}}, eqCmp("id", numTok("3")))

	assert.EqualError(t, err, `duplicate entry same@x.com for column "email"`, "found by the scan, since email has no tree")
	assert.GreaterOrEqual(t, cf.scans.Load(), int64(1))
}

// A point statement can be the first thing to need the indexes. It builds them
// while looking the row up, and must still update them afterwards.
func TestLookup_TheStatementThatBuildsTheIndexesStillMaintainsThem(t *testing.T) {
	statements := map[string]func(table *SqlTable) error{
		"delete": func(table *SqlTable) error { return table.Delete(eqCmp("id", numTok("3"))) },
		"update a plain column": func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("z")}}, eqCmp("id", numTok("3")))
		},
		"update a key column": func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "email", Value: strTok("new@x.com")}}, eqCmp("id", numTok("3")))
		},
	}
	for name, run := range statements {
		t.Run(name, func(t *testing.T) {
			table := keyedTable(t)
			for i := 1; i <= 5; i++ {
				writeRaw(t, table, []any{int64(i), fmt.Sprintf("u%d@x.com", i), fmt.Sprintf("n%d", i)})
			}
			require.Nil(t, table.builtIndexes())

			require.NoError(t, run(table))

			assertIndexesMatchHeap(t, table)
		})
	}
}

// An index entry whose row is gone means the index is wrong. The statement
// still answers correctly, and the index is rebuilt for the next one.
func TestLookup_AStaleEntryFallsBackToTheScanAndDiscardsTheIndexes(t *testing.T) {
	staleTable := func(t *testing.T) *SqlTable {
		table := builtKeyedTable(t)
		key, err := index.EncodeKey(int64(3))
		require.NoError(t, err)
		id, found := table.builtIndexes()[colID].Get(key)
		require.True(t, found)
		require.NoError(t, table.file.Delete(id)) // behind the index's back
		return table
	}

	t.Run("select", func(t *testing.T) {
		table := staleTable(t)

		resp, err := table.Select([]string{"*"}, eqCmp("id", numTok("3")))

		require.NoError(t, err)
		assert.Equal(t, [][]string{}, resp.Rows)
		assert.Nil(t, table.builtIndexes(), "the wrong index is discarded")

		resp, err = table.Select([]string{"*"}, eqCmp("id", numTok("4")))
		require.NoError(t, err)
		assert.Equal(t, [][]string{{"4", "u4@x.com", "n4"}}, resp.Rows)
		assertIndexesMatchHeap(t, table)
	})
	t.Run("delete", func(t *testing.T) {
		table := staleTable(t)

		require.NoError(t, table.Delete(eqCmp("id", numTok("3"))))

		assert.Len(t, fileRows(t, table), 4)
		_, err := table.indexes()
		require.NoError(t, err)
		assertIndexesMatchHeap(t, table)
	})
	t.Run("update", func(t *testing.T) {
		table := staleTable(t)

		require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("z")}}, eqCmp("id", numTok("3"))))

		assert.Len(t, fileRows(t, table), 4)
		_, err := table.indexes()
		require.NoError(t, err)
		assertIndexesMatchHeap(t, table)
	})
}

// Select holds only the read lock, so several point selects can race to build
// the indexes: they must build them once and all get the right row.
func TestLookup_ConcurrentSelectsBuildTheIndexesOnce(t *testing.T) {
	table := keyedTable(t)
	for i := 0; i < 200; i++ {
		writeRaw(t, table, []any{int64(i), fmt.Sprintf("u%d@x.com", i), "n"})
	}
	cf := countOn(table)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < 200; i += 8 {
				resp, err := table.Select([]string{"id"}, eqCmp("id", numTok(fmt.Sprint(i))))
				if assert.NoError(t, err) {
					assert.Equal(t, [][]string{{fmt.Sprint(i)}}, resp.Rows)
				}
			}
		}(g)
	}
	wg.Wait()

	assert.EqualValues(t, 1, cf.scans.Load(), "one scan, to build the indexes")
}
```

In `internal/engine/tableindex_test.go`, in `TestTableIndexes_AMissingEntryDiscardsThem`, replace the two cases

```go
		{"delete", func(table *SqlTable) error {
			return table.Delete(cmp(identTok("id"), parser.EQ, numTok("3")))
		}},
		{"update", func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, cmp(identTok("id"), parser.EQ, numTok("3")))
		}},
```

with:

```go
		// These WHEREs scan. A key lookup would not notice the missing entry:
		// it would find nothing and stop.
		{"delete", func(table *SqlTable) error {
			return table.Delete(cmp(identTok("id"), parser.GTE, numTok("3")))
		}},
		{"update", func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, cmp(identTok("name"), parser.EQ, strTok("n3")))
		}},
```

(These statements must still scan, to reach the maintenance code: a key lookup that finds nothing stops.)

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/engine/ -run 'Lookup_' 2>&1 | grep -E '^(\s*--- FAIL|FAIL|ok)' | head -20`
Expected: FAIL. `DeleteByKeyTouchesOneRow` and `UpdateByKeyTouchesOneRow` see scans where none are expected, and the `AStaleEntry...` delete and update cases leave the stale index built. `TheStatementThatBuildsTheIndexesStillMaintainsThem` fails because nothing builds the indexes for a point statement yet. `ConcurrentSelectsBuildTheIndexesOnce` and `UpdatingAColumnWithoutATreeStillScans` already pass: they pin behaviour that must not change.

- [ ] **Step 3: Implement**

In `internal/engine/table.go`, in `deleteFromFile`:

Replace the end of its doc comment

```text
// where, then fsyncs once, then removes the rows from the table's indexes.
```

with:

```text
// where, then fsyncs once, then removes the rows from the table's indexes. A
// where that pins a key is served from the index instead of a scan.
```

and replace the block from `// Only the key values of the matching rows` through the end of the scan loop:

```go
	// Only the key values of the matching rows are kept, and only if there is
	// an index to update.
	trackKeys := t.builtIndexes() != nil
	keyCols := t.keyColumns()
	var matched []storage.RowID
	var keys [][]any
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
```

with:

```go
	// A where that pins a key is served from the index. That can build the
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
```

In `Update`:

Replace the line

```text
// produce NULL. The table's file is scanned once to find the matching rows. A
```

in its doc comment with:

```text
// produce NULL. The matching rows are found by one scan of the table's file, or
// by one index lookup when where pins a key and cannot fail. A
```

and replace the block from `var trees map[int]*index.Tree` through the end of the scan loop:

```go
	var trees map[int]*index.Tree
	if len(uniqueColumns) > 0 {
		var err error
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
			// A column with an index is checked after the scan; one without
			// is checked here. A matched row's own old value is deliberately
			// excluded, so re-assigning a column back to its current value
			// never self-conflicts.
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
```

with:

```go
	// A where that pins a key is served from the index, but only if every
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
```

- [ ] **Step 4: Run the whole engine suite**

Run: `gofmt -w internal/ && go vet ./internal/... && go test -race -count=1 ./internal/engine/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/table.go internal/engine/lookup_test.go internal/engine/tableindex_test.go
git commit -m "feat(engine): serve UPDATE and DELETE that pin a key from the index" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Differential coverage, documentation, mutation checks and a measurement

**Files:**
- Modify: `internal/engine/indexdiff_test.go`, `CLAUDE.md`
- Modify (temporarily, then reverted): `internal/engine/lookup.go`, `internal/engine/table.go`, `internal/engine/tableindex.go`
- Create (temporarily, then deleted): `internal/engine/measure_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: a differential test whose `WHERE` generator also draws the shapes a lookup serves (key equalities joined by `AND`, mixed with ranges and non-key conditions) and the ones it must refuse; the `CLAUDE.md` update.

- [ ] **Step 1: Extend the differential test's `WHERE` shapes**

In `internal/engine/indexdiff_test.go`, in `randomWhere`, replace

```go
	switch rng.Intn(9) {
	case 0:
		return nil
```

with:

```go
	switch rng.Intn(14) {
	case 0:
		return nil
```

and replace the `default` case

```go
	default:
		return cmp(identTok("id"), parser.EQ, strTok("oops"))
	}
}
```

with:

```go
	case 8:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.AND, cmp(identTok("email"), parser.EQ, strTok(randEmail(rng))))
	case 9:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.AND, cmp(identTok("id"), parser.GTE, id()))
	case 10:
		return binaryExpr(cmp(identTok("email"), parser.EQ, strTok(randEmail(rng))), parser.AND, cmp(identTok("name"), parser.NEQ, strTok(randName(rng))))
	case 11:
		return binaryExpr(cmp(id(), parser.EQ, identTok("id")), parser.AND, cmp(strTok(randName(rng)), parser.EQ, identTok("name")))
	case 12:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.AND, cmp(identTok("nosuch"), parser.EQ, numTok("1")))
	default:
		return cmp(identTok("id"), parser.EQ, strTok("oops"))
	}
}
```

- [ ] **Step 2: Run it**

Run: `gofmt -w internal/ && go test -race -count=1 ./internal/engine/ -run IndexedTableMatches -v 2>&1 | grep -E '^(\s*--- |FAIL|ok|PASS)'`
Expected: PASS for the three seeds. (It passes on the first run because the implementation is complete; Step 4 is what proves the tests can fail.) If it fails, the message names the operation number: fix the code, not the test, and ledger a ruling.

- [ ] **Step 3: Update `CLAUDE.md`**

In the Storage paragraph, replace the sentence

> A PRIMARY KEY/UNIQUE string longer than 4084 bytes is rejected with `key too long`.

with:

> A PRIMARY KEY/UNIQUE string longer than 4084 bytes is rejected with `key too long`. SELECT, UPDATE and DELETE read one row through the index (one lookup and one `File.Get`) when the WHERE is an AND chain of column-against-literal comparisons that cannot fail on any row and pins a key column with `=`; any other WHERE scans, so errors are exactly what the scan reports. A lookup trusts that the index has every key whose row exists.

- [ ] **Step 4: Mutation checks**

For each row, make the one edit, run `go test -count=1 ./internal/engine/` (about 20 seconds), confirm a test fails (not a build failure), then restore the file with `git checkout internal/engine/`.

| # | Edit | A test that must fail |
| --- | --- | --- |
| 1 | `lookupRows`: replace the `if !matched {` block with `_ = matched` | `TestLookup_SelectByKeyReadsOneRow` (another condition that fails) or the differential test |
| 2 | `lookupRows`: in the `ErrRowNotFound` branch, return `nil, true, nil` instead of dropping the indexes and falling back | `TestLookup_AStaleEntryFallsBack...` |
| 3 | `collectConjuncts`: drop the `e.Operator == parser.AND &&` check | `TestPlanLookup` (OR cases) |
| 4 | `columnAgainstLiteral`: make the boolean restriction never apply (`isBool && false && ...`) | `TestPlanLookup_BooleanColumnsOnlyTakeEqualityOperators` |
| 5 | `lookupRows`: delete the `if tree == nil { continue }` guard | a test using a column with no tree, or the differential test |
| 6 | `lookupRows`: delete the `needTrees` loop | `TestLookup_UpdatingAColumnWithoutATreeStillScans` |
| 7 | `deleteFromFile`: decide `trackKeys` before calling `lookupRows` | `TestLookup_TheStatementThatBuildsTheIndexesStillMaintainsThem` (delete) |
| 8 | `lookupRows`: delete `t.dropIndexes()` from the `ErrRowNotFound` branch | `TestLookup_AStaleEntryFallsBack...` |
| 9 | `indexes`: ignore `noIndexes` | `TestLookup_IsOffWhenIndexesAre` or the differential test |
| 10 | `Update`: decide `trackKeys` before calling `lookupRows` | `TestLookup_TheStatementThatBuildsTheIndexesStillMaintainsThem` (update) |

Expected: all ten fail. A mutant that survives means a missing test: add one that fails for it, then continue.

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

// TestMeasurePointStatements is a one-off measurement, not part of the suite:
// run it with `go test ./internal/engine/ -run MeasurePoint -v -count=1`.
func TestMeasurePointStatements(t *testing.T) {
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
	measure := func(label string, n int, run func(i int) error) {
		start := time.Now()
		for i := 0; i < n; i++ {
			require.NoError(t, run(i))
		}
		t.Logf("%-34s %9.3f ms/op  (%d ops)", label, float64(time.Since(start))/float64(n)/1e6, n)
	}
	for _, mode := range []struct {
		name      string
		noIndexes bool
		ops       int
	}{{"scan   (indexes off)", true, 20}, {"lookup (indexes on)", false, 200}} {
		table := build(mode.noIndexes)
		_, err := table.Select([]string{"id"}, eqCmp("id", numTok("0"))) // builds the indexes when they are on
		require.NoError(t, err)
		measure(mode.name+" SELECT by id", mode.ops, func(i int) error {
			_, err := table.Select([]string{"*"}, eqCmp("id", numTok(fmt.Sprint(i*37%rows))))
			return err
		})
		measure(mode.name+" UPDATE by id", mode.ops, func(i int) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("changed")}}, eqCmp("id", numTok(fmt.Sprint(i*41%rows))))
		})
		measure(mode.name+" DELETE by id", mode.ops, func(i int) error {
			return table.Delete(eqCmp("id", numTok(fmt.Sprint(i*43%rows))))
		})
		measure(mode.name+" INSERT (key check)", mode.ops, func(i int) error {
			return table.InsertValues(nil, [][]parser.Token{tokRow(numTok(fmt.Sprint(rows+i)), strTok(fmt.Sprintf("new%d@example.com", i)), strTok("n"))})
		})
	}
}
```

Run: `go test ./internal/engine/ -run MeasurePoint -v -count=1 2>&1 | grep -E 'ms/op|ok|FAIL'`
Expected: eight `ms/op` lines, scan against lookup for SELECT, UPDATE, DELETE and INSERT on a 100,000-row table (on the author's machine: a point SELECT 12.6 ms against 0.004 ms; point UPDATE, DELETE and INSERT about 17 ms against about 4 ms, which is the fsync). Copy them into the PR description, then delete the file (`rm internal/engine/measure_test.go`) and confirm `git status --short` does not list it.

- [ ] **Step 6: Run everything**

Run: `git status --short` (expected: only `CLAUDE.md` and `internal/engine/indexdiff_test.go`), then `gofmt -l . ; go vet ./... && go test -race ./...`
Expected: every package passes and `gofmt` and `go vet` print nothing.

- [ ] **Step 7: Commit**

```bash
git add CLAUDE.md internal/engine/indexdiff_test.go
git commit -m "test(engine): compare lookups with scans over more WHERE shapes" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
