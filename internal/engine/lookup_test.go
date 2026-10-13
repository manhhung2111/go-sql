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

func orExpr(l, r parser.Expression) parser.Expression { return binaryExpr(l, parser.OR, r) }

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
				"unknown column in another conjunct": andExpr(eqCmp("id", numTok(key)), eqCmp("nosuch", numTok("1"))),
				"a number against a string column":   andExpr(eqCmp("id", numTok(key)), eqCmp("name", numTok("3"))),
				"a string against the key":           eqCmp("id", strTok("x")),
				"an ordering against the wrong type": andExpr(eqCmp("id", numTok(key)), cmp(identTok("name"), parser.GT, numTok("1"))),
				"OR with an unknown column":          orExpr(eqCmp("id", numTok(key)), eqCmp("nosuch", numTok("1"))),
				"two columns":                        andExpr(eqCmp("id", numTok(key)), cmp(identTok("id"), parser.EQ, identTok("name"))),
				"a number out of range for the type": eqCmp("id", numTok("99999999999")),
				"a fine one, for comparison":         andExpr(eqCmp("id", numTok(key)), cmp(identTok("name"), parser.NEQ, strTok("zzz"))),
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
