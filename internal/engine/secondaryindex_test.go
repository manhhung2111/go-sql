package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// namedIndex is a keyedTable (id PRIMARY KEY, email UNIQUE, name) with a
// CREATE INDEX on name.
func namedIndex(t *testing.T) *SqlTable {
	t.Helper()
	table := keyedTable(t)
	require.NoError(t, table.CreateIndex("by_name", "name", false))
	return table
}

func insertNamed(t *testing.T, table *SqlTable, id int, name string) {
	t.Helper()
	insertUser(t, table, id, fmt.Sprintf("u%d@x.com", id), name)
}

// idsOfName returns the row ids a secondary tree holds for value, in tree order.
func idsOfName(t *testing.T, tree *index.Tree, value any) []storage.RowID {
	t.Helper()
	lo, hi, err := index.SecondaryEqualRange(value)
	require.NoError(t, err)
	var ids []storage.RowID
	for _, id := range tree.Range(lo, hi) {
		ids = append(ids, id)
	}
	return ids
}

func TestSecondaryIndex_IsBuiltWithTheOthersAndHoldsDuplicates(t *testing.T) {
	table := namedIndex(t)
	a := writeRaw(t, table, []any{int64(1), "a@x.com", "n1"})
	writeRaw(t, table, []any{int64(2), "b@x.com", nil}) // NULL is never indexed
	b := writeRaw(t, table, []any{int64(3), "c@x.com", "n1"})
	c := writeRaw(t, table, []any{int64(4), "d@x.com", "n2"})

	set, err := table.indexSet()

	require.NoError(t, err)
	require.Len(t, set.unique, 2)
	require.Contains(t, set.secondary, "by_name")
	tree := set.secondary["by_name"]
	require.NotNil(t, tree)
	assert.Equal(t, 3, tree.Len(), "one entry per non-NULL value")
	assert.Equal(t, []storage.RowID{a, b}, idsOfName(t, tree, "n1"), "duplicates, in file order")
	assert.Equal(t, []storage.RowID{c}, idsOfName(t, tree, "n2"))
	assert.Empty(t, idsOfName(t, tree, "n3"))
}

func TestSecondaryIndex_WithNoUniqueColumnsStillBuilds(t *testing.T) {
	table, err := newTempTable(t, "events", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "kind", DataType: parser.VarCharDataType{Size: 20}},
	})
	require.NoError(t, err)
	require.NoError(t, table.CreateIndex("by_kind", "kind", false))
	writeRaw(t, table, []any{int64(1), "x"})
	writeRaw(t, table, []any{int64(2), "x"})

	set, err := table.indexSet()

	require.NoError(t, err)
	assert.Empty(t, set.unique)
	assert.Equal(t, 2, set.secondary["by_kind"].Len())
}

func TestSecondaryIndex_StaysInStepWithEveryWrite(t *testing.T) {
	table := namedIndex(t)
	for i := 1; i <= 6; i++ {
		insertNamed(t, table, i, fmt.Sprintf("n%d", i%3))
	}
	_, err := table.indexSet()
	require.NoError(t, err)

	steps := []struct {
		name string
		run  func() error
	}{
		{"insert", func() error {
			return table.InsertValues(nil, [][]parser.Token{tokRow(numTok("7"), strTok("u7@x.com"), strTok("n1"))})
		}},
		{"insert with NULL name", func() error {
			return table.InsertValues([]string{"id", "email"}, [][]parser.Token{tokRow(numTok("8"), strTok("u8@x.com"))})
		}},
		{"update the indexed value", func() error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("moved")}}, eqCmp("id", numTok("2")))
		}},
		{"update another column: the row still moves", func() error {
			return table.Update([]parser.Assignment{{Column: "email", Value: strTok("new@x.com")}}, eqCmp("id", numTok("3")))
		}},
		{"update several rows", func() error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("all")}}, cmp(identTok("id"), parser.GTE, numTok("5")))
		}},
		{"delete one", func() error { return table.Delete(eqCmp("id", numTok("1"))) }},
		{"delete by the indexed value", func() error { return table.Delete(eqCmp("name", strTok("all"))) }},
		{"delete everything", func() error { return table.Delete(nil) }},
	}
	for _, step := range steps {
		require.NoError(t, step.run(), step.name)
		assertIndexesMatchHeap(t, table)
	}
}

func TestSecondaryIndex_AFailedWriteDiscardsItAndItRebuilds(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*failingFile)
		run  func(*SqlTable) error
	}{
		{"an insert", func(f *failingFile) { f.failInserts = true }, func(tb *SqlTable) error {
			return tb.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("z@x.com"), strTok("n1"))})
		}},
		{"a sync", func(f *failingFile) { f.failSyncs = true }, func(tb *SqlTable) error {
			return tb.Update([]parser.Assignment{{Column: "name", Value: strTok("q")}}, eqCmp("id", numTok("1")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := namedIndex(t)
			insertNamed(t, table, 1, "n1")
			_, err := table.indexSet()
			require.NoError(t, err)
			ff := &failingFile{File: table.file}
			table.file = ff
			tc.fail(ff)

			assert.Error(t, tc.run(table))

			assert.Nil(t, table.builtSet(), "the trees were discarded")
			ff.failInserts, ff.failSyncs = false, false
			_, err = table.indexSet()
			require.NoError(t, err)
			assertIndexesMatchHeap(t, table)
		})
	}
}

func TestSecondaryIndex_CreateAndDropIndexDiscardTheTrees(t *testing.T) {
	table := keyedTable(t)
	insertNamed(t, table, 1, "n1")
	_, err := table.indexSet()
	require.NoError(t, err)

	require.NoError(t, table.CreateIndex("by_name", "name", false))
	assert.Nil(t, table.builtSet(), "a new index needs a build")
	set, err := table.indexSet()
	require.NoError(t, err)
	assert.Contains(t, set.secondary, "by_name")

	require.NoError(t, table.DropIndex("by_name", false))
	assert.Nil(t, table.builtSet(), "a dropped index must not linger")
	set, err = table.indexSet()
	require.NoError(t, err)
	assert.NotContains(t, set.secondary, "by_name")
}

func TestSecondaryIndex_FollowsSchemaChanges(t *testing.T) {
	table := namedIndex(t)
	insertNamed(t, table, 1, "n1")
	insertNamed(t, table, 2, "n1")
	_, err := table.indexSet()
	require.NoError(t, err)

	t.Run("renaming the indexed column keeps the trees", func(t *testing.T) {
		require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "label"}))

		assert.NotNil(t, table.builtSet())
		assertIndexesMatchHeap(t, table)
	})
	t.Run("adding and dropping another column rebuilds with the right column", func(t *testing.T) {
		require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "extra", DataType: parser.IntDataType{}}}))
		set, err := table.indexSet()
		require.NoError(t, err)
		assert.Equal(t, 2, set.secondary["by_name"].Len())
		require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "email"}))
		assert.Nil(t, table.builtSet(), "a rewritten file discards the trees")
		_, err = table.indexSet()
		require.NoError(t, err)
		assertIndexesMatchHeap(t, table)
	})
}

// notes is a table with a TEXT column, which (unlike the VARCHAR(50) name of
// keyedTable) can hold a value longer than an index entry may be, with a
// CREATE INDEX on it.
func notesTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "notes", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "note", DataType: parser.TextDataType{}},
	})
	require.NoError(t, err)
	require.NoError(t, table.CreateIndex("by_note", "note", false))
	return table
}

func TestSecondaryIndex_LongValues(t *testing.T) {
	long := strings.Repeat("x", maxKeyLen) // the value part alone leaves no room for the row id

	t.Run("INSERT and UPDATE reject them and write nothing", func(t *testing.T) {
		table := notesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("short"))}))

		err := table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok(long))})
		assert.ErrorContains(t, err, `key too long for index "by_note"`)
		err = table.Update([]parser.Assignment{{Column: "note", Value: strTok(long)}}, eqCmp("id", numTok("1")))
		assert.ErrorContains(t, err, `key too long for index "by_note"`)

		assert.Len(t, fileRows(t, table), 1)
		resp, err := table.Select([]string{"note"}, nil)
		require.NoError(t, err)
		assert.Equal(t, [][]string{{"short"}}, resp.Rows)
	})
	t.Run("a value that just fits is accepted", func(t *testing.T) {
		table := notesTable(t)
		fits := strings.Repeat("x", maxKeyLen-10) // value part n+2, plus 8 for the row id: exactly maxKeyLen
		over := strings.Repeat("x", maxKeyLen-9)  // one byte too many

		assert.ErrorContains(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("2"), strTok(over))}), "key too long")
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok(fits))}))

		set, err := table.indexSet()
		require.NoError(t, err)
		assert.Equal(t, 1, set.secondary["by_note"].Len())
	})
	t.Run("an UPDATE that matches nothing is not checked", func(t *testing.T) {
		table := notesTable(t)
		require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(numTok("1"), strTok("short"))}))

		err := table.Update([]parser.Assignment{{Column: "note", Value: strTok(long)}}, eqCmp("id", numTok("99")))

		assert.NoError(t, err)
	})
	t.Run("a value already in the file degrades only that index", func(t *testing.T) {
		table := notesTable(t)
		writeRaw(t, table, []any{int64(1), long})
		writeRaw(t, table, []any{int64(2), "short"})

		set, err := table.indexSet()

		require.NoError(t, err)
		assert.Nil(t, set.secondary["by_note"], "the index cannot hold the value")
		assert.NotNil(t, set.unique[0], "the unique tree is unaffected")
		assertIndexesMatchHeap(t, table)
	})
}
