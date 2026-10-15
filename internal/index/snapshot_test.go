package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

func snapEntries(tree *Tree) []string {
	out := []string{}
	if tree == nil {
		return out
	}
	for key, id := range tree.Range(nil, nil) {
		out = append(out, fmt.Sprintf("%x=%d/%d", key, id.Page, id.Slot))
	}
	return out
}

// snapTree builds a tree of n keys of the given byte length, in an order other
// than sorted, with row ids that depend on the key.
func snapTree(t *testing.T, n, keyLen int) *Tree {
	t.Helper()
	tree, err := New(DefaultNodeBytes)
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		j := (i * 7919) % n
		key := []byte(fmt.Sprintf("%0*d", keyLen, j))
		require.NoError(t, tree.Insert(key, storage.RowID{Page: int32(j / 10), Slot: j % 10}))
	}
	return tree
}

var (
	idPK     = TreeID{Kind: UniqueTree, Ordinal: 0, Column: "id"}
	idEmail  = TreeID{Kind: UniqueTree, Ordinal: 1, Column: "email"}
	idByName = TreeID{Kind: SecondaryTree, Name: "by_name", Column: "name"}
)

func snapPath(t *testing.T) string { return filepath.Join(t.TempDir(), "7.idx") }

// patchPage rewrites page n of the snapshot through the page file, which
// re-stamps the checksum, after change has edited its payload.
func patchPage(t *testing.T, path string, n int32, change func(payload []byte)) {
	t.Helper()
	pf, err := storage.OpenPageFile(path)
	require.NoError(t, err)
	page, err := pf.ReadPage(n)
	require.NoError(t, err)
	buf := append([]byte(nil), page...)
	change(buf[storage.PageHeaderSize:])
	require.NoError(t, pf.WritePage(n, buf))
	require.NoError(t, pf.Sync())
	require.NoError(t, pf.Close())
}

func writeSample(t *testing.T, path string) []SnapshotTree {
	t.Helper()
	trees := writeSampleTrees(t)
	require.NoError(t, WriteSnapshot(path, 7, 42, trees))
	return trees
}

func TestSnapshot_RoundTripsEveryKindOfTree(t *testing.T) {
	path := snapPath(t)
	empty, err := New(DefaultNodeBytes)
	require.NoError(t, err)
	idEmpty := TreeID{Kind: SecondaryTree, Name: "empty", Column: "x"}
	trees := append(writeSampleTrees(t), SnapshotTree{ID: idEmpty, Tree: empty})
	require.NoError(t, WriteSnapshot(path, 7, 42, trees))

	got, err := ReadSnapshot(path, 7, 42, []TreeID{idByName, idEmpty, idPK, idEmail})

	require.NoError(t, err)
	require.Len(t, got, 4, "in the order asked for")
	assert.Equal(t, []TreeID{idByName, idEmpty, idPK, idEmail}, []TreeID{got[0].ID, got[1].ID, got[2].ID, got[3].ID})
	assert.Equal(t, snapEntries(trees[2].Tree), snapEntries(got[0].Tree))
	require.NotNil(t, got[1].Tree, "an empty tree stays a tree")
	assert.Equal(t, 0, got[1].Tree.Len())
	assert.Equal(t, snapEntries(trees[0].Tree), snapEntries(got[2].Tree))
	assert.Equal(t, 3000, got[2].Tree.Len())
	assert.Nil(t, got[3].Tree, "a degraded tree stays degraded")
}

// writeSampleTrees is a unique tree spanning several pages, a degraded one and
// a small secondary tree.
func writeSampleTrees(t *testing.T) []SnapshotTree {
	t.Helper()
	return []SnapshotTree{
		{ID: idPK, Tree: snapTree(t, 3000, 100)},
		{ID: idEmail, Tree: nil},
		{ID: idByName, Tree: snapTree(t, 5, 3)},
	}
}

func TestSnapshot_SpansManyPages(t *testing.T) {
	path := snapPath(t)
	writeSample(t, path)

	pf, err := storage.OpenPageFile(path)
	require.NoError(t, err)
	defer pf.Close()

	assert.Greater(t, pf.NumPages(), int32(10), "3000 entries of 100 bytes do not fit a few pages")
}

func TestSnapshot_WriteLeavesNoTempFileAndReplacesAnOldOne(t *testing.T) {
	path := snapPath(t)
	require.NoError(t, os.WriteFile(path+".tmp", []byte("junk"), 0o644)) // a crashed earlier save
	writeSample(t, path)
	small := []SnapshotTree{{ID: idPK, Tree: snapTree(t, 2, 3)}}

	require.NoError(t, WriteSnapshot(path, 7, 42, small))

	_, err := os.Stat(path + ".tmp")
	assert.ErrorIs(t, err, os.ErrNotExist)
	got, err := ReadSnapshot(path, 7, 42, []TreeID{idPK})
	require.NoError(t, err)
	assert.Equal(t, 2, got[0].Tree.Len(), "the new content replaced the old")
}

func TestSnapshot_NoTreesIsAValidFile(t *testing.T) {
	path := snapPath(t)

	require.NoError(t, WriteSnapshot(path, 1, 1, nil))
	got, err := ReadSnapshot(path, 1, 1, nil)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSnapshot_TooManyDescriptorsFailsAndLeavesNothing(t *testing.T) {
	path := snapPath(t)
	var trees []SnapshotTree
	for i := 0; i < 2000; i++ {
		trees = append(trees, SnapshotTree{ID: TreeID{Kind: SecondaryTree, Name: fmt.Sprintf("index_number_%d", i), Column: "c"}})
	}

	err := WriteSnapshot(path, 1, 1, trees)

	assert.ErrorIs(t, err, ErrSnapshotTooLarge)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(path + ".tmp")
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSnapshot_ReadRejectsWhatDoesNotMatch(t *testing.T) {
	want := []TreeID{idPK, idEmail, idByName}
	tests := []struct {
		name     string
		heapID   int64
		pages    int32
		want     []TreeID
		mismatch bool
	}{
		{"another heap file", 8, 42, want, true},
		{"another page count", 7, 43, want, true},
		{"a tree missing from the table", 7, 42, []TreeID{idPK, idEmail}, true},
		{"a tree the file does not have", 7, 42, append(append([]TreeID{}, want...), TreeID{Kind: SecondaryTree, Name: "new", Column: "x"}), true},
		{"an index renamed", 7, 42, []TreeID{idPK, idEmail, {Kind: SecondaryTree, Name: "by_other", Column: "name"}}, true},
		{"a column renamed", 7, 42, []TreeID{idPK, idEmail, {Kind: SecondaryTree, Name: "by_name", Column: "label"}}, true},
		{"a unique column at another position", 7, 42, []TreeID{{Kind: UniqueTree, Ordinal: 3, Column: "id"}, idEmail, idByName}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := snapPath(t)
			writeSample(t, path)

			_, err := ReadSnapshot(path, tc.heapID, tc.pages, tc.want)

			assert.ErrorIs(t, err, ErrSnapshotMismatch)
		})
	}
}

func TestSnapshot_ReadRejectsDamage(t *testing.T) {
	want := []TreeID{idPK, idEmail, idByName}
	tests := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"a missing file", func(t *testing.T, path string) { require.NoError(t, os.Remove(path)) }},
		{"a flipped byte", func(t *testing.T, path string) {
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			b[2*storage.PageSize+100] ^= 0xFF
			require.NoError(t, os.WriteFile(path, b, 0o644))
		}},
		{"a torn file", func(t *testing.T, path string) {
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, b[:len(b)-1], 0o644))
		}},
		{"a bad magic", func(t *testing.T, path string) {
			patchPage(t, path, 0, func(p []byte) { p[hdrMagic] = 'X' })
		}},
		{"an unknown version", func(t *testing.T, path string) {
			patchPage(t, path, 0, func(p []byte) { p[hdrVersion] = 99 })
		}},
		{"entries out of order", func(t *testing.T, path string) {
			// Page 1's payload is the count (2 bytes), then entries of key
			// length (2), the 100-byte key, page (4) and slot (2). The first
			// two keys differ only in their last digit; swap those digits.
			patchPage(t, path, 1, func(p []byte) {
				first := 2 + 2
				second := first + 100 + 4 + 2 + 2
				p[first+99], p[second+99] = p[second+99], p[first+99]
			})
		}},
		{"a trailing page", func(t *testing.T, path string) {
			pf, err := storage.OpenPageFile(path)
			require.NoError(t, err)
			_, err = pf.AllocatePage()
			require.NoError(t, err)
			require.NoError(t, pf.Sync())
			require.NoError(t, pf.Close())
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := snapPath(t)
			writeSample(t, path)
			tc.damage(t, path)

			_, err := ReadSnapshot(path, 7, 42, want)

			assert.Error(t, err)
			assert.NotErrorIs(t, err, ErrSnapshotMismatch, "damage is not a mismatch")
		})
	}
}

func TestSnapshot_ReadRejectsAWrongEntryCount(t *testing.T) {
	path := snapPath(t)
	writeSample(t, path)
	patchPage(t, path, 0, func(p []byte) {
		// the first descriptor: kind(1) ordinal(4) degraded(1) then count(4)
		count := hdrDescriptors + 1 + 4 + 1
		p[count]++
	})

	_, err := ReadSnapshot(path, 7, 42, []TreeID{idPK, idEmail, idByName})

	assert.Error(t, err)
}
