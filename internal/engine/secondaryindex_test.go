package engine

import (
	"fmt"
	"math/rand"
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
func TestPlanLookup_SecondaryIndex(t *testing.T) {
	table := namedIndex(t)
	columnIndex := map[string]int{"id": 0, "email": 1, "name": 2}
	lo, hi, err := index.SecondaryEqualRange("n1")
	require.NoError(t, err)
	lo2, hi2, err := index.SecondaryEqualRange("n3")
	require.NoError(t, err)

	tests := []struct {
		name  string
		where parser.Expression
		want  []rangePlan
	}{
		{"an equality is the range of one value", eqCmp("name", strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo, hi: hi}}},
		{"reversed operands", cmp(strTok("n1"), parser.EQ, identTok("name")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo, hi: hi}}},
		{">= starts at the value", cmp(identTok("name"), parser.GTE, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo}}},
		{"> starts after the value's last entry", cmp(identTok("name"), parser.GT, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", lo: hi}}},
		{"< ends before the value", cmp(identTok("name"), parser.LT, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", hi: lo}}},
		{"<= ends after the value's last entry", cmp(identTok("name"), parser.LTE, strTok("n1")),
			[]rangePlan{{column: colName, secondary: "by_name", hi: hi}}},
		{"a pair of bounds is one range", andExpr(cmp(identTok("name"), parser.GTE, strTok("n1")), cmp(identTok("name"), parser.LT, strTok("n3"))),
			[]rangePlan{{column: colName, secondary: "by_name", lo: lo, hi: lo2}}},
		{"the tighter bound wins", andExpr(cmp(identTok("name"), parser.GT, strTok("n1")), cmp(identTok("name"), parser.LTE, strTok("n3"))),
			[]rangePlan{{column: colName, secondary: "by_name", lo: hi, hi: hi2}}},
		{"no plan for !=", cmp(identTok("name"), parser.NEQ, strTok("n1")), nil},
		{"no plan for OR", binaryExpr(eqCmp("name", strTok("n1")), parser.OR, eqCmp("name", strTok("n2"))), nil},
		{"no plan for a column without an index", eqCmp("id", numTok("1")), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plans := table.planLookup(tc.where, columnIndex)

			if tc.want == nil {
				// a key column still plans its own equality; only the secondary part is absent
				assert.Empty(t, plans.ranges)
				return
			}
			assert.Equal(t, tc.want, plans.ranges)
		})
	}
}

func TestPlanLookup_AKeyColumnNeverUsesASecondaryIndex(t *testing.T) {
	table := keyedTable(t)
	require.NoError(t, table.CreateIndex("by_id", "id", false))
	columnIndex := map[string]int{"id": 0, "email": 1, "name": 2}

	plans := table.planLookup(cmp(identTok("id"), parser.GTE, numTok("3")), columnIndex)

	require.Len(t, plans.ranges, 1)
	assert.Empty(t, plans.ranges[0].secondary, "the unique tree serves it")
	plans = table.planLookup(eqCmp("id", numTok("3")), columnIndex)
	assert.Len(t, plans.equalities, 1)
	assert.Empty(t, plans.ranges)
}

// wideGroups is a table of 1000-byte-padded rows (about fifteen to a page) with
// an INT id, an INT grp and a CREATE INDEX on grp; groupOf gives each id's group.
// Its indexes are built.
func wideGroups(t *testing.T, n int, groupOf func(id int) int) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "groups", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "grp", DataType: parser.IntDataType{}},
		{Name: "pad", DataType: parser.VarCharDataType{Size: 1200}},
	})
	require.NoError(t, err)
	pad := strings.Repeat("x", 1000)
	rows := make([][]parser.Token, n)
	for id := 0; id < n; id++ {
		rows[id] = tokRow(numTok(fmt.Sprint(id)), numTok(fmt.Sprint(groupOf(id))), strTok(pad))
	}
	require.NoError(t, table.InsertValues(nil, rows))
	require.NoError(t, table.CreateIndex("by_grp", "grp", false))
	_, err = table.indexSet()
	require.NoError(t, err)
	return table
}

func grpEq(v int) parser.Expression { return eqCmp("grp", numTok(fmt.Sprint(v))) }

func TestSecondaryLookup_ServesAnEqualityWithoutAScan(t *testing.T) {
	withCostFactor(t, 2)
	table := wideGroups(t, 160, func(id int) int { return id / 40 }) // four clustered groups
	plain := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain.noIndexes = true
	cf := countOn(table)

	got := idsOf(t, table, grpEq(1))

	assert.Equal(t, idsOf(t, plain, grpEq(1)), got)
	assert.Len(t, got, 40)
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 1, cf.getManyCalls.Load())
	assert.EqualValues(t, 40, cf.gets.Load())
	assert.Less(t, cf.pageReads.Load(), int64(5), "each page of the group is read once")
}

func TestSecondaryLookup_ServesRanges(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain.noIndexes = true
	cf := countOn(table)
	wheres := map[string]parser.Expression{
		">= 1 and < 3": andExpr(cmp(identTok("grp"), parser.GTE, numTok("1")), cmp(identTok("grp"), parser.LT, numTok("3"))),
		"> 1":          cmp(identTok("grp"), parser.GT, numTok("1")),
		"<= 1":         cmp(identTok("grp"), parser.LTE, numTok("1")),
		"< 1":          cmp(identTok("grp"), parser.LT, numTok("1")),
		"> 3":          cmp(identTok("grp"), parser.GT, numTok("3")),
		"= 9":          grpEq(9),
		"5 > grp":      cmp(numTok("5"), parser.GT, identTok("grp")),
	}
	for name, where := range wheres {
		t.Run(name, func(t *testing.T) {
			cf.reset()

			got := idsOf(t, table, where)

			assert.Equal(t, idsOf(t, plain, where), got)
			assert.EqualValues(t, 0, cf.scans.Load(), "served from the index")
		})
	}
}

func TestSecondaryLookup_ValuesSpreadOverMostPagesScan(t *testing.T) {
	withCostFactor(t, 2)
	table := wideGroups(t, 160, func(id int) int { return id % 4 }) // every group touches every page
	plain := wideGroups(t, 160, func(id int) int { return id % 4 })
	plain.noIndexes = true
	cf := countOn(table)

	got := idsOf(t, table, grpEq(1))

	assert.Equal(t, idsOf(t, plain, grpEq(1)), got, "the same answer")
	assert.EqualValues(t, 1, cf.scans.Load())
	assert.EqualValues(t, 0, cf.getManyCalls.Load())
}

func TestSecondaryLookup_UpdateAndDeleteTouchOnlyTheMatches(t *testing.T) {
	withCostFactor(t, 0)
	indexed := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain := wideGroups(t, 160, func(id int) int { return id / 40 })
	plain.noIndexes = true
	cf := countOn(indexed)
	assign := []parser.Assignment{{Column: "pad", Value: strTok(strings.Repeat("y", 1000))}}

	require.NoError(t, indexed.Update(assign, grpEq(2)))
	require.NoError(t, plain.Update(assign, grpEq(2)))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.EqualValues(t, 40, cf.gets.Load())
	assert.Equal(t, fileRows(t, plain), fileRows(t, indexed), "the same file")
	assertIndexesMatchHeap(t, indexed)

	cf.reset()
	require.NoError(t, indexed.Delete(grpEq(2)))
	require.NoError(t, plain.Delete(grpEq(2)))
	assert.EqualValues(t, 0, cf.scans.Load())
	assert.Equal(t, fileRows(t, plain), fileRows(t, indexed))
	assertIndexesMatchHeap(t, indexed)

	cf.reset()
	assert.Empty(t, idsOf(t, indexed, grpEq(2)), "and the moved or deleted rows are no longer found")
}

func TestSecondaryLookup_TheWholeWhereIsStillChecked(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 160, func(id int) int { return id / 40 })

	got := idsOf(t, table, andExpr(grpEq(1), eqCmp("id", numTok("50"))))

	assert.Equal(t, [][]string{{"50"}}, got)
	got = idsOf(t, table, andExpr(grpEq(1), cmp(identTok("id"), parser.LT, numTok("45"))))
	assert.Len(t, got, 5, "ids 40..44")
}

func TestSecondaryLookup_APrimaryKeyEqualityWinsOverASecondaryOne(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 160, func(id int) int { return id / 40 })
	cf := countOn(table)

	got := idsOf(t, table, andExpr(grpEq(1), eqCmp("id", numTok("50"))))

	assert.Equal(t, [][]string{{"50"}}, got)
	assert.EqualValues(t, 1, cf.gets.Load(), "one row fetched through the unique tree")
	assert.EqualValues(t, 0, cf.getManyCalls.Load())
}

func TestSecondaryLookup_ANullIsFoundByNoLookup(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 40, func(id int) int { return id / 20 })
	plain := wideGroups(t, 40, func(id int) int { return id / 20 })
	plain.noIndexes = true
	for _, tb := range []*SqlTable{table, plain} {
		// grp is left out, so it is NULL
		require.NoError(t, tb.InsertValues([]string{"id", "pad"}, [][]parser.Token{tokRow(numTok("100"), strTok("x"))}))
	}

	for _, where := range []parser.Expression{grpEq(0), cmp(identTok("grp"), parser.GTE, numTok("0")), cmp(identTok("grp"), parser.LT, numTok("5"))} {
		assert.Equal(t, idsOf(t, plain, where), idsOf(t, table, where))
	}
	assertIndexesMatchHeap(t, table)
}

func TestSecondaryLookup_AStaleEntryFallsBackToTheScan(t *testing.T) {
	withCostFactor(t, 0)
	table := wideGroups(t, 40, func(id int) int { return id / 20 })
	set, err := table.indexSet()
	require.NoError(t, err)
	ids := idsOfName(t, set.secondary["by_grp"], int64(0))
	require.NoError(t, table.file.Delete(ids[0])) // behind the index's back
	require.NoError(t, table.file.Sync())

	got := idsOf(t, table, grpEq(0))

	assert.Len(t, got, 19, "the scan's answer")
	assert.Nil(t, table.builtSet(), "the wrong index is discarded")
}

func TestSecondaryLookup_ADegradedIndexScans(t *testing.T) {
	withCostFactor(t, 0)
	table := namedIndex(t)
	long := strings.Repeat("x", maxKeyLen)
	writeRaw(t, table, []any{int64(1), "a@x.com", long})
	writeRaw(t, table, []any{int64(2), "b@x.com", "n1"})
	_, err := table.indexSet() // the build is a scan too; count the statement's only
	require.NoError(t, err)
	cf := countOn(table)

	resp, err := table.Select([]string{"id"}, eqCmp("name", strTok("n1")))

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"2"}}, resp.Rows)
	assert.EqualValues(t, 1, cf.scans.Load())
}

// events has no PRIMARY KEY or UNIQUE column, so every index lookup in this
// comparison is a lookup in the secondary tree.
func eventsTable(t *testing.T, noIndexes bool) *SqlTable {
	t.Helper()
	table, err := newTempTable(t, "events", []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{}},
		{Name: "kind", DataType: parser.VarCharDataType{Size: 20}},
	})
	require.NoError(t, err)
	require.NoError(t, table.CreateIndex("by_kind", "kind", false))
	table.noIndexes = noIndexes
	return table
}

func randKind(rng *rand.Rand) string { return fmt.Sprintf("k%d", rng.Intn(5)) }

func randomKindWhere(rng *rand.Rand) parser.Expression {
	id := func() parser.Token { return numTok(fmt.Sprint(rng.Intn(30))) }
	kind := func() parser.Token { return strTok(randKind(rng)) }
	ops := []parser.TokenType{parser.LT, parser.LTE, parser.GT, parser.GTE}
	switch rng.Intn(13) {
	case 0:
		return nil
	case 1, 2:
		return cmp(identTok("kind"), parser.EQ, kind())
	case 3:
		return cmp(kind(), parser.EQ, identTok("kind"))
	case 4:
		return cmp(identTok("kind"), ops[rng.Intn(4)], kind())
	case 5:
		return cmp(kind(), ops[rng.Intn(4)], identTok("kind"))
	case 6:
		return andExpr(cmp(identTok("kind"), ops[2+rng.Intn(2)], kind()), cmp(identTok("kind"), ops[rng.Intn(2)], kind()))
	case 7:
		return andExpr(cmp(identTok("kind"), parser.EQ, kind()), cmp(identTok("id"), ops[rng.Intn(4)], id()))
	case 8:
		return andExpr(cmp(identTok("kind"), parser.EQ, kind()), cmp(identTok("kind"), parser.NEQ, kind()))
	case 9:
		return binaryExpr(cmp(identTok("kind"), parser.EQ, kind()), parser.OR, cmp(identTok("id"), parser.EQ, id()))
	case 10:
		return cmp(identTok("id"), parser.GTE, id())
	case 11:
		return andExpr(cmp(identTok("kind"), parser.EQ, kind()), cmp(identTok("nosuch"), parser.EQ, numTok("1")))
	default:
		return cmp(identTok("kind"), parser.EQ, numTok("7")) // a literal that does not coerce
	}
}

func randomEventStatement(rng *rand.Rand) func(*SqlTable) (Response, error) {
	switch rng.Intn(10) {
	case 0, 1, 2, 3:
		rows := make([][]parser.Token, 1+rng.Intn(4))
		for i := range rows {
			if rng.Intn(8) == 0 {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(30))), parser.Token{Type: parser.NULL, Value: "NULL"})
			} else {
				rows[i] = tokRow(numTok(fmt.Sprint(rng.Intn(30))), strTok(randKind(rng)))
			}
		}
		return func(table *SqlTable) (Response, error) { return Response{}, table.InsertValues(nil, rows) }
	case 4, 5:
		assignment := parser.Assignment{Column: "kind", Value: strTok(randKind(rng))}
		if rng.Intn(3) == 0 {
			assignment = parser.Assignment{Column: "id", Value: numTok(fmt.Sprint(rng.Intn(30)))}
		}
		where := randomKindWhere(rng)
		return func(table *SqlTable) (Response, error) {
			return Response{}, table.Update([]parser.Assignment{assignment}, where)
		}
	case 6, 7:
		where := randomKindWhere(rng)
		return func(table *SqlTable) (Response, error) { return Response{}, table.Delete(where) }
	default:
		where := randomKindWhere(rng)
		return func(table *SqlTable) (Response, error) { return table.Select([]string{"*"}, where) }
	}
}

func TestSecondaryIndexMatchesAnUnindexedTable(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			indexed, plain := eventsTable(t, false), eventsTable(t, true)
			cf := countOn(indexed)

			for op := 0; op < 400; op++ {
				statement := randomEventStatement(rng)

				gotResp, gotErr := statement(indexed)
				wantResp, wantErr := statement(plain)

				require.Equal(t, errText(wantErr), errText(gotErr), "op %d", op)
				require.Equal(t, wantResp, gotResp, "op %d", op)
				require.Equal(t, fileRows(t, plain), fileRows(t, indexed), "op %d", op)
				if indexed.builtSet() != nil {
					assertIndexesMatchHeap(t, indexed)
				}
			}
			assert.Greater(t, cf.getManyCalls.Load(), int64(0), "the secondary tree must have served some statements")
		})
	}
}
