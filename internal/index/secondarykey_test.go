package index

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/storage"
)

func secKey(t *testing.T, v any, id storage.RowID) []byte {
	t.Helper()
	key, err := EncodeSecondaryKey(v, id)
	require.NoError(t, err)
	return key
}

func TestEncodeSecondaryKey_OrdersByValueThenRowID(t *testing.T) {
	// Already in the order they must sort in.
	ordered := []struct {
		v  any
		id storage.RowID
	}{
		{int64(-9223372036854775808), storage.RowID{Page: 5, Slot: 5}},
		{int64(-1), storage.RowID{Page: 0, Slot: 0}},
		{int64(0), storage.RowID{Page: 0, Slot: 1}},
		{int64(0), storage.RowID{Page: 0, Slot: 2}},
		{int64(0), storage.RowID{Page: 1, Slot: 0}},
		{int64(0), storage.RowID{Page: 2147483647, Slot: 0}},
		{int64(1), storage.RowID{Page: 0, Slot: 0}},
		{int64(9223372036854775807), storage.RowID{Page: 0, Slot: 0}},
	}
	for i := 1; i < len(ordered); i++ {
		a := secKey(t, ordered[i-1].v, ordered[i-1].id)
		b := secKey(t, ordered[i].v, ordered[i].id)
		assert.Less(t, bytes.Compare(a, b), 0, "entry %d sorts before entry %d", i-1, i)
	}

	strs := []string{"", "\x00", "\x00\x00", "a", "a\x00", "a\x00b", "a\x01", "ab", "b"}
	for i := 1; i < len(strs); i++ {
		a := secKey(t, strs[i-1], storage.RowID{Page: 9, Slot: 9})
		b := secKey(t, strs[i], storage.RowID{Page: 0, Slot: 0})
		assert.Less(t, bytes.Compare(a, b), 0, "%q sorts before %q whatever their row ids", strs[i-1], strs[i])
	}

	f := secKey(t, false, storage.RowID{Page: 9, Slot: 9})
	tr := secKey(t, true, storage.RowID{Page: 0, Slot: 0})
	assert.Less(t, bytes.Compare(f, tr), 0)
}

func TestEncodeSecondaryKey_Errors(t *testing.T) {
	_, err := EncodeSecondaryKey(nil, storage.RowID{})
	assert.Error(t, err)
	_, err = EncodeSecondaryKey(3.5, storage.RowID{})
	assert.Error(t, err)
}

// For a random set of entries, the entries in [lo, hi) are exactly the ones of
// the value, and none of its neighbours.
func TestSecondaryEqualRange_HoldsExactlyTheEntriesOfAValue(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	alphabet := []string{"", "\x00", "a", "a\x00", "ab", "b", "\xff", "\xff\xff"}
	type entry struct {
		v   any
		key []byte
	}
	var entries []entry
	for i := 0; i < 400; i++ {
		var v any
		if i%2 == 0 {
			v = alphabet[rng.Intn(len(alphabet))]
		} else {
			v = int64(rng.Intn(5) - 2)
		}
		id := storage.RowID{Page: int32(rng.Intn(1000)), Slot: rng.Intn(50)}
		entries = append(entries, entry{v, secKey(t, v, id)})
	}

	for _, probe := range []any{"", "a", "ab", "\x00", "\xff", "zz", int64(-2), int64(0), int64(2), int64(7)} {
		lo, hi, err := SecondaryEqualRange(probe)
		require.NoError(t, err)
		for _, e := range entries {
			inRange := bytes.Compare(e.key, lo) >= 0 && bytes.Compare(e.key, hi) < 0
			assert.Equal(t, e.v == probe, inRange, "probe %q entry %q", probe, e.v)
		}
	}
}

func TestSecondaryEntries_SortIntoGroups(t *testing.T) {
	var keys [][]byte
	for _, v := range []string{"b", "a", "ab", "a"} {
		for s := 0; s < 3; s++ {
			keys = append(keys, secKey(t, v, storage.RowID{Page: 0, Slot: s}))
		}
	}
	slices.SortFunc(keys, bytes.Compare)

	lo, hi, err := SecondaryEqualRange("a")
	require.NoError(t, err)
	first := slices.IndexFunc(keys, func(k []byte) bool { return bytes.Compare(k, lo) >= 0 })
	last := slices.IndexFunc(keys, func(k []byte) bool { return bytes.Compare(k, hi) >= 0 })
	assert.Equal(t, 6, last-first, "both inserts of a, three slots each, contiguous")
}

func TestEncodeSecondaryKey_RejectsANegativeRowID(t *testing.T) {
	_, err := EncodeSecondaryKey(int64(1), storage.RowID{Page: -1, Slot: 0})
	assert.Error(t, err)
	_, err = EncodeSecondaryKey(int64(1), storage.RowID{Page: 0, Slot: -1})
	assert.Error(t, err)
}

func TestSecondaryKeyLen(t *testing.T) {
	tests := []struct {
		v    any
		want int
	}{
		{int64(7), 16},
		{true, 9},
		{"", 10},
		{"a", 11},
		{"a\x00", 13}, // the zero byte is escaped to two
	}
	for _, tc := range tests {
		got, err := SecondaryKeyLen(tc.v)

		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
		key, err := EncodeSecondaryKey(tc.v, storage.RowID{})
		require.NoError(t, err)
		assert.Equal(t, len(key), got, "it is the length of the key")
	}
	_, err := SecondaryKeyLen(nil)
	assert.Error(t, err)
}
