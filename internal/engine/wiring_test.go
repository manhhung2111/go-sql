package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

// seededKeyedTable is a keyedTable with five rows whose indexes are built,
// watched by a counting file whose counters start at zero.
func seededKeyedTable(t *testing.T) (*SqlTable, *countingFile) {
	t.Helper()
	table := builtKeyedTable(t)
	cf := countOn(table)
	return table, cf
}

func TestUniqueness_UsesTheIndexInsteadOfScanning(t *testing.T) {
	table, cf := seededKeyedTable(t)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("fresh@x.com"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry 3 for column "id"`)

	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("u4@x.com"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry u4@x.com for column "email"`)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("u9@x.com"), strTok("n"))}))

	assert.EqualValues(t, 0, cf.scans.Load(), "checking keys must not read the table")
	assertIndexesMatchHeap(t, table)
}

func TestUniqueness_TheFirstStatementBuildsTheIndexWithOneScan(t *testing.T) {
	table := keyedTable(t)
	cf := countOn(table)
	insertUser(t, table, 1, "a@x.com", "A")
	assert.EqualValues(t, 1, cf.scans.Load(), "the first statement that needs the index builds it with one scan")
	cf.scans.Store(0)

	insertUser(t, table, 2, "b@x.com", "B")
	insertUser(t, table, 3, "c@x.com", "C")

	assert.EqualValues(t, 0, cf.scans.Load())
	assert.NotNil(t, table.builtIndexes())
}

func TestUniqueness_ABatchChecksItsOwnRowsToo(t *testing.T) {
	table, _ := seededKeyedTable(t)

	err := table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("10"), strTok("x@x.com"), strTok("n")),
		tokRow(numTok("11"), strTok("x@x.com"), strTok("n")),
	})

	assert.EqualError(t, err, `duplicate entry x@x.com for column "email"`)
	assert.Len(t, fileRows(t, table), 5, "a rejected batch writes nothing")
}

func TestUniqueness_AKeyIsFreeAgainAfterItsRowIsDeleted(t *testing.T) {
	table, _ := seededKeyedTable(t)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("2"))))

	insertUser(t, table, 2, "u2@x.com", "again")
	assertIndexesMatchHeap(t, table)
}

func TestUniqueness_UpdateMovesAKeyAndFreesTheOldOne(t *testing.T) {
	table, _ := seededKeyedTable(t)

	require.NoError(t, table.Update(
		[]parser.Assignment{{Column: "email", Value: strTok("moved@x.com")}},
		cmp(identTok("id"), parser.EQ, numTok("2"))))

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("moved@x.com"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry moved@x.com for column "email"`)
	insertUser(t, table, 9, "u2@x.com", "takes the old email")
	assertIndexesMatchHeap(t, table)
}

func TestUniqueness_UpdateConflictsAreFoundThroughTheIndex(t *testing.T) {
	t.Run("another row holds the value", func(t *testing.T) {
		table, _ := seededKeyedTable(t)

		err := table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("u4@x.com")}},
			cmp(identTok("id"), parser.EQ, numTok("2")))

		assert.EqualError(t, err, `duplicate entry u4@x.com for column "email"`)
		assertIndexesMatchHeap(t, table)
		assert.Len(t, fileRows(t, table), 5)
	})

	t.Run("re-assigning a row's own value is not a conflict", func(t *testing.T) {
		table, _ := seededKeyedTable(t)

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("u2@x.com")}},
			cmp(identTok("id"), parser.EQ, numTok("2"))))

		assertIndexesMatchHeap(t, table)
	})

	t.Run("two matched rows cannot share one value", func(t *testing.T) {
		table, _ := seededKeyedTable(t)

		err := table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("one@x.com")}},
			cmp(identTok("id"), parser.LTE, numTok("2")))

		assert.EqualError(t, err, `duplicate entry one@x.com for column "email"`)
		assertIndexesMatchHeap(t, table)
	})
}

func TestUniqueness_ADegradedColumnStillChecksByScanning(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "same@x.com", "A"})
	writeRaw(t, table, []any{int64(2), "same@x.com", "B"}) // an interrupted UPDATE left two rows with one email
	cf := countOn(table)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("same@x.com"), strTok("C"))})
	assert.EqualError(t, err, `duplicate entry same@x.com for column "email"`)
	assert.GreaterOrEqual(t, cf.scans.Load(), int64(1), "email has no tree, so it is checked by scanning")

	insertUser(t, table, 3, "fresh@x.com", "C")
	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("other@x.com"), strTok("D"))})
	assert.EqualError(t, err, `duplicate entry 3 for column "id"`, "id still has its tree")
	assert.Nil(t, table.builtIndexes()[colEmail])
	assert.NotNil(t, table.builtIndexes()[colID])
}

func codesTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "docs", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "code", DataType: parser.TextDataType{}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
		{Name: "note", DataType: parser.TextDataType{}},
	})
	require.NoError(t, err)
	return table
}

func TestLongKeys_AreRejectedAtTheLimit(t *testing.T) {
	atLimit := strings.Repeat("k", maxKeyLen)
	overLimit := strings.Repeat("k", maxKeyLen+1)
	wantErr := fmt.Sprintf(`key too long for column "code": %d bytes exceeds the maximum of %d`, maxKeyLen+1, maxKeyLen)

	t.Run("INSERT", func(t *testing.T) {
		table := codesTable(t)

		err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok(overLimit), strTok("n"))})
		assert.EqualError(t, err, wantErr)
		assertTableRows(t, table)

		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok(atLimit), strTok("n"))}))
		assertTableRows(t, table, []any{int64(1), atLimit, "n"})
	})

	t.Run("INSERT of a long value in a column that is not a key is fine", func(t *testing.T) {
		table := codesTable(t)

		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok(overLimit))}))
	})

	t.Run("a long value is reported where a duplicate would be", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))

		err := table.InsertValues(nil, [][]parser.Token{
			tokRow(numTok("1"), strTok(overLimit), strTok("n")), // duplicate id comes first in column order
		})

		assert.EqualError(t, err, `duplicate entry 1 for column "id"`)
	})

	t.Run("UPDATE", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))

		err := table.Update([]parser.Assignment{{Column: "code", Value: strTok(overLimit)}}, nil)

		assert.EqualError(t, err, wantErr)
		assertTableRows(t, table, []any{int64(1), "c", "n"})
		require.NoError(t, table.Update([]parser.Assignment{{Column: "code", Value: strTok(atLimit)}}, nil))
	})

	t.Run("an UPDATE that matches no row stores nothing, so it is not checked", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "code", Value: strTok(overLimit)}},
			cmp(identTok("id"), parser.EQ, numTok("99"))))
		assertTableRows(t, table, []any{int64(1), "c", "n"})
	})

	t.Run("ADD COLUMN with a long default", func(t *testing.T) {
		table := codesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("c"), strTok("n"))}))
		column := func(def string) parser.ColumnDefinition {
			return parser.ColumnDefinition{Name: "slug", DataType: parser.TextDataType{},
				Constraints: []parser.Constraint{parser.UniqueConstraint{}, parser.DefaultConstraint{DefaultValue: strTok(def)}}}
		}

		err := table.AlterColumns(parser.AddColumnAction{Column: column(overLimit)})
		assert.EqualError(t, err, strings.Replace(wantErr, `"code"`, `"slug"`, 1))
		assert.Len(t, table.Columns, 3, "the schema is unchanged")

		require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: column(atLimit)}))
	})
}

func TestLongKeys_RowsWrittenBeforeTheRuleKeepWorking(t *testing.T) {
	table := codesTable(t)
	long := strings.Repeat("k", maxKeyLen+10)
	writeRaw(t, table, []any{int64(1), long, "legacy"})

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok("short"), strTok("n"))}))
	err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("3"), strTok("short"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry short for column "code"`, "code is checked by scanning")
	err = table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("other"), strTok("n"))})
	assert.EqualError(t, err, `duplicate entry 1 for column "id"`)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.EQ, numTok("1"))))
	assertTableRows(t, table, []any{int64(2), "short", "n"})
}
