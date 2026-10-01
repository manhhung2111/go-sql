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

	// Exactly four rows: the old version is tombstoned, not left beside the new one.
	assertTableRowsAnyOrder(t, table,
		[]any{int64(1), "bob"},
		[]any{int64(2), "zed"},
		[]any{int64(3), "amy"},
		[]any{int64(4), "lee"},
	)
}

func TestTable_Update_WithoutWhereUpdatesEveryRow(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("x"))}, nil))

	assertTableRowsAnyOrder(t, table,
		[]any{int64(1), "x"},
		[]any{int64(2), "x"},
		[]any{int64(3), "x"},
		[]any{int64(4), "x"},
	)
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

	assertTableRowsAnyOrder(t, table,
		[]any{int64(1), "z", int64(99)},
		[]any{int64(2), "sam", int64(40)},
	)
}

func TestTable_Update_NoMatchLeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Update(
		[]parser.Assignment{setTo("name", strTok("zed"))},
		cmp(identTok("id"), parser.EQ, numTok("99")),
	))

	// Nothing moved, so even the order is unchanged.
	assertTableRows(t, table,
		[]any{int64(1), "bob"},
		[]any{int64(2), "sam"},
		[]any{int64(3), "amy"},
		[]any{int64(4), "lee"},
	)
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

	onDisk := fileRows(t, table)
	require.Len(t, onDisk, n)
	updated := 0
	for _, row := range onDisk {
		if row[1] == "m" {
			updated++
		}
	}
	assert.Equal(t, 700, updated)
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
			wantRows := fileRows(t, table)

			err := table.Update(tc.assignments, tc.where)

			assert.EqualError(t, err, tc.wantErr)
			assert.Equal(t, wantRows, fileRows(t, table)) // untouched, so even the order is unchanged
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
	wantRows := fileRows(t, table)

	// A longer name pushes row 2 over the limit; row 1 would still fit.
	err := table.Update([]parser.Assignment{setTo("name", strTok("a-much-longer-name"))}, nil)

	assert.ErrorContains(t, err, "row too large")
	assert.Equal(t, wantRows, fileRows(t, table))
}

func TestTable_Update_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Close())

	err := table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, nil)

	assert.EqualError(t, err, `table "users" is closed`)
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
	assert.ElementsMatch(t, fileRows(t, table), onDisk)
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

	assertTableRowsAnyOrder(t, table,
		[]any{int64(3), "amy"},
		[]any{int64(4), "lee"},
		[]any{int64(5), "kay"},
	)
}

func TestTable_Update_UnassignedNullColumnsStayNull(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// The row has no note, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))

	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, nil))

	assertTableRows(t, table, []any{int64(1), "zed", nil})
}
