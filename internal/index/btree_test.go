package index

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

// testNodeBytes is small enough that a handful of short keys split a node and
// MaxKeyLen is 12.
const testNodeBytes = 96

func newTestTree(t *testing.T) *Tree {
	t.Helper()
	tree, err := New(testNodeBytes)
	require.NoError(t, err)
	return tree
}

func rid(n int) storage.RowID { return storage.RowID{Page: int32(n / 100), Slot: n % 100} }

// check verifies every structural invariant of the tree and fails the test on
// the first violation.
func check(t *testing.T, tree *Tree) {
	t.Helper()
	var leaves []*node
	depth := -1
	count := 0

	var walk func(n *node, lo, hi []byte, level int)
	walk = func(n *node, lo, hi []byte, level int) {
		t.Helper()
		isRoot := n == tree.root
		size := nodeHeaderSize
		for i := range n.keys {
			size += n.entrySize(i)
			if i > 0 && bytes.Compare(n.keys[i-1], n.keys[i]) >= 0 {
				t.Fatalf("keys out of order in a node: %x then %x", n.keys[i-1], n.keys[i])
			}
			if lo != nil && bytes.Compare(n.keys[i], lo) < 0 {
				t.Fatalf("key %x below its subtree's lower separator %x", n.keys[i], lo)
			}
			if hi != nil && bytes.Compare(n.keys[i], hi) >= 0 {
				t.Fatalf("key %x not below its subtree's upper separator %x", n.keys[i], hi)
			}
		}
		if size != n.used {
			t.Fatalf("used is %d but the node's contents add up to %d", n.used, size)
		}
		if n.used > tree.nodeBytes {
			t.Fatalf("node uses %d bytes, over the budget of %d", n.used, tree.nodeBytes)
		}

		if n.leaf {
			require.Len(t, n.ids, len(n.keys))
			require.Empty(t, n.children)
			if !isRoot {
				require.NotEmpty(t, n.keys, "empty non-root leaf")
			}
			if depth == -1 {
				depth = level
			}
			require.Equal(t, depth, level, "leaves at different depths")
			leaves = append(leaves, n)
			count += len(n.keys)
			return
		}
		require.Empty(t, n.ids)
		require.Len(t, n.children, len(n.keys)+1)
		if !isRoot {
			require.NotEmpty(t, n.keys, "non-root internal node without a separator")
		}
		for i, child := range n.children {
			cLo, cHi := lo, hi
			if i > 0 {
				cLo = n.keys[i-1]
			}
			if i < len(n.keys) {
				cHi = n.keys[i]
			}
			walk(child, cLo, cHi, level+1)
		}
	}
	walk(tree.root, nil, nil, 0)

	require.Equal(t, tree.size, count, "Len does not match the number of leaf entries")
	for i, leaf := range leaves {
		var want *node
		if i+1 < len(leaves) {
			want = leaves[i+1]
		}
		require.Same(t, want, leaf.next, "leaf chain does not follow key order")
	}
}

func key(n int) []byte { return []byte(fmt.Sprintf("k%05d", n)) }

func TestNew_RejectsTooSmallNode(t *testing.T) {
	_, err := New(40)
	require.Error(t, err)
	assert.EqualError(t, err, "node size of 40 bytes is too small")

	tree, err := New(DefaultNodeBytes)
	require.NoError(t, err)
	assert.Equal(t, 4084, tree.MaxKeyLen())

	small := newTestTree(t)
	assert.Equal(t, 12, small.MaxKeyLen())
}

func TestTree_InsertAndGetInOneLeaf(t *testing.T) {
	tree := newTestTree(t)
	_, found := tree.Get([]byte("a"))
	assert.False(t, found)

	require.NoError(t, tree.Insert([]byte("b"), rid(2)))
	require.NoError(t, tree.Insert([]byte("a"), rid(1)))
	assert.Equal(t, 2, tree.Len())

	got, found := tree.Get([]byte("a"))
	assert.True(t, found)
	assert.Equal(t, rid(1), got)
	got, found = tree.Get([]byte("b"))
	assert.True(t, found)
	assert.Equal(t, rid(2), got)
	_, found = tree.Get([]byte("c"))
	assert.False(t, found)
	check(t, tree)
}

func TestTree_InsertRejectsDuplicateAndLongKeysWithoutChange(t *testing.T) {
	tree := newTestTree(t)
	require.NoError(t, tree.Insert([]byte("a"), rid(1)))

	err := tree.Insert([]byte("a"), rid(2))
	require.ErrorIs(t, err, ErrDuplicateKey)
	got, _ := tree.Get([]byte("a"))
	assert.Equal(t, rid(1), got, "a duplicate insert must not overwrite")

	long := bytes.Repeat([]byte("x"), tree.MaxKeyLen()+1)
	err = tree.Insert(long, rid(3))
	require.ErrorIs(t, err, ErrKeyTooLong)
	assert.EqualError(t, err, "key of 13 bytes exceeds the maximum of 12: key too long")

	assert.NoError(t, tree.Insert(bytes.Repeat([]byte("x"), tree.MaxKeyLen()), rid(4)))
	assert.Equal(t, 2, tree.Len())
	check(t, tree)
}

func TestTree_InsertCopiesTheKey(t *testing.T) {
	tree := newTestTree(t)
	k := []byte("abc")
	require.NoError(t, tree.Insert(k, rid(1)))
	k[0] = 'X'
	_, found := tree.Get([]byte("abc"))
	assert.True(t, found)
}

func TestTree_EmptyKeyIsValid(t *testing.T) {
	tree := newTestTree(t)
	require.NoError(t, tree.Insert([]byte{}, rid(1)))
	require.NoError(t, tree.Insert([]byte("a"), rid(2)))
	got, found := tree.Get(nil)
	assert.True(t, found)
	assert.Equal(t, rid(1), got)
	check(t, tree)
}

func TestTree_BulkInsertSplitsAndKeepsEveryKey(t *testing.T) {
	orders := map[string]func(i, n int) int{
		"ascending":  func(i, n int) int { return i },
		"descending": func(i, n int) int { return n - 1 - i },
		"shuffled": func() func(i, n int) int {
			perm := rand.New(rand.NewSource(7)).Perm(500)
			return func(i, n int) int { return perm[i] }
		}(),
	}
	for name, at := range orders {
		t.Run(name, func(t *testing.T) {
			const n = 500
			tree := newTestTree(t)
			for i := 0; i < n; i++ {
				require.NoError(t, tree.Insert(key(at(i, n)), rid(at(i, n))))
				check(t, tree)
			}
			assert.Equal(t, n, tree.Len())
			assert.False(t, tree.root.leaf, "500 keys must not fit in one 96-byte leaf")
			for i := 0; i < n; i++ {
				got, found := tree.Get(key(i))
				require.True(t, found, "key %d missing", i)
				assert.Equal(t, rid(i), got)
			}
		})
	}
}

// model is the reference implementation the property test compares against.
type model struct {
	keys [][]byte
	ids  []storage.RowID
}

func (m *model) find(key []byte) (int, bool) {
	i := sort.Search(len(m.keys), func(i int) bool { return bytes.Compare(m.keys[i], key) >= 0 })
	return i, i < len(m.keys) && bytes.Equal(m.keys[i], key)
}

func (m *model) insert(key []byte, id storage.RowID) bool {
	i, found := m.find(key)
	if found {
		return false
	}
	m.keys = slices.Insert(m.keys, i, slices.Clone(key))
	m.ids = slices.Insert(m.ids, i, id)
	return true
}

func (m *model) delete(key []byte) bool {
	i, found := m.find(key)
	if !found {
		return false
	}
	m.keys = slices.Delete(m.keys, i, i+1)
	m.ids = slices.Delete(m.ids, i, i+1)
	return true
}

func (m *model) rangeOf(lo, hi []byte) (keys [][]byte, ids []storage.RowID) {
	for i, k := range m.keys {
		if lo != nil && bytes.Compare(k, lo) < 0 {
			continue
		}
		if hi != nil && bytes.Compare(k, hi) >= 0 {
			continue
		}
		keys, ids = append(keys, k), append(ids, m.ids[i])
	}
	return keys, ids
}

func TestTree_InsertsMatchTheModel(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	tree := newTestTree(t)
	var m model
	for op := 0; op < 3000; op++ {
		k := randomKey(rng, tree.MaxKeyLen())
		err := tree.Insert(k, rid(op))
		if m.insert(k, rid(op)) {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrDuplicateKey)
		}
		check(t, tree)
	}
	require.Equal(t, len(m.keys), tree.Len())
	for i, k := range m.keys {
		got, found := tree.Get(k)
		require.True(t, found)
		require.Equal(t, m.ids[i], got)
	}
}

// randomKey draws from a three-letter alphabet so keys share prefixes and
// collide often, with lengths from 0 up to max.
func randomKey(rng *rand.Rand, max int) []byte {
	k := make([]byte, rng.Intn(max+1))
	for i := range k {
		k[i] = byte('a' + rng.Intn(3))
	}
	return k
}
