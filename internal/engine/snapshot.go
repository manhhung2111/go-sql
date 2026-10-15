package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"manhhung2111/go-sql/internal/index"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/internal/storage"
)

// These are variables so a test can make the removal of a snapshot fail.
var (
	removeSnapshotFile = os.Remove
	syncSnapshotDir    = storage.SyncDir
)

// snapshotPathFor is where the snapshot of the heap file at heapPath lives.
func snapshotPathFor(heapPath string) string {
	return strings.TrimSuffix(heapPath, tableFileExt) + snapshotExt
}

func (t *SqlTable) snapshotPath() string { return snapshotPathFor(t.path) }

// treeIDs names the trees the table has now, in the order of keyColumns and then
// indexDefs.
func (t *SqlTable) treeIDs() []index.TreeID {
	var ids []index.TreeID
	for _, col := range t.keyColumns() {
		ids = append(ids, index.TreeID{Kind: index.UniqueTree, Ordinal: col, Column: t.Columns[col].Name})
	}
	for _, def := range t.indexDefs {
		// The ordinal pins the column: two columns can swap names, and a tree
		// of the old column must not load as the tree of the new one.
		ids = append(ids, index.TreeID{Kind: index.SecondaryTree, Ordinal: max(slices.IndexFunc(t.Columns, func(c parser.ColumnDefinition) bool { return c.Name == def.column }), 0), Name: def.name, Column: def.column})
	}
	return ids
}

// loadSnapshot returns the trees saved by an earlier run, or nil if there is no
// usable snapshot. Anything wrong with the file removes it and returns nil: the
// caller builds the trees from the heap. The table still remembers that a
// snapshot may exist, because this removal is not made durable; the first heap
// write does that (see prepareHeapWrite). Callers hold t.mu.
func (t *SqlTable) loadSnapshot() *treeSet {
	if !t.snapshotMayExist || t.file == nil || t.idx.distrust {
		return nil
	}
	ids := t.treeIDs()
	if len(ids) == 0 {
		return nil
	}
	path := t.snapshotPath()
	saved, err := index.ReadSnapshot(path, t.fileID, t.file.NumPages(), ids)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			_ = removeSnapshotFile(path)
		}
		return nil
	}
	set := &treeSet{unique: map[int]*index.Tree{}, secondary: map[string]*index.Tree{}}
	for _, s := range saved {
		if s.ID.Kind == index.UniqueTree {
			set.unique[s.ID.Ordinal] = s.Tree
		} else {
			set.secondary[s.ID.Name] = s.Tree
		}
	}
	return set
}

// saveSnapshot writes the built trees next to the heap. It does nothing when
// they are not built: the file on disk, if any, is still what it was. A failure
// is ignored, since the snapshot is only a cache. Callers hold t.mu.
func (t *SqlTable) saveSnapshot() {
	if t.file == nil {
		return
	}
	set := t.builtSet()
	ids := t.treeIDs()
	if set == nil || len(ids) == 0 || t.distrusted() {
		return
	}
	trees := make([]index.SnapshotTree, len(ids))
	for i, id := range ids {
		trees[i].ID = id
		if id.Kind == index.UniqueTree {
			trees[i].Tree = set.unique[id.Ordinal]
		} else {
			trees[i].Tree = set.secondary[id.Name]
		}
	}
	_ = index.WriteSnapshot(t.snapshotPath(), t.fileID, t.file.NumPages(), trees)
}

// prepareHeapWrite makes sure no snapshot of the heap file can survive a write
// to it: the file is removed, and the removal made durable, before the first
// write after open. A crash after the write then finds no snapshot, which is a
// rebuild; without this it would find one that no longer matches. It is a
// no-op once done, and for tables that cannot have one. When it fails nothing
// has been written and the statement must fail. Callers hold t.mu for writing.
func (t *SqlTable) prepareHeapWrite() error {
	if !t.snapshotMayExist {
		return nil
	}
	path := t.snapshotPath()
	if err := removeSnapshotFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the index snapshot of table %q: %w", t.Name, err)
	}
	if err := syncSnapshotDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("removing the index snapshot of table %q: %w", t.Name, err)
	}
	t.snapshotMayExist = false
	return nil
}

// discardSnapshotFiles removes the snapshot and any half-written one of the heap
// file at heapPath. It is best effort: a file left behind is an orphan that
// startup removes, and never one that can be read, because the heap file it
// belongs to is gone or has a new id.
func (t *SqlTable) discardSnapshotFiles(heapPath string) {
	path := snapshotPathFor(heapPath)
	_ = removeSnapshotFile(path)
	_ = removeSnapshotFile(path + ".tmp")
}

func (t *SqlTable) distrusted() bool {
	t.idx.mu.Lock()
	defer t.idx.mu.Unlock()
	return t.idx.distrust
}
