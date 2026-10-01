package engine

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// stringRows renders rows the way Select does, for comparing a result with
// what Rows holds.
func stringRows(rows [][]any) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = make([]string, len(row))
		for j, v := range row {
			out[i][j] = stringifyValue(v)
		}
	}
	return out
}

func TestTable_Select_ReadsTheFileNotRows(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	table.Rows = nil // the file alone must answer

	resp, err := table.Select([]string{"*"}, nil)

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"1", "bob"}, {"2", "sam"}, {"3", "amy"}, {"4", "lee"}}, resp.Rows)
}

func TestTable_Select_SeesEveryMigratedStatement(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Update([]parser.Assignment{setTo("name", strTok("zed"))}, cmp(identTok("id"), parser.EQ, numTok("2"))))
	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("3"))))
	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: ageColumn}))
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("5"), strTok("kay"), numTok("50"))}))
	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))

	resp, err := table.Select([]string{"*"}, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{"id", "age"}, resp.Columns)
	assert.ElementsMatch(t, [][]string{{"1", "18"}, {"2", "18"}, {"4", "18"}, {"5", "50"}}, resp.Rows)
	assert.ElementsMatch(t, stringRows(table.Rows), resp.Rows, "the file answers exactly what Rows holds")
}

func TestTable_Select_AcrossManyPages(t *testing.T) {
	table := usersTable(t)
	name := strings.Repeat("n", 40)
	const n = 1500 // about 56 bytes a row, so several 16KiB pages
	rows := make([][]parser.Token, n)
	for i := range rows {
		rows[i] = tokRow(numTok(strconv.Itoa(i)), strTok(name))
	}
	require.NoError(t, table.InsertValues(nil, rows))

	all, err := table.Select([]string{"id"}, nil)
	require.NoError(t, err)
	late, err := table.Select([]string{"id"}, cmp(identTok("id"), parser.GTE, numTok("1400")))
	require.NoError(t, err)

	assert.Len(t, all.Rows, n)
	assert.Len(t, late.Rows, 100, "a WHERE matching only rows on the last pages")
}

func TestTable_Select_NullAndEmptyStringRenderDistinctly(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "note", DataType: parser.TextDataType{}})
	// The name is the empty string and the note was omitted, so it is NULL.
	require.NoError(t, table.InsertValues([]string{"id", "name"}, [][]parser.Token{tokRow(numTok("1"), strTok(""))}))

	resp, err := table.Select([]string{"*"}, nil)

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"1", "", "NULL"}}, resp.Rows)
}

func TestTable_Select_OnAClosedTableFails(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)
	require.NoError(t, table.Close())

	_, err := table.Select([]string{"*"}, nil)
	assert.EqualError(t, err, `table "users" is closed`)

	// A projection that names no column is a schema error and is reported first.
	_, err = table.Select([]string{"nope"}, nil)
	assert.EqualError(t, err, `unknown column "nope"`)
}

func TestTable_Select_ConcurrentSelects(t *testing.T) {
	table := usersTable(t)
	fourUsers(t, table)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				resp, err := table.Select([]string{"*"}, nil)
				assert.NoError(t, err)
				assert.Len(t, resp.Rows, 4)
			}
		}()
	}
	wg.Wait()
}

func TestTable_Select_ConcurrentWithWriters(t *testing.T) {
	table := usersTable(t)
	const writers, perWriter = 4, 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := strconv.Itoa(w*perWriter + i)
				assert.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok(id), strTok("n"))}))
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := table.Select([]string{"*"}, nil)
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	resp, err := table.Select([]string{"*"}, nil)
	require.NoError(t, err)
	assert.Len(t, resp.Rows, writers*perWriter)
	assertMirrored(t, table)
}
