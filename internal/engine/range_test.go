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
