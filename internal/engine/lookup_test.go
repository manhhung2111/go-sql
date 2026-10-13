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
