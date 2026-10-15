package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"manhhung2111/go-sql/internal/storage"
)

// A snapshot is a storage.PageFile holding a table's trees as sorted entries.
// Page 0 is the header, after the page file's own header:
//
//	magic "GSIX" (4) | version u16 | heap file id i64 | heap pages u32 |
//	tree count u16 | one descriptor per tree
//
// A descriptor is: kind u8 | ordinal u32 | degraded u8 | entry count u32 |
// first page u32 | name (u16 length, bytes) | column (u16 length, bytes).
//
// Entry pages follow, the trees one after another in descriptor order. An entry
// page is: count u16, then that many entries of key (u16 length, bytes) | row
// page u32 | row slot u16. All integers are little-endian.
const (
	snapshotMagic   = "GSIX"
	snapshotVersion = 1

	hdrMagic       = 0
	hdrVersion     = 4
	hdrHeapID      = 6
	hdrHeapPages   = 14
	hdrTreeCount   = 18
	hdrDescriptors = 20
)

// TreeKind says which of a table's trees a snapshot tree is.
type TreeKind uint8

const (
	// UniqueTree is the tree of a PRIMARY KEY/UNIQUE column.
	UniqueTree TreeKind = iota + 1
	// SecondaryTree is the tree of a CREATE INDEX definition.
	SecondaryTree
)

// TreeID names a tree of a table. Two snapshots hold the same trees exactly when
// their IDs are equal as a set.
type TreeID struct {
	Kind    TreeKind
	Ordinal int    // UniqueTree: the column's schema index
	Name    string // SecondaryTree: the index name
	Column  string // the column's name
}

// SnapshotTree is a tree and its identity. A nil Tree is a degraded one: a
// column or index that could not hold the heap's values.
type SnapshotTree struct {
	ID   TreeID
	Tree *Tree
}

var (
	// ErrSnapshotMismatch means the file is a valid snapshot of something else:
	// another heap file, another page count or another set of trees.
	ErrSnapshotMismatch = errors.New("snapshot does not match")
	// ErrSnapshotTooLarge means the descriptors do not fit the header page.
	ErrSnapshotTooLarge = errors.New("snapshot header too large")
)

// WriteSnapshot saves trees to path for the heap file heapID of heapPages
// pages. It writes path+".tmp", fsyncs it, renames it over path and fsyncs the
// directory, so path is either what it was or complete.
func WriteSnapshot(path string, heapID int64, heapPages int32, trees []SnapshotTree) (err error) {
	tmp := path + ".tmp"
	if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return rmErr
	}
	pf, err := storage.CreatePageFile(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = pf.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = pf.AllocatePage(); err != nil { // page 0, rewritten last
		return err
	}

	header := make([]byte, storage.PageSize)
	payload := header[storage.PageHeaderSize:]
	copy(payload[hdrMagic:], snapshotMagic)
	binary.LittleEndian.PutUint16(payload[hdrVersion:], snapshotVersion)
	binary.LittleEndian.PutUint64(payload[hdrHeapID:], uint64(heapID))
	binary.LittleEndian.PutUint32(payload[hdrHeapPages:], uint32(heapPages))
	binary.LittleEndian.PutUint16(payload[hdrTreeCount:], uint16(len(trees)))
	pos := hdrDescriptors
	for _, st := range trees {
		first, count, werr := writeEntries(pf, st.Tree)
		if werr != nil {
			return werr
		}
		need := 1 + 4 + 1 + 4 + 4 + 2 + len(st.ID.Name) + 2 + len(st.ID.Column)
		if pos+need > len(payload) || len(trees) > 1<<16-1 {
			return ErrSnapshotTooLarge
		}
		payload[pos] = byte(st.ID.Kind)
		binary.LittleEndian.PutUint32(payload[pos+1:], uint32(st.ID.Ordinal))
		if st.Tree == nil {
			payload[pos+5] = 1
		}
		binary.LittleEndian.PutUint32(payload[pos+6:], uint32(count))
		binary.LittleEndian.PutUint32(payload[pos+10:], uint32(first))
		pos += 14
		pos = putString(payload, pos, st.ID.Name)
		pos = putString(payload, pos, st.ID.Column)
	}
	if err = pf.WritePage(0, header); err != nil {
		return err
	}
	if err = pf.Sync(); err != nil {
		return err
	}
	if err = pf.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return storage.SyncDir(filepath.Dir(path))
}

func putString(b []byte, pos int, s string) int {
	binary.LittleEndian.PutUint16(b[pos:], uint16(len(s)))
	return pos + 2 + copy(b[pos+2:], s)
}

// writeEntries appends tree's entries to pf as entry pages and returns the page
// number of the first and the number of entries. A nil or empty tree writes
// no page.
func writeEntries(pf storage.PageFile, tree *Tree) (first int32, count int, err error) {
	if tree == nil {
		return 0, 0, nil
	}
	buf := make([]byte, storage.PageSize)
	pos, inPage := storage.PageHeaderSize+2, 0
	flush := func() error {
		if inPage == 0 {
			return nil
		}
		binary.LittleEndian.PutUint16(buf[storage.PageHeaderSize:], uint16(inPage))
		n := pf.NumPages()
		if first == 0 {
			first = n
		}
		if err := pf.WritePage(n, buf); err != nil {
			return err
		}
		clear(buf)
		pos, inPage = storage.PageHeaderSize+2, 0
		return nil
	}
	for key, id := range tree.Range(nil, nil) {
		if pos+2+len(key)+4+2 > len(buf) {
			if err := flush(); err != nil {
				return 0, 0, err
			}
		}
		binary.LittleEndian.PutUint16(buf[pos:], uint16(len(key)))
		pos += 2
		pos += copy(buf[pos:], key)
		binary.LittleEndian.PutUint32(buf[pos:], uint32(id.Page))
		binary.LittleEndian.PutUint16(buf[pos+4:], uint16(id.Slot))
		pos += 6
		inPage++
		count++
	}
	if err := flush(); err != nil {
		return 0, 0, err
	}
	return first, count, nil
}

// reader reads little-endian values from a page, remembering the first time it
// ran past the end.
type reader struct {
	b   []byte
	pos int
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || n < 0 || r.pos+n > len(r.b) {
		r.bad = true
		return nil
	}
	out := r.b[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *reader) u8() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() int {
	if b := r.take(2); b != nil {
		return int(binary.LittleEndian.Uint16(b))
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *reader) str() string { return string(r.take(r.u16())) }

type descriptor struct {
	id       TreeID
	degraded bool
	count    uint32
	first    uint32
}

// ReadSnapshot loads the trees of path, which must be a snapshot of heap file
// heapID with heapPages pages holding exactly the trees want names. It returns
// them in want's order. A damaged file is an error; a valid snapshot of
// something else wraps ErrSnapshotMismatch.
func ReadSnapshot(path string, heapID int64, heapPages int32, want []TreeID) ([]SnapshotTree, error) {
	pf, err := storage.OpenPageFile(path)
	if err != nil {
		return nil, err
	}
	defer pf.Close()
	bad := func(format string, args ...any) error {
		return fmt.Errorf("snapshot %q: %s", path, fmt.Sprintf(format, args...))
	}
	if pf.NumPages() < 1 {
		return nil, bad("no header page")
	}
	page, err := pf.ReadPage(0)
	if err != nil {
		return nil, err
	}
	hdr := page[storage.PageHeaderSize:]
	if string(hdr[hdrMagic:hdrMagic+4]) != snapshotMagic {
		return nil, bad("bad magic")
	}
	if v := binary.LittleEndian.Uint16(hdr[hdrVersion:]); v != snapshotVersion {
		return nil, bad("unsupported version %d", v)
	}
	if got := int64(binary.LittleEndian.Uint64(hdr[hdrHeapID:])); got != heapID {
		return nil, fmt.Errorf("%w: it is for heap file %d, not %d", ErrSnapshotMismatch, got, heapID)
	}
	if got := int32(binary.LittleEndian.Uint32(hdr[hdrHeapPages:])); got != heapPages {
		return nil, fmt.Errorf("%w: the heap had %d pages, it has %d", ErrSnapshotMismatch, got, heapPages)
	}
	n := int(binary.LittleEndian.Uint16(hdr[hdrTreeCount:]))
	if n != len(want) {
		return nil, fmt.Errorf("%w: %d trees, want %d", ErrSnapshotMismatch, n, len(want))
	}

	r := reader{b: hdr, pos: hdrDescriptors}
	descs := make([]descriptor, n)
	wanted := make(map[TreeID]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}
	seen := make(map[TreeID]bool, n)
	for i := range descs {
		d := &descs[i]
		d.id.Kind = TreeKind(r.u8())
		d.id.Ordinal = int(r.u32())
		d.degraded = r.u8() == 1
		d.count = r.u32()
		d.first = r.u32()
		d.id.Name = r.str()
		d.id.Column = r.str()
		if r.bad {
			return nil, bad("damaged header")
		}
		if !wanted[d.id] || seen[d.id] {
			return nil, fmt.Errorf("%w: tree %+v", ErrSnapshotMismatch, d.id)
		}
		seen[d.id] = true
	}

	trees := make(map[TreeID]*Tree, n)
	next := uint32(1)
	for _, d := range descs {
		if d.degraded {
			if d.count != 0 {
				return nil, bad("a degraded tree with entries")
			}
			trees[d.id] = nil
			continue
		}
		tree, err := New(DefaultNodeBytes)
		if err != nil {
			return nil, err
		}
		trees[d.id] = tree
		if d.count == 0 {
			continue
		}
		if d.first != next {
			return nil, bad("tree %+v starts at page %d, want %d", d.id, d.first, next)
		}
		remaining := int(d.count)
		var prev []byte
		havePrev := false
		for remaining > 0 {
			if int32(next) >= pf.NumPages() {
				return nil, bad("entries run past the end")
			}
			buf, err := pf.ReadPage(int32(next))
			if err != nil {
				return nil, err
			}
			next++
			er := reader{b: buf, pos: storage.PageHeaderSize}
			k := er.u16()
			if k == 0 || k > remaining {
				return nil, bad("page %d holds %d entries, %d remain", next-1, k, remaining)
			}
			for i := 0; i < k; i++ {
				key := er.take(er.u16())
				pg, sl := er.u32(), er.u16()
				if er.bad {
					return nil, bad("damaged entry page %d", next-1)
				}
				if havePrev && bytes.Compare(prev, key) >= 0 {
					return nil, bad("entries out of order on page %d", next-1)
				}
				if err := tree.Insert(key, storage.RowID{Page: int32(pg), Slot: sl}); err != nil {
					return nil, bad("page %d: %v", next-1, err)
				}
				prev, havePrev = key, true
			}
			remaining -= k
		}
	}
	if int32(next) != pf.NumPages() {
		return nil, bad("%d trailing pages", pf.NumPages()-int32(next))
	}

	out := make([]SnapshotTree, len(want))
	for i, w := range want {
		out[i] = SnapshotTree{ID: w, Tree: trees[w]}
	}
	return out, nil
}
