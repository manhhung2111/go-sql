package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

const (
	colID    = 0
	colEmail = 1
	colName  = 2
)

// keyedTable has a PRIMARY KEY (id), a UNIQUE column (email) and a plain one.
func keyedTable(t *testing.T) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "users", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "email", DataType: parser.VarCharDataType{Size: 100}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
	})
	require.NoError(t, err)
	return table
}

func insertUser(t *testing.T, table *SqlTable, id int, email, name string) {
	t.Helper()
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok(fmt.Sprint(id)), strTok(email), strTok(name)),
	}))
}

// writeRaw stores row in the table's file without any of the statement-level
// checks, the way rows written by an older version or an interrupted UPDATE
// got there.
func writeRaw(t *testing.T, table *SqlTable, row []any) storage.RowID {
	t.Helper()
	b, err := EncodeRow(table.Columns, row)
	require.NoError(t, err)
	id, err := table.file.Insert(b)
	require.NoError(t, err)
	require.NoError(t, table.file.Sync())
	return id
}

func entries(tree *index.Tree) []string {
	out := []string{}
	for key, id := range tree.Range(nil, nil) {
		out = append(out, fmt.Sprintf("%x=%d/%d", key, id.Page, id.Slot))
	}
	return out
}

// assertIndexesMatchHeap fails unless the table's built indexes hold exactly
// what a fresh build from the heap would.
func assertIndexesMatchHeap(t *testing.T, table *SqlTable) {
	t.Helper()
	got := table.builtIndexes()
	require.NotNil(t, got, "the indexes must be built")
	want, err := table.buildIndexes()
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for col, wantTree := range want {
		gotTree, ok := got[col]
		require.True(t, ok, "column %d has no entry", col)
		if wantTree == nil {
			assert.Nil(t, gotTree, "column %d cannot be indexed", col)
			continue
		}
		require.NotNil(t, gotTree, "column %d lost its tree", col)
		assert.Equal(t, entries(wantTree), entries(gotTree), "column %d", col)
	}
}

func TestTableIndexes_BuildEveryKeyColumn(t *testing.T) {
	table := keyedTable(t)
	idA := writeRaw(t, table, []any{int64(1), "a@x.com", "A"})
	writeRaw(t, table, []any{int64(2), nil, "B"}) // a NULL is never indexed
	idC := writeRaw(t, table, []any{int64(3), "c@x.com", "C"})

	trees, err := table.indexes()

	require.NoError(t, err)
	require.Len(t, trees, 2, "one tree per PRIMARY KEY/UNIQUE column, none for name")
	assert.Equal(t, 3, trees[colID].Len())
	assert.Equal(t, 2, trees[colEmail].Len())
	key, err := index.EncodeKey("a@x.com")
	require.NoError(t, err)
	got, found := trees[colEmail].Get(key)
	assert.True(t, found)
	assert.Equal(t, idA, got)
	key, err = index.EncodeKey(int64(3))
	require.NoError(t, err)
	got, found = trees[colID].Get(key)
	assert.True(t, found)
	assert.Equal(t, idC, got)
}

func TestTableIndexes_BuiltOnceAndKeptUntilDropped(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "a@x.com", "A"})
	cf := countOn(table)
	assert.Nil(t, table.builtIndexes(), "nothing is built before first use")

	_, err := table.indexes()
	require.NoError(t, err)
	_, err = table.indexes()
	require.NoError(t, err)
	assert.EqualValues(t, 1, cf.scans.Load(), "the second call reuses the trees")
	assert.NotNil(t, table.builtIndexes())

	table.dropIndexes()
	assert.Nil(t, table.builtIndexes())
	_, err = table.indexes()
	require.NoError(t, err)
	assert.EqualValues(t, 2, cf.scans.Load(), "a dropped index is rebuilt from the heap")
}

func TestTableIndexes_RebuiltWhenTheFileChanges(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "a@x.com", "A"})
	cf := countOn(table)
	_, err := table.indexes()
	require.NoError(t, err)

	table.fileID++ // what adopting a rewritten file does
	_, err = table.indexes()

	require.NoError(t, err)
	assert.EqualValues(t, 2, cf.scans.Load(), "trees built from another file are never trusted")
}

func TestTableIndexes_DisabledMeansNoTrees(t *testing.T) {
	table := keyedTable(t)
	table.noIndexes = true
	cf := countOn(table)

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.Empty(t, trees)
	assert.Nil(t, table.builtIndexes())
	assert.EqualValues(t, 0, cf.scans.Load())
}

func TestTableIndexes_NoKeyColumnsNeedNoScan(t *testing.T) {
	table := usersTable(t) // no PRIMARY KEY, no UNIQUE
	cf := countOn(table)

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.NotNil(t, trees)
	assert.Empty(t, trees)
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.NotNil(t, table.builtIndexes(), "an empty set of trees counts as built")
}

func TestTableIndexes_ADuplicateKeyDegradesOnlyThatColumn(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "same@x.com", "A"})
	writeRaw(t, table, []any{int64(2), "same@x.com", "B"}) // what an interrupted UPDATE can leave

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.Nil(t, trees[colEmail], "email keeps its scan path")
	require.NotNil(t, trees[colID])
	assert.Equal(t, 2, trees[colID].Len())
}

func TestTableIndexes_AKeyOverTheLimitDegradesOnlyThatColumn(t *testing.T) {
	table, err := newTempTable(t, "docs", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "code", DataType: parser.TextDataType{}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
	})
	require.NoError(t, err)
	writeRaw(t, table, []any{int64(1), strings.Repeat("x", maxKeyLen+1)}) // written before such keys were rejected
	writeRaw(t, table, []any{int64(2), "short"})

	trees, err := table.indexes()

	require.NoError(t, err)
	assert.Nil(t, trees[1], "code keeps its scan path")
	require.NotNil(t, trees[0])
	assert.Equal(t, 2, trees[0].Len())
}

func TestTableIndexes_AClosedTableCannotBuild(t *testing.T) {
	table := keyedTable(t)
	require.NoError(t, table.Close())

	trees, err := table.indexes()

	assert.EqualError(t, err, `table "users" is closed`)
	assert.Nil(t, trees)
	assert.Nil(t, table.builtIndexes(), "a failed build caches nothing")
}

// Select holds only the read lock, so several readers can ask for the indexes
// at once; they must build them exactly once. Meaningful under -race.
func TestTableIndexes_ConcurrentReadersBuildOnce(t *testing.T) {
	table := keyedTable(t)
	for i := 0; i < 200; i++ {
		writeRaw(t, table, []any{int64(i), fmt.Sprintf("u%d@x.com", i), "n"})
	}
	cf := countOn(table)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			table.mu.RLock()
			defer table.mu.RUnlock()
			trees, err := table.indexes()
			if assert.NoError(t, err) {
				assert.Equal(t, 200, trees[colID].Len())
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, cf.scans.Load())
}

// builtKeyedTable is a keyedTable holding five rows whose indexes are built.
func builtKeyedTable(t *testing.T) *SqlTable {
	t.Helper()
	table := keyedTable(t)
	for i := 1; i <= 5; i++ {
		writeRaw(t, table, []any{int64(i), fmt.Sprintf("u%d@x.com", i), fmt.Sprintf("n%d", i)})
	}
	_, err := table.indexes()
	require.NoError(t, err)
	return table
}

func TestTableIndexes_InsertAddsTheNewRows(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.InsertValues(nil, [][]parser.Token{
		tokRow(numTok("6"), strTok("u6@x.com"), strTok("n6")),
		tokRow(numTok("7"), strTok("u7@x.com"), strTok("n7")),
	}))
	require.NoError(t, table.InsertValues([]string{"id"}, [][]parser.Token{tokRow(numTok("8"))})) // email stays NULL

	assertIndexesMatchHeap(t, table)
	assert.Equal(t, 8, table.builtIndexes()[colID].Len())
	assert.Equal(t, 7, table.builtIndexes()[colEmail].Len())
}

func TestTableIndexes_DeleteRemovesTheRows(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.Delete(cmp(identTok("id"), parser.GT, numTok("3"))))
	assertIndexesMatchHeap(t, table)
	assert.Equal(t, 3, table.builtIndexes()[colID].Len())

	require.NoError(t, table.Delete(nil))
	assertIndexesMatchHeap(t, table)
	assert.Equal(t, 0, table.builtIndexes()[colID].Len())
}

func TestTableIndexes_DeleteDoesNotBuildThem(t *testing.T) {
	table := keyedTable(t)
	writeRaw(t, table, []any{int64(1), "a@x.com", "A"})

	require.NoError(t, table.Delete(nil))

	assert.Nil(t, table.builtIndexes(), "only statements that need an index build one")
}

func TestTableIndexes_UpdateRepointsEveryUpdatedRow(t *testing.T) {
	t.Run("a key column changes", func(t *testing.T) {
		table := builtKeyedTable(t)

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "email", Value: strTok("new@x.com")}},
			cmp(identTok("id"), parser.EQ, numTok("2"))))

		assertIndexesMatchHeap(t, table)
		key, err := index.EncodeKey("new@x.com")
		require.NoError(t, err)
		_, found := table.builtIndexes()[colEmail].Get(key)
		assert.True(t, found)
		key, err = index.EncodeKey("u2@x.com")
		require.NoError(t, err)
		_, found = table.builtIndexes()[colEmail].Get(key)
		assert.False(t, found, "the old value is gone")
	})

	t.Run("only a plain column changes, on many rows", func(t *testing.T) {
		table := builtKeyedTable(t)

		require.NoError(t, table.Update([]parser.Assignment{{Column: "name", Value: strTok("same")}}, nil))

		assertIndexesMatchHeap(t, table) // every row moved to a new RowID
		assert.Equal(t, 5, table.builtIndexes()[colID].Len())
	})

	t.Run("no row matches", func(t *testing.T) {
		table := builtKeyedTable(t)

		require.NoError(t, table.Update(
			[]parser.Assignment{{Column: "name", Value: strTok("x")}},
			cmp(identTok("id"), parser.EQ, numTok("99"))))

		assertIndexesMatchHeap(t, table)
	})
}

func TestTableIndexes_AWriteThatFailsDiscardsThemAndTheyRebuildCorrectly(t *testing.T) {
	tests := []struct {
		name string
		arm  func(ff *failingFile)
		run  func(table *SqlTable) error
	}{
		{"insert fails", func(ff *failingFile) { ff.failInserts = true }, func(table *SqlTable) error {
			return table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("z@x.com"), strTok("z"))})
		}},
		{"sync fails after an insert", func(ff *failingFile) { ff.failSyncs = true }, func(table *SqlTable) error {
			return table.InsertValues(nil, [][]parser.Token{tokRow(numTok("9"), strTok("z@x.com"), strTok("z"))})
		}},
		{"delete fails", func(ff *failingFile) { ff.failDeletes = 1 }, func(table *SqlTable) error {
			return table.Delete(cmp(identTok("id"), parser.GT, numTok("1")))
		}},
		{"sync fails after a delete", func(ff *failingFile) { ff.failSyncs = true }, func(table *SqlTable) error {
			return table.Delete(nil)
		}},
		{"update's insert fails", func(ff *failingFile) { ff.failInserts = true }, func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, nil)
		}},
		{"update's delete fails", func(ff *failingFile) { ff.failDeletes = 1 }, func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, nil)
		}},
		{"update's sync fails", func(ff *failingFile) { ff.failSyncs = true }, func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := builtKeyedTable(t)
			ff := &failingFile{File: table.file}
			table.file = ff
			tc.arm(ff)

			err := tc.run(table)

			require.ErrorIs(t, err, errInjected)
			assert.Nil(t, table.builtIndexes(), "the indexes may disagree with the file, so they are gone")

			*ff = failingFile{File: ff.File} // the disk behaves again
			_, err = table.indexes()
			require.NoError(t, err)
			assertIndexesMatchHeap(t, table)
		})
	}
}

func TestTableIndexes_AStatementThatFailsBeforeWritingKeepsThem(t *testing.T) {
	table := builtKeyedTable(t)

	err := table.InsertValues(nil, [][]parser.Token{tokRow(strTok("not a number"), strTok("z@x.com"), strTok("z"))})
	require.Error(t, err)

	assert.NotNil(t, table.builtIndexes())
	assertIndexesMatchHeap(t, table)
}

func TestTableIndexes_AddAndDropColumnDiscardThem(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.AlterColumns(parser.AddColumnAction{Column: parser.ColumnDefinition{Name: "age", DataType: parser.IntDataType{}}}))
	assert.Nil(t, table.builtIndexes(), "an ALTER rewrites the file, so every RowID is new")
	_, err := table.indexes()
	require.NoError(t, err)
	assertIndexesMatchHeap(t, table)

	require.NoError(t, table.AlterColumns(parser.DropColumnAction{Column: "name"}))
	assert.Nil(t, table.builtIndexes())
	_, err = table.indexes()
	require.NoError(t, err)
	assertIndexesMatchHeap(t, table)
}

func TestTableIndexes_RenamingAColumnKeepsThem(t *testing.T) {
	table := builtKeyedTable(t)

	require.NoError(t, table.AlterColumns(parser.RenameColumnAction{OldName: "name", NewName: "nickname"}))

	assert.NotNil(t, table.builtIndexes(), "no row moved and no column index changed")
	assertIndexesMatchHeap(t, table)
}

func TestTableIndexes_CloseAndDropDiscardThem(t *testing.T) {
	closed := builtKeyedTable(t)
	require.NoError(t, closed.Close())
	assert.Nil(t, closed.builtIndexes())

	dropped := builtKeyedTable(t)
	require.NoError(t, dropped.Drop())
	assert.Nil(t, dropped.builtIndexes())
}

// With no PRIMARY KEY, an UPDATE of the UNIQUE column can be the first
// statement to need the index. It builds the trees itself, and must still
// remember the old key so it can be removed afterwards.
func TestTableIndexes_AnUpdateThatBuildsThemLeavesThemRight(t *testing.T) {
	table, err := newTempTable(t, "contacts", []parser.ColumnDefinition{
		{Name: "email", DataType: parser.VarCharDataType{Size: 100}, Constraints: []parser.Constraint{parser.UniqueConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
	})
	require.NoError(t, err)
	writeRaw(t, table, []any{"a@x.com", "A"})
	require.Nil(t, table.builtIndexes())

	require.NoError(t, table.Update([]parser.Assignment{{Column: "email", Value: strTok("b@x.com")}}, nil))

	assertIndexesMatchHeap(t, table)
	require.NoError(t, table.InsertValues(nil, [][]parser.Token{tokRow(strTok("a@x.com"), strTok("A again"))}), "the old email is free")
}

// An entry the file does not back means the index was wrong: it is discarded.
func TestTableIndexes_AMissingEntryDiscardsThem(t *testing.T) {
	tests := []struct {
		name string
		run  func(table *SqlTable) error
	}{
		// These WHEREs scan. A key lookup would not notice the missing entry:
		// it would find nothing and stop.
		{"delete", func(table *SqlTable) error {
			return table.Delete(cmp(identTok("id"), parser.GTE, numTok("3")))
		}},
		{"update", func(table *SqlTable) error {
			return table.Update([]parser.Assignment{{Column: "name", Value: strTok("x")}}, cmp(identTok("name"), parser.EQ, strTok("n3")))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			table := builtKeyedTable(t)
			key, err := index.EncodeKey(int64(3))
			require.NoError(t, err)
			require.True(t, table.builtIndexes()[colID].Delete(key), "now the index disagrees with the file")

			require.NoError(t, tc.run(table))

			assert.Nil(t, table.builtIndexes())
		})
	}
}
