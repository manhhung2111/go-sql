package engine

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func numTok(v string) parser.Token               { return parser.Token{Type: parser.NUMBER, Value: v} }
func strTok(v string) parser.Token               { return parser.Token{Type: parser.STRING, Value: v} }
func tokRow(toks ...parser.Token) []parser.Token { return toks }

func usersTable(t *testing.T, extraColumns ...parser.ColumnDefinition) *SqlTable {
	t.Helper()
	columns := append([]parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
	}, extraColumns...)
	table, err := NewTable("users", columns)
	require.NoError(t, err)
	return table
}

func TestTable_InsertValues_ExplicitOutOfOrderColumns(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues([]string{"name", "id"}, [][]parser.Token{
		tokRow(strTok("bob"), numTok("1")),
	})

	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	assert.Equal(t, numTok("1"), table.Rows[0][0])   // id, schema position 0
	assert.Equal(t, strTok("bob"), table.Rows[0][1]) // name, schema position 1
}

func TestTable_InsertValues_NoColumnList_FullRow(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
	})

	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	assert.Equal(t, []any{numTok("1"), strTok("bob")}, table.Rows[0])
}

func TestTable_InsertValues_NoColumnList_ArityMismatch(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1")),
	})

	assert.EqualError(t, err, "expected 2 values, got 1")
}

func TestTable_InsertValues_ExplicitColumnList_ArityMismatch(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues([]string{"id"}, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
	})

	assert.EqualError(t, err, "expected 1 values, got 2")
}

func TestTable_InsertValues_UnknownColumn(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues([]string{"nickname"}, [][]parser.Token{tokRow(strTok("bob"))})

	assert.EqualError(t, err, `unknown column "nickname"`)
}

func TestTable_InsertValues_DuplicateColumnInList(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues([]string{"id", "id"}, [][]parser.Token{
		tokRow(numTok("1"), numTok("2")),
	})

	assert.EqualError(t, err, `duplicate column "id" in column list`)
}

func TestTable_InsertValues_OmittedColumnWithDefault(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{
		Name:        "age",
		DataType:    parser.IntDataType{},
		Constraints: []parser.Constraint{parser.DefaultConstraint{DefaultValue: numTok("18")}},
	})

	err := table.InsertValues([]string{"id", "name"}, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
	})

	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	assert.Equal(t, numTok("18"), table.Rows[0][2])
}

func TestTable_InsertValues_OmittedNotNullColumnWithoutDefault(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{
		Name:        "age",
		DataType:    parser.IntDataType{},
		Constraints: []parser.Constraint{parser.NotNullConstraint{}},
	})

	err := table.InsertValues([]string{"id", "name"}, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
	})

	assert.EqualError(t, err, `field "age" doesn't have a default value`)
}

func TestTable_InsertValues_OmittedNullableColumn(t *testing.T) {
	table := usersTable(t, parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}})

	err := table.InsertValues([]string{"id", "name"}, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
	})

	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	assert.Nil(t, table.Rows[0][2])
}

func TestTable_InsertValues_BatchIsAtomic(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
		tokRow(numTok("3")), // bad arity
	})

	require.Error(t, err)
	assert.Empty(t, table.Rows, "a failed batch must not leave earlier rows committed")
}

func TestTable_InsertValues_Concurrent(t *testing.T) {
	table := usersTable(t)
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = table.InsertValues(nil, [][]parser.Token{
				tokRow(numTok("1"), strTok("bob")),
			})
		}(i)
	}
	wg.Wait()

	assert.Len(t, table.Rows, n)
}
