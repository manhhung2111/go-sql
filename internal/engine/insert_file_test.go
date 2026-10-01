package engine

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

func TestTable_InsertValues_MirrorsRowsToFile(t *testing.T) {
	table := usersTable(t)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
	}))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("amy"))}))

	assertTableRows(t, table,
		[]any{int64(1), "bob"},
		[]any{int64(2), "sam"},
		[]any{int64(3), "amy"},
	)
}

func TestTable_InsertValues_MirrorsNullAndEmptyStringDistinctly(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// Row 1: name is the empty string and note was omitted, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok(""))}))
	// Row 2: note is the empty string.
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("sam"), strTok(""))}))

	onDisk := fileRows(t, table)
	require.Len(t, onDisk, 2)
	assert.Equal(t, []any{int64(1), "", nil}, onDisk[0])
	assert.Equal(t, []any{int64(2), "sam", ""}, onDisk[1])
}

func TestTable_InsertValues_MirrorsAcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	// Each row encodes to about 56 bytes, so 1500 rows span several 16KiB pages.
	const n = 1500
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}

	require.NoError(t, table.InsertValues(nil, rows))

	onDisk := fileRows(t, table)
	require.Len(t, onDisk, n)
	assert.Equal(t, []any{int64(0), name}, onDisk[0])
	assert.Equal(t, []any{int64(n - 1), name}, onDisk[n-1])
	info, err := os.Stat(table.path)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(2*16*1024), "the rows span several pages")
	assert.Zero(t, info.Size()%(16*1024), "the file is a whole number of pages")
}

// This guards the order of the phases: validation must finish before the
// first row is written. It passes even before the mirror exists, because
// nothing was written at all then; it fails if a later change writes rows
// as it validates them.
func TestTable_InsertValues_RejectedBatchWritesNothingToFile(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),   // valid
		tokRow(strTok("two"), strTok("sam")), // a string where an INT belongs
	})

	require.Error(t, err)
	assert.Empty(t, fileRows(t, table))
}

func TestTable_InsertValues_OversizedRowIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	huge := strings.Repeat("x", storage.MaxRowSize+1)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob"), strTok("short")), // fits
		tokRow(numTok("2"), strTok("sam"), strTok(huge)),    // fits on no page
	})

	assert.ErrorContains(t, err, "row too large")
	assert.Empty(t, fileRows(t, table), "the batch is all-or-nothing: the row that did fit was not written either")
}

func TestTable_InsertValues_LargestRowThatFitsIsStored(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	// The encoded row is a 12-byte offset table, the 8-byte id, "bob" and the bio.
	bio := strings.Repeat("x", storage.MaxRowSize-12-8-len("bob"))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"), strTok(bio))}))

	onDisk := fileRows(t, table)
	require.Len(t, onDisk, 1)
	assert.Equal(t, bio, onDisk[0][2])
}

func TestTable_InsertValues_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.Close())

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))})

	assert.EqualError(t, err, `table "users" is closed`)
}

func TestTable_InsertValues_TableWithNoColumns(t *testing.T) {
	table, err := newTempTable(t, "empty", nil)
	require.NoError(t, err)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(), tokRow()}))

	assertTableRows(t, table, []any{}, []any{})
}
