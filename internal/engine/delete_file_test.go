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

func fourUsers(t *testing.T, table *SqlTable) {
	t.Helper()
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
		tokRow(numTok("3"), strTok("amy")),
		tokRow(numTok("4"), strTok("lee")),
	}))
}

func TestTable_Delete_MirrorsToFile(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("2"))))

	assertTableRows(t, table,
		[]any{int64(1), "bob"},
		[]any{int64(3), "amy"},
		[]any{int64(4), "lee"},
	)
}

func TestTable_Delete_WithoutWhereEmptiesTheFileAndItStaysUsable(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(nil))

	assert.Empty(t, fileRows(t, table))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("zed"))}))
	assertTableRows(t, table, []any{int64(9), "zed"})
}

func TestTable_Delete_NoMatchLeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("99"))))

	assertTableRows(t, table,
		[]any{int64(1), "bob"},
		[]any{int64(2), "sam"},
		[]any{int64(3), "amy"},
		[]any{int64(4), "lee"},
	)
}

func TestTable_Delete_AcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	// Ids 0..699 sit on the first pages, which are not the cached tail page.
	require.NoError(t, table.Delete(cmp(identTok("id"), parser.LT, numTok("700"))))

	onDisk := fileRows(t, table)
	require.Len(t, onDisk, n-700)
	assert.Equal(t, int64(700), onDisk[0][0], "the survivors keep their order")
}

func TestTable_Delete_FailedWhereLeavesFileAndRowsUntouched(t *testing.T) {
	tests := []struct {
		name    string
		where   parser.Expression
		wantErr string
	}{
		{"unknown column", cmp(identTok("nope"), parser.EQ, numTok("1")), `unknown column "nope"`},
		{"literal of the wrong kind", cmp(identTok("id"), parser.EQ, strTok("abc")), "expected a numeric value, got abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := usersTable(t)
			fourUsers(t, table)
			wantRows := fileRows(t, table)

			err := table.Delete(tc.where)

			assert.EqualError(t, err, tc.wantErr)
			assert.Equal(t, wantRows, fileRows(t, table))
		})
	}
}

func TestTable_Delete_NullNeverMatches(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}})
	// Row 1 has no age, so it is NULL; row 2 has age 5.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("sam"), numTok("5"))}))

	require.NoError(t, table.Delete(cmp(identTok("age"), parser.NEQ, numTok("7"))))

	// NULL != 7 never matches, so only the row with age 5 goes.
	onDisk := fileRows(t, table)
	require.Len(t, onDisk, 1)
	assert.Equal(t, int64(1), onDisk[0][0])
}

func TestTable_Delete_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Close())

	err := table.Delete(nil)

	assert.EqualError(t, err, `table "users" is closed`)
}

func TestTable_Delete_ThenInsertMirrorsInOrder(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("2"))))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("5"), strTok("kay"))}))

	ids := []int64{}
	for _, row := range fileRows(t, table) {
		ids = append(ids, row[0].(int64))
	}
	assert.Equal(t, []int64{1, 3, 4, 5}, ids, "a deleted slot is never reused, so the new row goes at the end")
}

func TestTable_Delete_IsOnDiskOnceItReturns(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.LT, numTok("3"))))

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
	assert.Equal(t, fileRows(t, table), onDisk)
}
