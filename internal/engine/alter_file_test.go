package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

var ageColumn = parser.ColumnDefinition{
	Name:        "age",
	DataType:    parser.IntDataType{},
	Constraints: []parser.Constraint{parser.DefaultConstraint{DefaultValue: numTok("18")}},
}

// filesInTableDir lists the file names next to the table's file.
func filesInTableDir(t *testing.T, table *SqlTable) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(table.path))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func twoUsers(t *testing.T, table *SqlTable) {
	t.Helper()
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
	}))
}

func TestTable_AddColumn_RewritesTheFileInTheNewSchema(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	oldPath := table.path

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	assertTableRows(t, table,
		[]any{int64(1), "bob", int64(18)},
		[]any{int64(2), "sam", int64(18)},
	)
	assert.NotEqual(t, oldPath, table.path, "the rewrite goes to a new file")
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table), "and the old file is gone")
}

func TestTable_AddColumn_NullableColumnBackfillsNull(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}},
	}))

	assertTableRows(t, table,
		[]any{int64(1), "bob", nil},
		[]any{int64(2), "sam", nil},
	)
}

func TestTable_DropColumn_RewritesTheFileInTheNewSchema(t *testing.T) {
	table := usersTable(t, ageColumn)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob"), numTok("30")),
		tokRow(numTok("2"), strTok("sam"), numTok("40")),
	}))
	oldPath := table.path

	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	assertTableRows(t, table, []any{int64(1), int64(30)}, []any{int64(2), int64(40)})
	assert.NotEqual(t, oldPath, table.path)
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table))
}

func TestTable_RenameColumn_LeavesTheFileAlone(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	path := table.path

	require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "full_name"}))

	assert.Equal(t, path, table.path, "rows are positional, so a rename needs no rewrite")
	assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table))
	assertTableRows(t, table, []any{int64(1), "bob"}, []any{int64(2), "sam"})
}

func TestTable_Alter_RejectedAlterLeavesFileSchemaAndRowsUntouched(t *testing.T) {
	tests := []struct {
		name   string
		action parser.AlterAction
	}{
		{"duplicate column name", parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "id", DataType: parser.IntDataType{}}}},
		{"NOT NULL without a default", parser.AddColumnAction{Column: parser.ColumnDefinition{
			Name: "req", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.NotNullConstraint{}},
		}}},
		{"UNIQUE default backfilled onto two rows", parser.AddColumnAction{Column: parser.ColumnDefinition{
			Name: "code", DataType: parser.IntDataType{},
			Constraints: []parser.Constraint{parser.UniqueConstraint{}, parser.DefaultConstraint{DefaultValue: numTok("1")}},
		}}},
		{"drop an unknown column", parser.DropColumnAction{Column: "nope"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := usersTable(t)
			twoUsers(t, table)
			path := table.path
			wantColumns := append([]parser.ColumnDefinition(nil), table.Columns...)
			wantRows := fileRows(t, table)

			err := table.AlterColumns(tc.action)

			require.Error(t, err)
			assert.Equal(t, path, table.path)
			assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table), "no stray new file")
			assert.Equal(t, wantColumns, table.Columns)
			assert.Equal(t, wantRows, fileRows(t, table))
		})
	}
}

func TestTable_DropColumn_TheOnlyColumnIsRejectedWithoutTouchingTheFile(t *testing.T) {
	table, err := newTempTable(t, "solo", idColumn)
	require.NoError(t, err)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"))}))
	path := table.path

	err = table.AlterColumns(parser.DropColumnAction{Column: "id"})

	assert.EqualError(t, err, "cannot drop the only column")
	assert.Equal(t, path, table.path)
	assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table))
	assertTableRows(t, table, []any{int64(1)})
}

func TestTable_AddColumn_ThatMakesARowTooLargeIsRejected(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "bio", DataType: parser.TextDataType{}})
	// This row encodes to exactly storage.MaxRowSize bytes: a 12-byte offset
	// table, the 8-byte id, "bob" and the bio. Any new column pushes it over.
	bio := strings.Repeat("x", storage.MaxRowSize-12-8-len("bob"))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"), strTok(bio))}))
	path := table.path

	err := table.AlterColumns(parser.AddColumnAction{Column: ageColumn})

	assert.ErrorContains(t, err, "row too large")
	assert.Len(t, table.Columns, 3, "the schema is unchanged")
	assert.Equal(t, path, table.path)
	assert.Equal(t, []string{filepath.Base(path)}, filesInTableDir(t, table), "no stray new file")
	assert.Len(t, fileRows(t, table), 1, "the row is still in the file, in the old layout")
}

func TestTable_Alter_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.Close())

	err := table.AlterColumns(parser.AddColumnAction{Column: ageColumn})

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Len(t, table.Columns, 2, "the schema is unchanged")
}

func TestTable_Alter_OnAnEmptyTable(t *testing.T) {
	table := usersTable(t)
	oldPath := table.path

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))
	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	assertTableRows(t, table)
	assert.NotEqual(t, oldPath, table.path)
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table))
}

func TestTable_Alter_ManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	onDisk := fileRows(t, table)
	require.Len(t, onDisk, n)
	assert.Equal(t, []any{int64(0), name, int64(18)}, onDisk[0])
	info, err := os.Stat(table.path)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(2*16*1024), "the rewritten rows span several pages")
	assert.Zero(t, info.Size()%(16*1024))
	assert.Equal(t, []string{filepath.Base(table.path)}, filesInTableDir(t, table))
}

func TestTable_Alter_InsertAfterAlterMirrorsInTheNewLayout(t *testing.T) {
	table := usersTable(t)
	twoUsers(t, table)
	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("amy"), numTok("50"))}))

	assertTableRows(t, table,
		[]any{int64(1), "bob", int64(18)},
		[]any{int64(2), "sam", int64(18)},
		[]any{int64(3), "amy", int64(50)},
	)
}
