package engine

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// An UPDATE appends the new version of a row to the file and tombstones the
// old one, so the file holds the same rows as Rows but not necessarily in the
// same order.
func assertMirroredAsSet(t *testing.T, table *SqlTable) {
	t.Helper()
	assert.ElementsMatch(t, table.Rows, fileRows(t, table), "the file must hold exactly the table's rows, in any order")
}

func setTo(column string, value parser.Token) parser.Assignment {
	return parser.Assignment{Column: column, Value: value}
}

func TestTable_Update_MirrorsToFile(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.EQ, numTok("2")),
	))

	assert.Equal(t, []any{int64(2), "zed"}, table.Rows[1])
	assertMirroredAsSet(t, table)
	assert.Len(t, fileRows(t, table), 4, "the old version is tombstoned, not left beside the new one")
}

func TestTable_Update_WithoutWhereUpdatesEveryRow(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("x"))}, nil))

	for _, row := range table.Rows {
		assert.Equal(t, "x", row[1])
	}
	assertMirroredAsSet(t, table)
	assert.Len(t, fileRows(t, table), 4)
}

func TestTable_Update_MultipleAssignmentsMirror(t *testing.T) {
	table := usersTable(t, ageColumn)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob"), numTok("30")),
		tokRow(numTok("2"), strTok("sam"), numTok("40")),
	}))

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("z")), setTo("age", numTok("99"))},
		cmp(identTok("id"), parser.EQ, numTok("1")),
	))

	assert.Equal(t, []any{int64(1), "z", int64(99)}, table.Rows[0])
	assertMirroredAsSet(t, table)
}

func TestTable_Update_NoMatchLeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.EQ, numTok("99")),
	))

	assertMirrored(t, table) // nothing moved, so even the order is unchanged
}

func TestTable_Update_AcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	// Ids 0..699 sit on the first pages; their new versions go at the end.
	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("m"))},
		cmp(identTok("id"), parser.LT, numTok("700")),
	))

	assert.Len(t, table.Rows, n)
	assertMirroredAsSet(t, table)
	assert.Len(t, fileRows(t, table), n)
}

func TestTable_Update_FailedUpdateLeavesFileAndRowsUntouched(t *testing.T) {
	tests := []struct {
		name        string
		assignments []parser.Assignment
		where       parser.Expression
		wantErr     string
	}{
		{"unknown column in SET", []parser.Assignment{setTo("nope", strTok("x"))}, nil, `unknown column "nope"`},
		{"literal of the wrong kind in SET", []parser.Assignment{setTo("id", strTok("abc"))}, nil, `column "id": expected a numeric value, got abc`},
		{"unknown column in WHERE", []parser.Assignment{setTo("email", strTok("x@x.com"))}, cmp(identTok("nope"), parser.EQ, numTok("1")), `unknown column "nope"`},
		{"UNIQUE violation", []parser.Assignment{setTo("email", strTok("b@x.com"))}, cmp(identTok("id"), parser.EQ, numTok("1")), `duplicate entry b@x.com for column "email"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := uniqueUsersTable(t)
			require.NoError(t, table.InsertValues(nil, [][]parser.Token{
				tokRow(numTok("1"), strTok("a@x.com")),
				tokRow(numTok("2"), strTok("b@x.com")),
			}))
			wantRows := append([][]any(nil), table.Rows...)

			err := table.Update(tc.assignments, tc.where)

			assert.EqualError(t, err, tc.wantErr)
			assert.Equal(t, wantRows, table.Rows)
			assertMirrored(t, table) // untouched, so even the order is unchanged
		})
	}
}

func TestTable_Update_RowThatGrowsPastAPageIsRejectedAtomically(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	// Row 1 is tiny. Row 2 encodes to exactly storage.MaxRowSize: a 12-byte
	// offset table, the 8-byte id, the 1-byte name and the bio.
	bio := strings.Repeat("x", storage.MaxRowSize-12-8-1)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("a"), strTok("short")),
		tokRow(numTok("2"), strTok("b"), strTok(bio)),
	}))
	wantRows := append([][]any(nil), table.Rows...)

	// A longer name pushes row 2 over the limit; row 1 would still fit.
	err := table.Update([]parser.Assignment{setTo("name", strTok("a-much-longer-name"))}, nil)

	assert.ErrorContains(t, err, "row too large")
	assert.Equal(t, wantRows, table.Rows)
	assertMirrored(t, table)
}

func TestTable_Update_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	wantRows := append([][]any(nil), table.Rows...)
	require.NoError(t, table.Close())

	err := table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, nil)

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Equal(t, wantRows, table.Rows, "Rows is only committed after the file step succeeds")
}

func TestTable_Update_IsOnDiskOnceItReturns(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.LT, numTok("3")),
	))

	// A second handle on the same file sees what is really on disk.
	other, err := storage.OpenFile(table.path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	onDisk := make([][]any, 0)
	for row, err := range other.Scan() {
		require.NoError(t, err)
		decoded, err := DecodeRow(table.Columns, row.Bytes)
		require.NoError(t, err)
		onDisk = append(onDisk, decoded)
	}
	assert.ElementsMatch(t, table.Rows, onDisk)
}

func TestTable_Update_ThenDeleteThenInsertStayMirrored(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.LT, numTok("3")),
	))

	// DELETE decides its file matches from the file's own rows, so it must
	// find the new versions the UPDATE appended.
	require.NoError(t, table.Delete(cmp(identTok("name"), parser.EQ, strTok("zed"))))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("5"), strTok("kay"))}))

	assert.Len(t, table.Rows, 3) // ids 3, 4 and 5
	assertMirroredAsSet(t, table)
}

func TestTable_Update_UnassignedNullColumnsStayNull(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// The row has no note, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))

	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, nil))

	assert.Equal(t, []any{int64(1), "zed", nil}, table.Rows[0])
	assertMirroredAsSet(t, table)
	assert.Equal(t, []any{int64(1), "zed", nil}, fileRows(t, table)[0])
}

// Rows and the file can drift until every statement is migrated. UPDATE decides
// the file's matches from the file's own rows, so a row that matches in only one
// of them is simply not updated in the other, and that is not an error.
func TestTable_Update_ToleratesAFileThatDriftedFromRows(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	table.Rows[1][1] = "zed" // Rows says id 2 is "zed"; the file still says "sam"

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("kim"))},
		cmp(identTok("name"), parser.EQ, strTok("zed")),
	))

	assert.Equal(t, []any{int64(2), "kim"}, table.Rows[1], "Rows updated the row it knows as zed")
	assert.Len(t, fileRows(t, table), 4, "the file has no zed, so it updated nothing and raised no error")
}
