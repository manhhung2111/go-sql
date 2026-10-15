package engine

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// maxKeyLen is the longest PRIMARY KEY/UNIQUE string the indexes can hold.
var maxKeyLen = index.MaxKeyLenFor(index.DefaultNodeBytes)

// tableIndexes holds the in-memory indexes of one table: a cache of its heap
// file, built on first use and discarded whenever it might disagree with the
// file. Callers hold the table's mu; mu here only serialises building and
// discarding, and is a leaf lock (Table.mu, then tableIndexes.mu).
type tableIndexes struct {
	mu sync.Mutex
	// fileID is the file the trees were built from. A RowID means something
	// only within one file, so trees built from another file are never used.
	fileID int64
	// set holds the trees. A nil set means "not built"; a nil tree in it means
	// the column or index cannot hold the file's values (see buildIndexes) and
	// its statements scan the file instead.
	set *treeSet
}

// treeSet is the trees of one table: unique maps a PRIMARY KEY/UNIQUE column's
// schema index to its tree, secondary maps a CREATE INDEX name to its tree. A
// nil tree means the column or index cannot hold the file's values (see
// buildIndexes) and statements on it scan.
type treeSet struct {
	unique    map[int]*index.Tree
	secondary map[string]*index.Tree
}

// keyColumns returns the schema indexes of the PRIMARY KEY and UNIQUE columns.
func (t *SqlTable) keyColumns() []int {
	cols := make([]int, 0)
	for i, col := range t.Columns {
		if col.IsPrimaryKey() || col.IsUnique() {
			cols = append(cols, i)
		}
	}
	return cols
}

// keyValuesAt returns row's values at cols, which a statement computes once
// with keyColumns.
func keyValuesAt(row []any, cols []int) []any {
	values := make([]any, len(cols))
	for i, col := range cols {
		values[i] = row[col]
	}
	return values
}

// checkKeyLength rejects a string too long to be indexed. It applies to the
// values of PRIMARY KEY and UNIQUE columns only; callers check that.
func checkKeyLength(col string, value any) error {
	if s, ok := value.(string); ok && len(s) > maxKeyLen {
		return fmt.Errorf("key too long for column %q: %d bytes exceeds the maximum of %d", col, len(s), maxKeyLen)
	}
	return nil
}

// indexSet returns the table's trees, building them from one scan of the file
// if they are missing or were built from another file. Callers hold t.mu, for
// reading or writing. With noIndexes set it returns no trees, so every column
// takes its scan path.
func (t *SqlTable) indexSet() (*treeSet, error) {
	if t.noIndexes {
		return &treeSet{unique: map[int]*index.Tree{}, secondary: map[string]*index.Tree{}}, nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.set != nil && t.idx.fileID == t.fileID {
		return t.idx.set, nil
	}
	if t.file == nil {
		return nil, fmt.Errorf("table %q is closed", t.Name)
	}
	set := t.loadSnapshot()
	if set == nil {
		var err error
		if set, err = t.buildIndexes(); err != nil {
			return nil, err
		}
	}
	t.idx.set, t.idx.fileID = set, t.fileID
	return set, nil
}

// indexes returns just the PRIMARY KEY/UNIQUE trees.
func (t *SqlTable) indexes() (map[int]*index.Tree, error) {
	set, err := t.indexSet()
	if err != nil {
		return nil, err
	}
	return set.unique, nil
}

// builtSet returns the trees if they are built for the current file, and nil
// otherwise. It never builds.
func (t *SqlTable) builtSet() *treeSet {
	if t.noIndexes {
		return nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.set != nil && t.idx.fileID == t.fileID {
		return t.idx.set
	}
	return nil
}

// builtIndexes returns the PRIMARY KEY/UNIQUE trees if built, and nil otherwise.
func (t *SqlTable) builtIndexes() map[int]*index.Tree {
	if set := t.builtSet(); set != nil {
		return set.unique
	}
	return nil
}

// dropIndexes discards the trees; the next indexSet call rebuilds them from the
// heap. Writers call it after any failure that may have left the file and the
// trees disagreeing, and a lookup calls it when an entry points at a row that
// is not there (possibly under only the read lock; see tableIndexes).
func (t *SqlTable) dropIndexes() {
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	t.idx.set = nil
}

// secondaryColumns returns the schema index of each CREATE INDEX definition's
// column, in the order of t.indexDefs; -1 for a column that is not there.
func (t *SqlTable) secondaryColumns() []int {
	cols := make([]int, len(t.indexDefs))
	for i, def := range t.indexDefs {
		cols[i] = slices.IndexFunc(t.Columns, func(c parser.ColumnDefinition) bool { return c.Name == def.column })
	}
	return cols
}

// trackedColumns are the columns whose values a write must remember to update
// the trees: the PRIMARY KEY/UNIQUE columns, then the indexed columns.
func (t *SqlTable) trackedColumns() []int {
	return append(t.keyColumns(), t.secondaryColumns()...)
}

// buildIndexes scans the file once and indexes every non-NULL PRIMARY
// KEY/UNIQUE value and every non-NULL value of an indexed column. A unique
// column whose values the tree cannot hold gets a nil tree: a key seen twice (an
// interrupted UPDATE can leave two rows with one key) or longer than maxKeyLen.
// A secondary index gets a nil tree only for a value too long to index.
// Duplicates are legal there. Any other error aborts the build.
func (t *SqlTable) buildIndexes() (*treeSet, error) {
	set := &treeSet{unique: map[int]*index.Tree{}, secondary: map[string]*index.Tree{}}
	for _, col := range t.keyColumns() {
		tree, err := index.New(index.DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		set.unique[col] = tree
	}
	secondaryCols := t.secondaryColumns()
	for i, def := range t.indexDefs {
		if secondaryCols[i] < 0 {
			set.secondary[def.name] = nil
			continue
		}
		tree, err := index.New(index.DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		set.secondary[def.name] = tree
	}
	if len(set.unique) == 0 && len(set.secondary) == 0 {
		return set, nil
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return nil, err
		}
		for col, tree := range set.unique {
			if tree == nil || row[col] == nil {
				continue
			}
			key, err := index.EncodeKey(row[col])
			if err != nil {
				return nil, err
			}
			if err := tree.Insert(key, fileRow.ID); err != nil {
				if !errors.Is(err, index.ErrDuplicateKey) && !errors.Is(err, index.ErrKeyTooLong) {
					return nil, err
				}
				set.unique[col] = nil
			}
		}
		for i, def := range t.indexDefs {
			tree, col := set.secondary[def.name], secondaryCols[i]
			if tree == nil || col < 0 || row[col] == nil {
				continue
			}
			key, err := index.EncodeSecondaryKey(row[col], fileRow.ID)
			if err != nil {
				return nil, err
			}
			if err := tree.Insert(key, fileRow.ID); err != nil {
				if !errors.Is(err, index.ErrDuplicateKey) && !errors.Is(err, index.ErrKeyTooLong) {
					return nil, err
				}
				set.secondary[def.name] = nil
			}
		}
	}
	return set, nil
}

// indexInserted records rows just written to the file at ids. A built index
// that cannot take an entry is discarded, never left half updated.
func (t *SqlTable) indexInserted(ids []storage.RowID, rows [][]any) {
	set := t.builtSet()
	if set == nil {
		return
	}
	if !t.addEntries(set, rows, ids) {
		t.dropIndexes()
	}
}

// addEntries adds each row's entries to every tree and reports whether they all
// went in.
func (t *SqlTable) addEntries(set *treeSet, rows [][]any, ids []storage.RowID) bool {
	for col, tree := range set.unique {
		if tree == nil {
			continue
		}
		for i, row := range rows {
			if row[col] == nil {
				continue
			}
			key, err := index.EncodeKey(row[col])
			if err == nil {
				err = tree.Insert(key, ids[i])
			}
			if err != nil {
				return false
			}
		}
	}
	for i, col := range t.secondaryColumns() {
		tree := set.secondary[t.indexDefs[i].name]
		if tree == nil || col < 0 {
			continue
		}
		for r, row := range rows {
			if row[col] == nil {
				continue
			}
			key, err := index.EncodeSecondaryKey(row[col], ids[r])
			if err == nil {
				err = tree.Insert(key, ids[r])
			}
			if err != nil {
				return false
			}
		}
	}
	return true
}

// indexDeleted removes rows just tombstoned. keys[i] holds row i's values at
// trackedColumns, ids[i] is its old RowID. An entry that is missing means the
// index was wrong, so it is discarded.
func (t *SqlTable) indexDeleted(keys [][]any, ids []storage.RowID) {
	set := t.builtSet()
	if set == nil {
		return
	}
	if !t.removeKeys(set, keys, ids) {
		t.dropIndexes()
	}
}

// indexMoved records an UPDATE: every updated row got a new RowID, so its old
// entries are removed first and the new values are added at the new ids.
// Unchanged values are re-pointed too, for a secondary entry ends in the RowID.
func (t *SqlTable) indexMoved(oldKeys [][]any, oldIDs []storage.RowID, rows [][]any, newIDs []storage.RowID) {
	set := t.builtSet()
	if set == nil {
		return
	}
	if !t.removeKeys(set, oldKeys, oldIDs) || !t.addEntries(set, rows, newIDs) {
		t.dropIndexes()
	}
}

// removeKeys deletes each row's entries from the trees and reports whether every
// entry was there.
func (t *SqlTable) removeKeys(set *treeSet, keys [][]any, ids []storage.RowID) bool {
	unique := t.keyColumns()
	for j, col := range unique {
		tree := set.unique[col]
		if tree == nil {
			continue
		}
		for _, values := range keys {
			if values[j] == nil {
				continue
			}
			key, err := index.EncodeKey(values[j])
			if err != nil || !tree.Delete(key) {
				return false
			}
		}
	}
	for i, col := range t.secondaryColumns() {
		tree := set.secondary[t.indexDefs[i].name]
		if tree == nil || col < 0 {
			continue
		}
		for r, values := range keys {
			value := values[len(unique)+i]
			if value == nil {
				continue
			}
			key, err := index.EncodeSecondaryKey(value, ids[r])
			if err != nil || !tree.Delete(key) {
				return false
			}
		}
	}
	return true
}
