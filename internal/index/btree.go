package index

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sort"

	"manhhung2111/go-sql/internal/storage"
)

// Sizes are what a node would occupy if serialised into a page. A node's
// budget is a byte count, not an entry count, because keys vary in length.
const (
	// DefaultNodeBytes is one storage page.
	DefaultNodeBytes = 16 * 1024

	// nodeHeaderSize covers the node kind, entry count and the next-leaf id
	// (leaf) or leftmost child id (internal).
	nodeHeaderSize = 16
	// keyLenSize prefixes every key.
	keyLenSize = 2
	// rowIDSize is the page number (int32) plus the slot (uint16). A slot
	// always fits in 2 bytes: a 16KiB page cannot hold 65536 slots.
	rowIDSize = 4 + 2
	// childIDSize is the id of the child to the right of a separator.
	childIDSize = 4
	// minEntriesPerNode is how many maximum-size entries a node must hold,
	// which bounds the key length.
	minEntriesPerNode = 4
)

var (
	// ErrDuplicateKey is returned by Insert when the key is already present.
	ErrDuplicateKey = errors.New("duplicate key")
	// ErrKeyTooLong is returned by Insert when the key exceeds MaxKeyLen.
	ErrKeyTooLong = errors.New("key too long")
)

func leafEntrySize(key []byte) int { return keyLenSize + len(key) + rowIDSize }
func sepEntrySize(key []byte) int  { return keyLenSize + len(key) + childIDSize }

// node is a leaf (keys with row ids, chained through next) or an internal
// node (separators with len(keys)+1 children). A separator S divides its left
// subtree (keys < S) from its right subtree (keys >= S); it need not be the
// key of a live row.
type node struct {
	leaf     bool
	keys     [][]byte
	ids      []storage.RowID // leaf only, parallel to keys
	children []*node         // internal only, len(keys)+1
	next     *node           // leaf only: the next leaf to the right
	used     int             // serialised size in bytes, header included
}

func newLeaf() *node { return &node{leaf: true, used: nodeHeaderSize} }

// recount recomputes used from the node's contents.
func (n *node) recount() {
	n.used = nodeHeaderSize
	for i := range n.keys {
		n.used += n.entrySize(i)
	}
}

func (n *node) entrySize(i int) int {
	if n.leaf {
		return leafEntrySize(n.keys[i])
	}
	return sepEntrySize(n.keys[i])
}

// search returns the index of the first key >= key and whether it equals key.
func (n *node) search(key []byte) (int, bool) {
	i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) >= 0 })
	return i, i < len(n.keys) && bytes.Equal(n.keys[i], key)
}

// childIndex returns the child that may hold key: the number of separators
// that are <= key.
func (n *node) childIndex(key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) > 0 })
}

func insertAt[T any](s []T, i int, v T) []T {
	var zero T
	s = append(s, zero)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// Tree is an in-memory B+ tree from unique byte-string keys to row ids. It
// has no locking; the owner serialises access.
type Tree struct {
	root      *node
	nodeBytes int
	maxKey    int
	size      int
}

// New returns an empty tree whose nodes hold at most nodeBytes bytes. It fails
// if nodeBytes is too small to hold a key of even one byte.
func New(nodeBytes int) (*Tree, error) {
	maxKey := (nodeBytes-nodeHeaderSize)/minEntriesPerNode - (keyLenSize + rowIDSize)
	if maxKey < 1 {
		return nil, fmt.Errorf("node size of %d bytes is too small", nodeBytes)
	}
	return &Tree{root: newLeaf(), nodeBytes: nodeBytes, maxKey: maxKey}, nil
}

// MaxKeyLen is the longest key Insert accepts.
func (t *Tree) MaxKeyLen() int { return t.maxKey }

// Len is the number of keys in the tree.
func (t *Tree) Len() int { return t.size }

// Get returns the row id stored under key.
func (t *Tree) Get(key []byte) (storage.RowID, bool) {
	n := t.root
	for !n.leaf {
		n = n.children[n.childIndex(key)]
	}
	if i, found := n.search(key); found {
		return n.ids[i], true
	}
	return storage.RowID{}, false
}

// Insert adds key. It fails, leaving the tree unchanged, if the key is
// already present or longer than MaxKeyLen. The tree keeps its own copy of key.
func (t *Tree) Insert(key []byte, id storage.RowID) error {
	if len(key) > t.maxKey {
		return fmt.Errorf("key of %d bytes exceeds the maximum of %d: %w", len(key), t.maxKey, ErrKeyTooLong)
	}
	sep, right, err := t.insert(t.root, slices.Clone(key), id)
	if err != nil {
		return err
	}
	if right != nil {
		t.growRoot(sep, right)
	}
	t.size++
	return nil
}

// growRoot makes a new root over the old root and right, which it was split from.
func (t *Tree) growRoot(sep []byte, right *node) {
	root := &node{keys: [][]byte{sep}, children: []*node{t.root, right}}
	root.recount()
	t.root = root
}

// insert adds key below n. If n overflows and splits, it returns the separator
// to add to n's parent and the new right sibling.
func (t *Tree) insert(n *node, key []byte, id storage.RowID) ([]byte, *node, error) {
	if n.leaf {
		i, found := n.search(key)
		if found {
			return nil, nil, fmt.Errorf("key %x already exists: %w", key, ErrDuplicateKey)
		}
		n.keys = insertAt(n.keys, i, key)
		n.ids = insertAt(n.ids, i, id)
		n.used += leafEntrySize(key)
		if n.used <= t.nodeBytes {
			return nil, nil, nil
		}
		sep, right := t.splitLeaf(n)
		return sep, right, nil
	}

	ci := n.childIndex(key)
	sep, right, err := t.insert(n.children[ci], key, id)
	if err != nil || right == nil {
		return nil, nil, err
	}
	n.adoptSplit(ci, sep, right)
	if n.used <= t.nodeBytes {
		return nil, nil, nil
	}
	sep, right = t.splitInternal(n)
	return sep, right, nil
}

// adoptSplit records that child ci split, giving sep and its new right sibling.
func (n *node) adoptSplit(ci int, sep []byte, right *node) {
	n.keys = insertAt(n.keys, ci, sep)
	n.children = insertAt(n.children, ci+1, right)
	n.used += sepEntrySize(sep)
}

// splitPoint returns the first index at which the entries before it hold at
// least half of n's entry bytes, clamped to [lo, hi].
func (n *node) splitPoint(lo, hi int) int {
	total := n.used - nodeHeaderSize
	acc := 0
	for i := 0; i < hi; i++ {
		acc += n.entrySize(i)
		if acc*2 >= total {
			return max(i+1, lo)
		}
	}
	return hi
}

// splitLeaf moves the upper entries of n to a new right leaf and returns the
// separator (a copy of the right leaf's first key) with that leaf.
func (t *Tree) splitLeaf(n *node) ([]byte, *node) {
	at := n.splitPoint(1, len(n.keys)-1)
	right := newLeaf()
	right.keys = slices.Clone(n.keys[at:])
	right.ids = slices.Clone(n.ids[at:])
	right.next = n.next
	clear(n.keys[at:])
	n.keys, n.ids = n.keys[:at], n.ids[:at]
	n.next = right
	n.recount()
	right.recount()
	return right.keys[0], right
}

// splitInternal moves the upper separators of n to a new right node. The
// middle separator moves up to the parent and stays in neither half.
func (t *Tree) splitInternal(n *node) ([]byte, *node) {
	at := n.splitPoint(1, len(n.keys)-2)
	sep := n.keys[at]
	right := &node{
		keys:     slices.Clone(n.keys[at+1:]),
		children: slices.Clone(n.children[at+1:]),
	}
	clear(n.keys[at:])
	clear(n.children[at+1:])
	n.keys, n.children = n.keys[:at], n.children[:at+1]
	n.recount()
	right.recount()
	return sep, right
}

// Range yields the keys in [lo, hi) in order. A nil lo starts at the first
// key and a nil hi runs to the last; a non-nil empty hi is an empty range.
// Yielded keys are valid until the tree is next modified. The tree must not be
// modified while iterating.
func (t *Tree) Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID] {
	return func(yield func([]byte, storage.RowID) bool) {
		n := t.root
		for !n.leaf {
			n = n.children[n.childIndex(lo)]
		}
		i, _ := n.search(lo)
		for ; n != nil; n, i = n.next, 0 {
			for ; i < len(n.keys); i++ {
				if hi != nil && bytes.Compare(n.keys[i], hi) >= 0 {
					return
				}
				if !yield(n.keys[i], n.ids[i]) {
					return
				}
			}
		}
	}
}
