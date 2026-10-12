package index

import (
	"bytes"
	"errors"
	"fmt"
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
	leaf := t.root // the root stays a leaf until Task 2 adds splits
	i, found := leaf.search(key)
	if found {
		return fmt.Errorf("key %x already exists: %w", key, ErrDuplicateKey)
	}
	key = slices.Clone(key)
	leaf.keys = insertAt(leaf.keys, i, key)
	leaf.ids = insertAt(leaf.ids, i, id)
	leaf.used += leafEntrySize(key)
	t.size++
	return nil
}
