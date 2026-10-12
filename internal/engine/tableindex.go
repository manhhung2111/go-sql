package engine

import (
	"errors"
	"fmt"
	"sync"

	"manhhung2111/go-sql/internal/index"
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
	// trees maps a PRIMARY KEY/UNIQUE column's schema index to its tree. A nil
	// map means "not built"; a nil tree means the column cannot be indexed
	// (see buildIndexes) and its checks scan the file instead.
	trees map[int]*index.Tree
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

// keyValues returns row's values at the key columns, in keyColumns order.
func (t *SqlTable) keyValues(row []any) []any {
	cols := t.keyColumns()
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

// indexes returns the table's trees, building them from one scan of the file
// if they are missing or were built from another file. Callers hold t.mu, for
// reading or writing. With noIndexes set it returns no trees, so every column
// takes its scan path.
func (t *SqlTable) indexes() (map[int]*index.Tree, error) {
	if t.noIndexes {
		return map[int]*index.Tree{}, nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.trees != nil && t.idx.fileID == t.fileID {
		return t.idx.trees, nil
	}
	if t.file == nil {
		return nil, fmt.Errorf("table %q is closed", t.Name)
	}
	trees, err := t.buildIndexes()
	if err != nil {
		return nil, err
	}
	t.idx.trees, t.idx.fileID = trees, t.fileID
	return trees, nil
}

// builtIndexes returns the trees if they are built for the current file, and
// nil otherwise. It never builds.
func (t *SqlTable) builtIndexes() map[int]*index.Tree {
	if t.noIndexes {
		return nil
	}
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	if t.idx.trees != nil && t.idx.fileID == t.fileID {
		return t.idx.trees
	}
	return nil
}

// dropIndexes discards the trees; the next indexes call rebuilds them from the
// heap. Writers call it after any failure that may have left the file and the
// trees disagreeing.
func (t *SqlTable) dropIndexes() {
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	t.idx.trees = nil
}

// buildIndexes scans the file once and indexes every non-NULL PRIMARY
// KEY/UNIQUE value. A column whose values the tree cannot hold gets a nil tree
// and keeps using its scan path: a key seen twice (an interrupted UPDATE can
// leave two rows with one key) or longer than maxKeyLen (written before such
// values were rejected). Any other error aborts the build.
func (t *SqlTable) buildIndexes() (map[int]*index.Tree, error) {
	trees := make(map[int]*index.Tree)
	for _, col := range t.keyColumns() {
		tree, err := index.New(index.DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		trees[col] = tree
	}
	if len(trees) == 0 {
		return trees, nil
	}

	for fileRow, err := range t.file.Scan() {
		if err != nil {
			return nil, err
		}
		row, err := DecodeRow(t.Columns, fileRow.Bytes)
		if err != nil {
			return nil, err
		}
		for col, tree := range trees {
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
				trees[col] = nil
			}
		}
	}
	return trees, nil
}
