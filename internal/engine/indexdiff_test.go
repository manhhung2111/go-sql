package engine

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func randEmail(rng *rand.Rand) string { return fmt.Sprintf("u%d@x.com", rng.Intn(25)) }
func randName(rng *rand.Rand) string  { return fmt.Sprintf("n%d", rng.Intn(4)) }

// randomWhere draws a WHERE from a mix of shapes: none, equalities on the key
// columns (either operand order), ranges, AND/OR, and one that cannot be
// coerced. The expression is built once so both tables see the same one.
func randomWhere(rng *rand.Rand) parser.Expression {
	id := func() parser.Token { return numTok(fmt.Sprint(rng.Intn(25))) }
	switch rng.Intn(20) {
	case 0:
		return nil
	case 1:
		return cmp(identTok("id"), parser.EQ, id())
	case 2:
		return cmp(identTok("id"), parser.GTE, id())
	case 3:
		return cmp(identTok("email"), parser.EQ, strTok(randEmail(rng)))
	case 4:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.AND, cmp(identTok("name"), parser.EQ, strTok(randName(rng))))
	case 5:
		return binaryExpr(cmp(identTok("id"), parser.EQ, id()), parser.OR, cmp(identTok("id"), parser.EQ, id()))
	case 6:
		return cmp(id(), parser.EQ, identTok("id"))
	case 7:
		return cmp(identTok("name"), parser.EQ, strTok(randName(rng)))
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
		ops := []parser.TokenType{parser.LT, parser.LTE, parser.GT, parser.GTE}
		return cmp(id(), ops[rng.Intn(len(ops))], identTok("id"))
	default:
		return cmp(identTok("id"), parser.EQ, strTok("oops"))
	}
}

// randomStatement draws one statement and returns it as a function to run on a
// table, so the very same statement can run on two tables.
func randomStatement(rng *rand.Rand) func(*SqlTable) (Response, error) {
	switch rng.Intn(10) {
	case 0, 1, 2, 3: // INSERT
		var columns []string
		rows := make([][]parser.Token, 1+rng.Intn(3))
		withEmail := rng.Intn(5) > 0
		for i := range rows {
			if withEmail {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(25))), strTok(randEmail(rng)), strTok(randName(rng)))
			} else {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(25))), strTok(randName(rng)))
			}
		}
		if !withEmail {
			columns = []string{"id", "name"}
		}
		return func(table *SqlTable) (Response, error) { return Response{}, table.InsertValues(columns, rows) }
	case 4, 5: // UPDATE
		var assignment parser.Assignment
		switch rng.Intn(4) {
		case 0:
			assignment = parser.Assignment{Column: "email", Value: strTok(randEmail(rng))}
		case 1:
			assignment = parser.Assignment{Column: "id", Value: numTok(fmt.Sprint(rng.Intn(25)))}
		default:
			assignment = parser.Assignment{Column: "name", Value: strTok(randName(rng))}
		}
		where := randomWhere(rng)
		return func(table *SqlTable) (Response, error) {
			return Response{}, table.Update([]parser.Assignment{assignment}, where)
		}
	case 6, 7: // DELETE
		where := randomWhere(rng)
		return func(table *SqlTable) (Response, error) { return Response{}, table.Delete(where) }
	default: // SELECT
		where := randomWhere(rng)
		return func(table *SqlTable) (Response, error) { return table.Select([]string{"*"}, where) }
	}
}

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

// With the factor so high that every non-empty range is too expensive, all
// ranges scan: the answers must not change. (The rest of the package runs with
// every range a lookup; see TestMain.)
func TestIndexedTableMatchesWhenEveryRangeScans(t *testing.T) {
	withCostFactor(t, 1_000_000)
	for seed := int64(7); seed <= 8; seed++ {
		compareWithUnindexed(t, seed)
	}
}
