# In-memory B+ tree Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an in-memory B+ tree to `internal/index` that maps encoded keys to `storage.RowID`, sizes its nodes in bytes as if they were serialised pages, and supports `Insert`, `Get`, `Delete` and `Range`.

**Architecture:** A `Tree` of `node`s. Leaves hold keys and row ids and are chained through `next`; internal nodes hold separators and children. Every node tracks `used`, its serialised size, and splits when it exceeds the byte budget. Delete rebalances by borrowing from a sibling or merging, and a borrow that grows a parent separator may split the parent. No locking, no I/O, no engine change.

**Tech Stack:** Go 1.26.4, `bytes`, `sort`, `slices`, `iter`, testify (`assert`/`require`).

**Spec:** `docs/specs/2026-10-12-in-memory-btree-design.md`

## Global Constraints

- Go 1.26.4; `internal/index` may import `internal/storage` (for `storage.RowID`); `storage` never imports `index`; nothing in `internal/engine`, `internal/parser` or `internal/grpcserver` changes.
- Size accounting, in one place: node header 16 bytes; leaf entry 2 + key + 6; internal entry 2 + separator + 4. `MaxKeyLen = (nodeBytes - 16) / 4 - 8`; `New` fails when that is below 1.
- Keys are unique; `Insert` copies the key; the empty key is valid; `Range` is half-open `[lo, hi)`, `nil` is unbounded, and a non-nil empty `hi` is an empty range.
- No internal locking; a tree must not be modified while a `Range` is consumed.
- Error messages: lowercase, no trailing punctuation. Sentinels `ErrDuplicateKey` ("duplicate key") and `ErrKeyTooLong` ("key too long") are wrapped with `%w`.
- Run `gofmt -w internal/index/` after each edit; the plan's snippets are gofmt-clean but appending and deleting leaves stray blank lines.
- Tests use testify, a fixed seed, and a 96-byte node (`MaxKeyLen` 12) so splits, borrows and merges happen constantly. Every test is seen failing before the code that makes it pass.

## Review Focus

- A delete-time borrow that replaces a separator with a longer key must not overflow the parent: the parent splits instead. Pinned by the delete-heavy phase of the property test in Task 5.
- Deleting every key must collapse the tree to a single empty leaf that still works. Pinned in Task 4.
- A key of exactly `MaxKeyLen` is accepted and one byte more is rejected, both without changing the tree. Pinned in Task 1.
- `Range` edges: `nil` bounds, `lo == hi`, a non-nil empty `hi`, a `lo` past the last key, and a range across many leaves. Pinned in Task 3.
- A separator whose row was deleted still gives "not found" and the key can be inserted again. Pinned in Task 4.

---

### Task 1: Tree skeleton, `Get` and `Insert` into one leaf

**Files:**
- Create: `internal/index/btree.go`
- Test: `internal/index/btree_test.go`

**Interfaces:**
- Consumes: `storage.RowID`. (`EncodeKey` from the codec is not used here; keys arrive as `[]byte`.)
- Produces: `DefaultNodeBytes`, `ErrDuplicateKey`, `ErrKeyTooLong`, `type Tree`, `New(nodeBytes int) (*Tree, error)`, `(*Tree).MaxKeyLen() int`, `Len() int`, `Get(key []byte) (storage.RowID, bool)`, `Insert(key []byte, id storage.RowID) error`. Internally: `node{leaf, keys, ids, children, next, used}`, the size helpers, and the test helpers `newTestTree`, `rid`, `key`, `check` that later tasks reuse. After this task the root is always a leaf; `Insert` has no splitting yet.

- [ ] **Step 1: Write the failing tests**

Create `internal/index/btree_test.go`:

```go
package index

import (
	"bytes"
	"fmt"
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/index/ 2>&1 | head -20`
Expected: FAIL to compile: `undefined: New`, `Tree`, `nodeHeaderSize` and the rest. The feature is missing; the codec tests are untouched.

- [ ] **Step 3: Write the implementation**

Create `internal/index/btree.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/index/ -v 2>&1 | grep -E '^(--- |FAIL|ok|PASS)'`
Expected: PASS for the five new tests and the existing codec tests.

- [ ] **Step 5: Commit**

```bash
git add internal/index/btree.go internal/index/btree_test.go
git commit -m "feat(index): add the B+ tree skeleton with Get and a single-leaf Insert" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Splits

**Files:**
- Modify: `internal/index/btree.go`
- Test: `internal/index/btree_test.go`

**Interfaces:**
- Consumes: Task 1's `node`, `Tree`, `insertAt`, `search`, `childIndex`, `leafEntrySize`, `sepEntrySize`.
- Produces: `Insert` that splits full nodes and grows the root; `(*node).recount()`, `growRoot`, `(*Tree).insert`, `(*node).adoptSplit`, `splitPoint`, `splitLeaf`, `splitInternal`; the test helpers `model` (with `find`, `insert`, `delete`, `rangeOf`) and `randomKey`, reused by Tasks 3 to 5.

- [ ] **Step 1: Write the failing tests**

In `internal/index/btree_test.go`, replace the import block with:

```go
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
```

and append:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/index/ -run 'Bulk|InsertsMatch' 2>&1 | head -20`
Expected: FAIL. `check` reports `node uses N bytes, over the budget of 96`, because Task 1's `Insert` never splits.

- [ ] **Step 3: Implement splitting**

In `internal/index/btree.go`, delete Task 1's `Insert` and add the following (the `Insert` below replaces it; the rest is new). `recount` may sit next to `entrySize`:

```go
// recount recomputes used from the node's contents.
func (n *node) recount() {
	n.used = nodeHeaderSize
	for i := range n.keys {
		n.used += n.entrySize(i)
	}
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/index/ -v 2>&1 | grep -E '^(--- FAIL|FAIL|ok|PASS)'`
Expected: PASS for the whole package, including the three bulk-insert orders and the model comparison.

- [ ] **Step 5: Commit**

```bash
git add internal/index/btree.go internal/index/btree_test.go
git commit -m "feat(index): split full B+ tree nodes by bytes" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `Range`

**Files:**
- Modify: `internal/index/btree.go`
- Test: `internal/index/btree_test.go`

**Interfaces:**
- Consumes: the leaf chain (`next`) that `splitLeaf` maintains.
- Produces: `(*Tree).Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID]`; the test helpers `collect` and `filled`.

- [ ] **Step 1: Add a stub so the tests compile**

In `internal/index/btree.go`, add `"iter"` to the imports (after `"fmt"`) and append:

```go
// Range yields the keys in [lo, hi) in order.
func (t *Tree) Range(lo, hi []byte) iter.Seq2[[]byte, storage.RowID] {
	return func(yield func([]byte, storage.RowID) bool) {}
}
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/index/btree_test.go`:

```go
func collect(tree *Tree, lo, hi []byte) (keys [][]byte, ids []storage.RowID) {
	for k, id := range tree.Range(lo, hi) {
		keys, ids = append(keys, slices.Clone(k)), append(ids, id)
	}
	return keys, ids
}

func filled(t *testing.T, n int) *Tree {
	t.Helper()
	tree := newTestTree(t)
	for i := 0; i < n; i++ {
		require.NoError(t, tree.Insert(key(i), rid(i)))
	}
	return tree
}

func TestTree_RangeBounds(t *testing.T) {
	tree := filled(t, 200)
	tests := []struct {
		name     string
		lo, hi   []byte
		from, to int // expected keys are key(from) .. key(to-1)
	}{
		{"everything", nil, nil, 0, 200},
		{"half open", key(10), key(20), 10, 20},
		{"lo only", key(190), nil, 190, 200},
		{"hi only", nil, key(5), 0, 5},
		{"lo equals hi", key(50), key(50), 0, 0},
		{"lo between keys", []byte("k00010a"), key(13), 11, 13},
		{"lo past the last key", []byte("z"), nil, 0, 0},
		{"hi before the first key", nil, []byte("a"), 0, 0},
		{"non-nil empty hi is empty", nil, []byte{}, 0, 0},
		{"non-nil empty lo is unbounded", []byte{}, key(3), 0, 3},
		{"spans many leaves", key(3), key(197), 3, 197},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keys, ids := collect(tree, tc.lo, tc.hi)
			require.Len(t, keys, tc.to-tc.from)
			for i := range keys {
				assert.Equal(t, key(tc.from+i), keys[i])
				assert.Equal(t, rid(tc.from+i), ids[i])
			}
		})
	}
}

func TestTree_RangeStopsWhenTheConsumerBreaks(t *testing.T) {
	tree := filled(t, 100)
	seen := 0
	for range tree.Range(nil, nil) {
		seen++
		if seen == 3 {
			break
		}
	}
	assert.Equal(t, 3, seen)
}

func TestTree_RangeOnAnEmptyTree(t *testing.T) {
	keys, _ := collect(newTestTree(t), nil, nil)
	assert.Empty(t, keys)
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/index/ -run Range 2>&1 | head -20`
Expected: FAIL. Every `RangeBounds` case that expects keys fails with a length mismatch (the stub yields nothing), and `StopsWhenTheConsumerBreaks` fails with `3 != 0`. The empty-range cases and `OnAnEmptyTree` already pass.

- [ ] **Step 4: Implement `Range`**

Replace the stub with:

```go
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/index/ -v 2>&1 | grep -E '^(--- FAIL|FAIL|ok|PASS)'`
Expected: PASS for the whole package.

- [ ] **Step 6: Commit**

```bash
git add internal/index/btree.go internal/index/btree_test.go
git commit -m "feat(index): add Range over the B+ tree leaf chain" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `Delete`, borrowing, merging and root collapse

**Files:**
- Modify: `internal/index/btree.go`
- Test: `internal/index/btree_test.go`

**Interfaces:**
- Consumes: Task 2's `splitInternal`, `adoptSplit`, `growRoot`, `recount`; Task 3's test helper `filled`.
- Produces: `(*Tree).Delete(key []byte) bool`; `delete`, `rebalance`, `borrowFromLeft`, `borrowFromRight`, `replaceSeparator`, `merge`; `removeAt`.

- [ ] **Step 1: Add a stub so the tests compile**

In `internal/index/btree.go` append:

```go
// Delete removes key and reports whether it was present.
func (t *Tree) Delete(key []byte) bool { return false }
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/index/btree_test.go`:

```go
func TestTree_DeleteAbsentKey(t *testing.T) {
	tree := filled(t, 50)
	assert.False(t, tree.Delete([]byte("nope")))
	assert.Equal(t, 50, tree.Len())
	check(t, tree)
}

func TestTree_DeleteEverythingLeavesAnEmptyUsableTree(t *testing.T) {
	const n = 400
	asc := make([]int, n)
	desc := make([]int, n)
	for i := range asc {
		asc[i], desc[i] = i, n-1-i
	}
	for name, order := range map[string][]int{
		"ascending":  asc,
		"descending": desc,
		"shuffled":   rand.New(rand.NewSource(3)).Perm(n),
	} {
		t.Run(name, func(t *testing.T) {
			tree := filled(t, n)
			for i, k := range order {
				require.True(t, tree.Delete(key(k)), "key %d", k)
				require.Equal(t, n-1-i, tree.Len())
				check(t, tree)
				_, found := tree.Get(key(k))
				require.False(t, found)
			}
			assert.True(t, tree.root.leaf, "an emptied tree collapses to a single leaf")
			assert.Equal(t, nodeHeaderSize, tree.root.used)

			require.NoError(t, tree.Insert([]byte("again"), rid(1)))
			check(t, tree)
		})
	}
}

func TestTree_StaleSeparatorStillGivesNotFoundAndAllowsReinsert(t *testing.T) {
	tree := filled(t, 60)
	require.False(t, tree.root.leaf)
	sep := slices.Clone(tree.root.keys[0])

	// The separator is the first key of a leaf; deleting that key may leave it
	// behind in an internal node.
	var n int
	_, err := fmt.Sscanf(string(sep), "k%05d", &n)
	require.NoError(t, err)
	require.True(t, tree.Delete(sep))
	check(t, tree)
	_, found := tree.Get(sep)
	assert.False(t, found)

	require.NoError(t, tree.Insert(sep, rid(n)))
	got, found := tree.Get(sep)
	assert.True(t, found)
	assert.Equal(t, rid(n), got)
	check(t, tree)
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/index/ -run 'Delete|Stale' 2>&1 | head -20`
Expected: FAIL. `DeleteEverything` fails on its first `require.True(t, tree.Delete(...))` in all three orders, and `StaleSeparator` fails the same way. `DeleteAbsentKey` already passes, because the stub returns false.

- [ ] **Step 4: Implement delete**

In `internal/index/btree.go`, delete the stub and add `removeAt` beside `insertAt`:

```go
func removeAt[T any](s []T, i int) []T {
	var zero T
	copy(s[i:], s[i+1:])
	s[len(s)-1] = zero
	return s[:len(s)-1]
}
```

then append:

```go
// Delete removes key and reports whether it was present. Only the leaf holding
// the key changes, plus whatever rebalancing the leaf's shrinking triggers; a
// separator equal to the key may stay behind.
func (t *Tree) Delete(key []byte) bool {
	found, sep, right := t.delete(t.root, key)
	if !found {
		return false
	}
	if right != nil {
		t.growRoot(sep, right)
	}
	for !t.root.leaf && len(t.root.keys) == 0 {
		t.root = t.root.children[0]
	}
	t.size--
	return true
}

// delete removes key below n and rebalances under-full children. Borrowing can
// replace a separator with a longer one, so n may overflow and split; the
// separator and right sibling are returned like insert's.
func (t *Tree) delete(n *node, key []byte) (bool, []byte, *node) {
	if n.leaf {
		i, found := n.search(key)
		if !found {
			return false, nil, nil
		}
		n.used -= leafEntrySize(n.keys[i])
		n.keys = removeAt(n.keys, i)
		n.ids = removeAt(n.ids, i)
		return true, nil, nil
	}

	ci := n.childIndex(key)
	found, csep, cright := t.delete(n.children[ci], key)
	if !found {
		return false, nil, nil
	}
	if cright != nil {
		n.adoptSplit(ci, csep, cright)
	} else if n.children[ci].used < t.nodeBytes/2 {
		t.rebalance(n, ci)
	}
	if n.used <= t.nodeBytes {
		return true, nil, nil
	}
	sep, right := t.splitInternal(n)
	return true, sep, right
}

// rebalance fixes the under-full child ci of parent: it borrows from a
// sibling that stays at least half full, else merges with a sibling when the
// two fit in one node, else leaves the child alone.
func (t *Tree) rebalance(parent *node, ci int) {
	hasLeft, hasRight := ci > 0, ci+1 < len(parent.children)
	switch {
	case hasLeft && t.borrowFromLeft(parent, ci):
	case hasRight && t.borrowFromRight(parent, ci):
	case hasLeft && t.merge(parent, ci-1):
	case hasRight && t.merge(parent, ci):
	}
}

// borrowFromLeft moves the last entry of the left sibling into child ci.
func (t *Tree) borrowFromLeft(parent *node, ci int) bool {
	left, child, s := parent.children[ci-1], parent.children[ci], ci-1
	last := len(left.keys) - 1
	if child.leaf {
		moved := leafEntrySize(left.keys[last])
		if left.used-moved < t.nodeBytes/2 || child.used+moved > t.nodeBytes {
			return false
		}
		child.keys = insertAt(child.keys, 0, left.keys[last])
		child.ids = insertAt(child.ids, 0, left.ids[last])
		left.keys, left.ids = removeAt(left.keys, last), removeAt(left.ids, last)
		child.used += moved
		left.used -= moved
		parent.replaceSeparator(s, child.keys[0])
		return true
	}
	lent, pulled := sepEntrySize(left.keys[last]), sepEntrySize(parent.keys[s])
	if left.used-lent < t.nodeBytes/2 || child.used+pulled > t.nodeBytes {
		return false
	}
	child.keys = insertAt(child.keys, 0, parent.keys[s])
	child.children = insertAt(child.children, 0, left.children[last+1])
	parent.replaceSeparator(s, left.keys[last])
	left.keys, left.children = removeAt(left.keys, last), removeAt(left.children, last+1)
	child.used += pulled
	left.used -= lent
	return true
}

// borrowFromRight moves the first entry of the right sibling into child ci.
func (t *Tree) borrowFromRight(parent *node, ci int) bool {
	child, right, s := parent.children[ci], parent.children[ci+1], ci
	if child.leaf {
		moved := leafEntrySize(right.keys[0])
		if right.used-moved < t.nodeBytes/2 || child.used+moved > t.nodeBytes {
			return false
		}
		child.keys = append(child.keys, right.keys[0])
		child.ids = append(child.ids, right.ids[0])
		right.keys, right.ids = removeAt(right.keys, 0), removeAt(right.ids, 0)
		child.used += moved
		right.used -= moved
		parent.replaceSeparator(s, right.keys[0])
		return true
	}
	lent, pulled := sepEntrySize(right.keys[0]), sepEntrySize(parent.keys[s])
	if right.used-lent < t.nodeBytes/2 || child.used+pulled > t.nodeBytes {
		return false
	}
	child.keys = append(child.keys, parent.keys[s])
	child.children = append(child.children, right.children[0])
	parent.replaceSeparator(s, right.keys[0])
	right.keys, right.children = removeAt(right.keys, 0), removeAt(right.children, 0)
	child.used += pulled
	right.used -= lent
	return true
}

// replaceSeparator swaps parent.keys[s] for key and adjusts the byte count.
func (n *node) replaceSeparator(s int, key []byte) {
	n.used += sepEntrySize(key) - sepEntrySize(n.keys[s])
	n.keys[s] = key
}

// merge folds child s+1 into child s when they fit in one node, removing
// separator s from the parent. It reports whether it merged.
func (t *Tree) merge(parent *node, s int) bool {
	left, right := parent.children[s], parent.children[s+1]
	combined := left.used + right.used - nodeHeaderSize
	if !left.leaf {
		combined += sepEntrySize(parent.keys[s])
	}
	if combined > t.nodeBytes {
		return false
	}
	if left.leaf {
		left.keys = append(left.keys, right.keys...)
		left.ids = append(left.ids, right.ids...)
		left.next = right.next
	} else {
		left.keys = append(append(left.keys, parent.keys[s]), right.keys...)
		left.children = append(left.children, right.children...)
	}
	left.used = combined
	parent.used -= sepEntrySize(parent.keys[s])
	parent.keys = removeAt(parent.keys, s)
	parent.children = removeAt(parent.children, s+1)
	return true
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./internal/index/ -v 2>&1 | grep -E '^(--- FAIL|FAIL|ok|PASS)'`
Expected: PASS for the whole package. `check` runs after every delete, so a wrong byte count, separator, depth or leaf chain fails here.

- [ ] **Step 6: Commit**

```bash
git add internal/index/btree.go internal/index/btree_test.go
git commit -m "feat(index): delete from the B+ tree with borrow, merge and root collapse" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Property test, mutation checks and the full suite

**Files:**
- Modify (temporarily, then reverted): `internal/index/btree.go`
- Test: `internal/index/btree_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `TestTree_MatchesTheModel`, which drives random inserts, deletes, gets and ranges against a sorted slice and runs `check` after every operation, in three mixes (balanced, grow then shrink, delete heavy).

- [ ] **Step 1: Write the property test**

Append to `internal/index/btree_test.go`:

```go
// TestTree_MatchesTheModel drives random operations against the tree and a
// sorted slice and requires them to agree after every step.
func TestTree_MatchesTheModel(t *testing.T) {
	phases := []struct {
		name                 string
		seed                 int64
		ops                  int
		insertPct, deletePct int
	}{
		{"balanced", 21, 2500, 45, 35},
		{"grow then shrink", 22, 2500, 70, 10},
		{"delete heavy", 23, 2500, 25, 60},
	}
	for _, ph := range phases {
		t.Run(ph.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(ph.seed))
			tree := newTestTree(t)
			var m model
			for op := 0; op < ph.ops; op++ {
				switch pick := rng.Intn(100); {
				case pick < ph.insertPct:
					k := randomKey(rng, tree.MaxKeyLen())
					err := tree.Insert(k, rid(op))
					if m.insert(k, rid(op)) {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, ErrDuplicateKey)
					}
				case pick < ph.insertPct+ph.deletePct:
					var k []byte
					if len(m.keys) > 0 && rng.Intn(10) < 8 {
						k = m.keys[rng.Intn(len(m.keys))]
					} else {
						k = randomKey(rng, tree.MaxKeyLen())
					}
					require.Equal(t, m.delete(k), tree.Delete(k), "delete %x", k)
				default:
					k := randomKey(rng, tree.MaxKeyLen())
					i, want := m.find(k)
					got, found := tree.Get(k)
					require.Equal(t, want, found)
					if want {
						require.Equal(t, m.ids[i], got)
					}
					lo, hi := randomKey(rng, 4), randomKey(rng, 4)
					if rng.Intn(4) == 0 {
						lo = nil
					}
					if rng.Intn(4) == 0 {
						hi = nil
					}
					gotKeys, gotIDs := collect(tree, lo, hi)
					wantKeys, wantIDs := m.rangeOf(lo, hi)
					require.Equal(t, len(wantKeys), len(gotKeys), "range [%x, %x)", lo, hi)
					for i := range wantKeys {
						require.Equal(t, wantKeys[i], gotKeys[i])
						require.Equal(t, wantIDs[i], gotIDs[i])
					}
				}
				require.Equal(t, len(m.keys), tree.Len())
				check(t, tree)
			}
		})
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -race ./internal/index/ -run MatchesTheModel -v 2>&1 | grep -E '^(\s*--- |FAIL|ok|PASS)'`
Expected: PASS for the three phases. (It passes on the first run because the implementation is already complete; Step 3 is what proves the test can fail.) If a phase fails, the output names the operation and the violated invariant: fix the code, not the test, and add a ledger ruling.

- [ ] **Step 3: Mutation checks**

For each row, make the one edit in `internal/index/btree.go`, run `go test ./internal/index/`, confirm at least one test fails, then restore with `git checkout internal/index/btree.go`.

| # | Edit | Test that must fail |
| --- | --- | --- |
| 1 | In `splitLeaf`, delete the line `n.next = right` | `BulkInsert...` (leaf chain) |
| 2 | In `splitLeaf`, return `n.keys[len(n.keys)-1]` instead of `right.keys[0]` | `BulkInsert...` |
| 3 | In `insert`, delete the line `n.used += leafEntrySize(key)` | the one-leaf tests |
| 4 | In `merge`, delete the line `parent.keys = removeAt(parent.keys, s)` | `DeleteEverything...` |
| 5 | In `Range`, change `>= 0` to `> 0` in the `hi` comparison | `RangeBounds` |
| 6 | In `borrowFromLeft`, delete the leaf branch's `parent.replaceSeparator(s, child.keys[0])` | `MatchesTheModel` or `DeleteEverything...` |
| 7 | In `Delete`, change the root-collapse loop condition to `false` | `DeleteEverything...` |
| 8 | In `delete`, change `if cright != nil {` to `if false {` | `MatchesTheModel` |
| 9 | In `merge`, delete `combined += sepEntrySize(parent.keys[s])` | `DeleteEverything...` |
| 10 | In `Delete`, delete the line `t.size--` | `DeleteEverything...` |
| 11 | In `childIndex`, change `> 0` to `>= 0` | `BulkInsert...` |

Expected: all eleven fail a test. A mutant that survives means a missing test: add one that fails for it, then continue.

- [ ] **Step 4: Run the full suite on the restored code**

Run: `git status --short` (expected: nothing uncommitted except this task's test), then `go test -race ./... && go vet ./... && gofmt -l .`
Expected: every package passes, and `go vet` and `gofmt -l .` print nothing. Note how long `go test -race ./internal/index/` takes; it should stay in the low seconds.

- [ ] **Step 5: Commit**

```bash
git add internal/index/btree_test.go
git commit -m "test(index): check the B+ tree against a sorted-slice model" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
