package engine

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// manyUniqueUsers fills a uniqueUsersTable (id PRIMARY KEY, email UNIQUE) with
// n rows, enough to span several 16KiB pages.
func manyUniqueUsers(t *testing.T, n int) *SqlTable {
	t.Helper()
	table := uniqueUsersTable(t)
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok("user"+strconv.Itoa(i)+"@example.com"))
	}
	require.NoError(t, table.InsertValues(nil, rows))
	return table
}

func TestTable_InsertValues_DuplicateOnALatePage(t *testing.T) {
	table := manyUniqueUsers(t, 1500)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1499"), strTok("fresh@example.com"))})
	assert.EqualError(t, err, `duplicate entry 1499 for column "id"`)

	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1500"), strTok("user7@example.com"))})
	assert.EqualError(t, err, `duplicate entry user7@example.com for column "email"`)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1500"), strTok("fresh@example.com"))}))
	assert.Len(t, fileRows(t, table), 1501)
}

// An earlier row's duplicate key has always been reported before a later row's
// error, because rows were checked one at a time.
func TestTable_InsertValues_DuplicateBeatsALaterRowError(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("z@x.com")),   // duplicates id 1
		tokRow(strTok("two"), strTok("y@x.com")), // a string where an INT belongs
	})

	assert.EqualError(t, err, `duplicate entry 1 for column "id"`)
}

// ...and an earlier row's error has always been reported before a later row's
// duplicate key.
func TestTable_InsertValues_CoercionErrorBeatsALaterDuplicate(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(strTok("two"), strTok("y@x.com")), // a string where an INT belongs
		tokRow(numTok("1"), strTok("z@x.com")),   // duplicates id 1
	})

	assert.EqualError(t, err, `column "id": expected a numeric value, got two`)
}

func TestTable_Update_UniqueConflictReportsTheFirstConflictingColumn(t *testing.T) {
	// Row 1 would collide with row 3 on id and with row 2 on email.
	setup := func(t *testing.T) *SqlTable {
		table := uniqueUsersTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{
			tokRow(numTok("1"), strTok("a@x.com")),
			tokRow(numTok("2"), strTok("b@x.com")),
			tokRow(numTok("3"), strTok("c@x.com")),
		}))
		return table
	}
	only1 := cmp(identTok("id"), parser.EQ, numTok("1"))

	err := setup(t).Update([]parser.Assignment{setTo("email", strTok("b@x.com")), setTo("id", numTok("3"))}, only1)
	assert.EqualError(t, err, `duplicate entry b@x.com for column "email"`, "email is assigned first")

	err = setup(t).Update([]parser.Assignment{setTo("id", numTok("3")), setTo("email", strTok("b@x.com"))}, only1)
	assert.EqualError(t, err, `duplicate entry 3 for column "id"`, "id is assigned first")
}

// Two matched rows would both receive the assigned value; the row-by-row check
// reported it on the first unique column in assignment order.
func TestTable_Update_SeveralMatchesReportTheFirstUniqueColumn(t *testing.T) {
	setup := func(t *testing.T) *SqlTable {
		table := uniqueUsersTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{
			tokRow(numTok("1"), strTok("a@x.com")),
			tokRow(numTok("2"), strTok("b@x.com")),
		}))
		return table
	}

	err := setup(t).Update([]parser.Assignment{setTo("id", numTok("5")), setTo("email", strTok("q@x.com"))}, nil)
	assert.EqualError(t, err, `duplicate entry 5 for column "id"`)

	err = setup(t).Update([]parser.Assignment{setTo("email", strTok("q@x.com")), setTo("id", numTok("5"))}, nil)
	assert.EqualError(t, err, `duplicate entry q@x.com for column "email"`)
}

func TestTable_Update_UniquenessAcrossManyPages(t *testing.T) {
	table := manyUniqueUsers(t, 1500)
	row0 := cmp(identTok("id"), parser.EQ, numTok("0"))

	// The only row holding this email sits on the last page.
	err := table.Update([]parser.Assignment{setTo("email", strTok("user1499@example.com"))}, row0)
	assert.EqualError(t, err, `duplicate entry user1499@example.com for column "email"`)

	require.NoError(t, table.Update([]parser.Assignment{setTo("email", strTok("fresh@example.com"))}, row0))
	assert.Len(t, fileRows(t, table), 1500)
}

// One value backfilled onto fewer than two rows holds no key twice, so a UNIQUE
// column with a default is accepted on an empty table and on a one-row table;
// two rows are rejected by TestTable_AlterColumns_AddColumn_UniqueWithDefault
// RejectedOnMultipleRows.
func TestTable_AddColumn_UniqueDefaultIsAcceptedOnFewerThanTwoRows(t *testing.T) {
	region := func() parser.ColumnDefinition {
		return parser.ColumnDefinition{
			Name:     "region",
			DataType: parser.VarCharDataType{Size: 10},
			Constraints: []parser.Constraint{
				parser.UniqueConstraint{},
				parser.DefaultConstraint{DefaultValue: strTok("us")},
			},
		}
	}

	empty := usersTable(t)
	require.NoError(t, empty.AlterColumns(parser.AddColumnAction{Column: region()}))
	assert.Len(t, empty.Columns, 3)

	one := usersTable(t)
	require.NoError(t, one.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))
	require.NoError(t, one.AlterColumns(parser.AddColumnAction{Column: region()}))
	assertTableRows(t, one, []any{int64(1), "bob", "us"})
}
