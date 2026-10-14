package engine

import (
	"fmt"
	"strings"
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
		{"reversed: 3 <= id is id >= 3", cmp(numTok("3"), parser.LTE, identTok("id")), []rangePlan{{colID, id(3), nil}}},
		{"reversed: 3 > id is id < 3", cmp(numTok("3"), parser.GT, identTok("id")), []rangePlan{{colID, nil, id(3)}}},
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
	mirrored := map[parser.TokenType]parser.TokenType{parser.GT: parser.LT, parser.GTE: parser.LTE, parser.LT: parser.GT, parser.LTE: parser.GTE}
	for _, lit := range []int{0, 1, 7, 20, 21} {
		for _, o := range ops {
			// Written both ways round: "id > 7" and "7 < id" say the same.
			forms := map[string]parser.Expression{
				fmt.Sprintf("id %s %d", o.name, lit): cmp(identTok("id"), o.op, numTok(fmt.Sprint(lit))),
				fmt.Sprintf("%d %s id", lit, map[parser.TokenType]string{parser.GT: "<", parser.GTE: "<=", parser.LT: ">", parser.LTE: ">="}[o.op]): cmp(numTok(fmt.Sprint(lit)), mirrored[o.op], identTok("id")),
			}
			for name, where := range forms {
				t.Run(name, func(t *testing.T) {
					want := [][]string{}
					for id := 1; id <= 20; id++ {
						if o.holds(id, lit) {
							want = append(want, []string{fmt.Sprint(id)})
						}
					}
					cf.scans.Store(0)
					cf.gets.Store(0)

					got := idsOf(t, table, where)

					assert.Equal(t, want, got)
					assert.EqualValues(t, 0, cf.scans.Load(), "no scan")
					assert.EqualValues(t, len(want), cf.gets.Load(), "one get per row in the range")
				})
			}
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
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("21"), strTok(""), strTok("n"))}))
	cf := countOn(table)
	emails := []string{}
	for id := 1; id <= 20; id++ {
		emails = append(emails, fmt.Sprintf("e%03d", id+100))
	}
	emails = append(emails, "") // id 21 has the empty string for its email
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
				cf.gets.Store(0)

				got := idsOf(t, table, cmp(identTok("email"), o.op, strTok(lit)))

				assert.Equal(t, want, got)
				assert.EqualValues(t, 0, cf.scans.Load(), "no scan")
				assert.EqualValues(t, len(want), cf.gets.Load(), "one get per row in the range, so no extra rows were read")
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

// The same claim as above across many pages: rows of a few KB put about five on
// a page, so a range spans several, and the rows that were moved to the end sit
// on the last pages.
func TestRangeLookup_FileOrderAcrossPages(t *testing.T) {
	build := func() *SqlTable {
		table := codesTable(t)
		rows := make([][]parser.Token, 30)
		padding := strings.Repeat("p", 3000)
		for i := range rows {
			rows[i] = tokRow(numTok(fmt.Sprint(i)), strTok(fmt.Sprintf("c%02d", i)), strTok(padding))
		}
		require.NoError(t, table.InsertValues(nil, rows))
		_, err := table.indexes()
		require.NoError(t, err)
		return table
	}
	indexed, plain := build(), build()
	plain.noIndexes = true
	for _, table := range []*SqlTable{indexed, plain} {
		for _, id := range []string{"4", "11", "17"} {
			require.NoError(t, table.Update([]parser.Assignment{{Column: "note", Value: strTok("moved")}}, eqCmp("id", numTok(id))))
		}
	}
	cf := countOn(indexed)
	where := andExpr(cmp(identTok("id"), parser.GTE, numTok("0")), cmp(identTok("id"), parser.LT, numTok("30")))

	got := idsOf(t, indexed, where)

	assert.Equal(t, idsOf(t, plain, where), got, "the scan's order")
	assert.Len(t, got, 30)
	assert.Equal(t, [][]string{{"4"}, {"11"}, {"17"}}, got[27:], "the moved rows are last")
	assert.EqualValues(t, 0, cf.scans.Load(), "and it was a lookup")

	for _, table := range []*SqlTable{indexed, plain} {
		require.NoError(t, table.Update([]parser.Assignment{{Column: "note", Value: strTok("again")}}, where))
	}
	assert.Equal(t, fileRows(t, plain), fileRows(t, indexed), "an UPDATE over the range writes the same file")
}

// "email < ”" can match nothing, and a second upper bound must not widen it
// back: the whole WHERE would hide the extra rows, so only the reads show it.
func TestRangeLookup_AnEmptyUpperBoundStaysEmpty(t *testing.T) {
	table := intKeys(t, sequence(1, 20)...)
	cf := countOn(table)

	got := idsOf(t, table, andExpr(
		cmp(identTok("email"), parser.LT, strTok("")),
		cmp(identTok("email"), parser.LT, strTok("e105"))))

	assert.Equal(t, [][]string{}, got)
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 0, cf.gets.Load(), "no row is in the range")
}
