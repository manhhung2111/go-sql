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
	assert.Equal(t, int64(1), table.Rows[0][0]) // id, schema position 0
	assert.Equal(t, "bob", table.Rows[0][1])    // name, schema position 1
}

func TestTable_InsertValues_NoColumnList_FullRow(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
	})

	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	assert.Equal(t, []any{int64(1), "bob"}, table.Rows[0])
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
	assert.Equal(t, int64(18), table.Rows[0][2])
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

func TestNewTable_RejectsInvalidDefault(t *testing.T) {
	_, err := NewTable("users", []parser.ColumnDefinition{
		{
			Name:        "age",
			DataType:    parser.IntDataType{},
			Constraints: []parser.Constraint{parser.DefaultConstraint{DefaultValue: strTok("not-a-number")}},
		},
	})

	assert.EqualError(t, err, `column "age": invalid default value: expected a numeric value, got not-a-number`)
}

func TestTable_InsertValues_WrongLiteralKind(t *testing.T) {
	table := usersTable(t)

	err := table.InsertValues([]string{"id", "name"}, [][]parser.Token{
		tokRow(strTok("one"), strTok("bob")),
	})

	assert.EqualError(t, err, `column "id": expected a numeric value, got one`)
}

func TestTable_InsertValues_PrimaryKeyImpliesNotNull(t *testing.T) {
	table, err := NewTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
	})
	require.NoError(t, err)

	err = table.InsertValues([]string{"name"}, [][]parser.Token{tokRow(strTok("bob"))})

	assert.EqualError(t, err, `field "id" doesn't have a default value`)
}

func uniqueUsersTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := NewTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "email", DataType: parser.VarCharDataType{Size: 100}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
	})
	require.NoError(t, err)
	return table
}

func TestTable_InsertValues_DuplicateAgainstExistingRow(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("a@x.com"))})

	assert.EqualError(t, err, `duplicate entry a@x.com for column "email"`)
}

func TestTable_InsertValues_DuplicateWithinSameBatch(t *testing.T) {
	table := uniqueUsersTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("a@x.com")),
		tokRow(numTok("2"), strTok("a@x.com")),
	})

	assert.EqualError(t, err, `duplicate entry a@x.com for column "email"`)
	assert.Empty(t, table.Rows, "a rejected batch must not leave earlier rows committed")
}

func TestTable_InsertValues_DuplicatePrimaryKey(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("a@x.com"))}))

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("b@x.com"))})

	assert.EqualError(t, err, `duplicate entry 1 for column "id"`)
}

func TestTable_InsertValues_NullExemptFromUniqueness(t *testing.T) {
	table := uniqueUsersTable(t)
	require.NoError(t, table.InsertValues([]string{"id"}, [][]parser.Token{tokRow(numTok("1"))}))

	err := table.InsertValues([]string{"id"}, [][]parser.Token{tokRow(numTok("2"))})

	require.NoError(t, err, "two rows both omitting the nullable UNIQUE column must not conflict")
	assert.Nil(t, table.Rows[0][1])
	assert.Nil(t, table.Rows[1][1])
}

func TestTable_AlterColumns_AddColumn_EmptyTable(t *testing.T) {
	table := usersTable(t)

	err := table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}},
	})

	require.NoError(t, err)
	require.Len(t, table.Columns, 3)
	assert.Equal(t, "age", table.Columns[2].Name)
}

func TestTable_AlterColumns_AddColumn_BackfillsDefault(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
	}))

	err := table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{
			Name:        "age",
			DataType:    parser.IntDataType{},
			Constraints: []parser.Constraint{parser.DefaultConstraint{DefaultValue: numTok("18")}},
		},
	})

	require.NoError(t, err)
	require.Len(t, table.Rows[0], 3)
	assert.Equal(t, int64(18), table.Rows[0][2])
	assert.Equal(t, int64(18), table.Rows[1][2])
}

func TestTable_AlterColumns_AddColumn_BackfillsNull(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))

	err := table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}},
	})

	require.NoError(t, err)
	assert.Nil(t, table.Rows[0][2])
}

func TestTable_AlterColumns_AddColumn_NotNullWithoutDefaultRejected(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))

	err := table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{
			Name:        "age",
			DataType:    parser.IntDataType{},
			Constraints: []parser.Constraint{parser.NotNullConstraint{}},
		},
	})

	assert.EqualError(t, err, `field "age" doesn't have a default value`)
	assert.Len(t, table.Columns, 2, "a rejected ADD COLUMN must not partially apply")
}

func TestTable_AlterColumns_AddColumn_UniqueWithDefaultRejectedOnMultipleRows(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("1"), strTok("bob")),
		tokRow(numTok("2"), strTok("sam")),
	}))

	err := table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{
			Name:     "region",
			DataType: parser.VarCharDataType{Size: 10},
			Constraints: []parser.Constraint{
				parser.UniqueConstraint{},
				parser.DefaultConstraint{DefaultValue: strTok("us")},
			},
		},
	})

	assert.EqualError(t, err, `duplicate entry us for column "region"`)
	assert.Len(t, table.Columns, 2, "a rejected ADD COLUMN must not partially apply")
}

func TestTable_AlterColumns_AddColumn_DuplicateName(t *testing.T) {
	table := usersTable(t)

	err := table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{Name: "id", DataType: parser.IntDataType{}},
	})

	assert.EqualError(t, err, `column name "id" already exists`)
}

func TestTable_AlterColumns_AddColumn_SecondPrimaryKeyRejected(t *testing.T) {
	table, err := NewTable("users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
	})
	require.NoError(t, err)

	err = table.AlterColumns(parser.AddColumnAction{
		Column: parser.ColumnDefinition{
			Name:        "email",
			DataType:    parser.VarCharDataType{Size: 50},
			Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}},
		},
	})

	assert.EqualError(t, err, "table users has more than one primary key column")
}

func TestTable_AlterColumns_DropColumn(t *testing.T) {
	table := usersTable(t)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("bob"))}))

	err := table.AlterColumns(parser.DropColumnAction{Column: "name"})

	require.NoError(t, err)
	require.Len(t, table.Columns, 1)
	assert.Equal(t, "id", table.Columns[0].Name)
	require.Len(t, table.Rows[0], 1)
	assert.Equal(t, int64(1), table.Rows[0][0])
}

func TestTable_AlterColumns_DropColumn_Unknown(t *testing.T) {
	table := usersTable(t)

	err := table.AlterColumns(parser.DropColumnAction{Column: "nickname"})

	assert.EqualError(t, err, `unknown column "nickname"`)
}

func TestTable_AlterColumns_DropColumn_LastColumnRejected(t *testing.T) {
	table, err := NewTable("users", []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}})
	require.NoError(t, err)

	err = table.AlterColumns(parser.DropColumnAction{Column: "id"})

	assert.EqualError(t, err, "cannot drop the only column")
}

func TestTable_AlterColumns_RenameColumn(t *testing.T) {
	table := usersTable(t)

	err := table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "full_name"})

	require.NoError(t, err)
	assert.Equal(t, "full_name", table.Columns[1].Name)
}

func TestTable_AlterColumns_RenameColumn_UnknownOldName(t *testing.T) {
	table := usersTable(t)

	err := table.AlterColumns(parser.RenameColumnAction{OldName: "nickname", NewName: "n"})

	assert.EqualError(t, err, `unknown column "nickname"`)
}

func TestTable_AlterColumns_RenameColumn_CollidingNewName(t *testing.T) {
	table := usersTable(t)

	err := table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "id"})

	assert.EqualError(t, err, `column name "id" already exists`)
}

func TestTable_Rename(t *testing.T) {
	table := usersTable(t)

	table.Rename("people")

	assert.Equal(t, "people", table.Name)
}
