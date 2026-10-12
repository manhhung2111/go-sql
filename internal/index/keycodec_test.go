package index

import (
	"bytes"
	"cmp"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustEncode(t *testing.T, v any) []byte {
	t.Helper()
	key, err := EncodeKey(v)
	require.NoError(t, err)
	return key
}

func TestEncodeKey_Int64OrderAtBoundaries(t *testing.T) {
	values := []int64{math.MinInt64, -1 << 32, -2, -1, 0, 1, 2, 1 << 32, math.MaxInt64}
	for i := 0; i+1 < len(values); i++ {
		a, b := mustEncode(t, values[i]), mustEncode(t, values[i+1])
		assert.Len(t, a, 8)
		assert.Len(t, b, 8)
		assert.Less(t, bytes.Compare(a, b), 0, "%d must sort before %d", values[i], values[i+1])
	}
}

func TestEncodeKey_Int64SignBoundaryBytes(t *testing.T) {
	assert.Equal(t, []byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, mustEncode(t, int64(-1)))
	assert.Equal(t, []byte{0x80, 0, 0, 0, 0, 0, 0, 0}, mustEncode(t, int64(0)))
}

func TestEncodeKey_Int64PairwiseOrderMatchesValues(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 10000 {
		a, b := int64(rng.Uint64()), int64(rng.Uint64())
		got := bytes.Compare(mustEncode(t, a), mustEncode(t, b))
		assert.Equal(t, cmp.Compare(a, b), got, "a=%d b=%d", a, b)
	}
	assert.Equal(t, mustEncode(t, int64(42)), mustEncode(t, int64(42)))
}

func TestEncodeKey_Errors(t *testing.T) {
	var nilPtr *int64
	tests := []struct {
		name    string
		value   any
		wantErr string
	}{
		{"nil is NULL", nil, "cannot index a NULL value"},
		{"int", 5, "unsupported key type int"},
		{"float64", 1.5, "unsupported key type float64"},
		{"typed nil pointer", nilPtr, "unsupported key type *int64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := EncodeKey(tc.value)
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantErr)
			assert.Nil(t, key)
		})
	}
}

func TestEncodeKey_StringOrder(t *testing.T) {
	// Strictly increasing under byte-wise comparison: the empty string first,
	// uppercase before lowercase, a prefix before its extensions, and embedded
	// zero bytes ordered by value.
	ordered := []string{"", "B", "a", "a\x00", "a\x00b", "ab"}
	for i := 0; i+1 < len(ordered); i++ {
		a, b := mustEncode(t, ordered[i]), mustEncode(t, ordered[i+1])
		assert.Less(t, bytes.Compare(a, b), 0, "%q must sort before %q", ordered[i], ordered[i+1])
	}
}

func TestEncodeKey_StringBytesAreUnchanged(t *testing.T) {
	for _, s := range []string{"", "abc", "a\x00b", "\xff\xfe", "héllo"} {
		assert.Equal(t, []byte(s), mustEncode(t, s), "%q", s)
	}
}

func TestEncodeKey_Bool(t *testing.T) {
	f, tr := mustEncode(t, false), mustEncode(t, true)
	assert.Equal(t, []byte{0}, f)
	assert.Equal(t, []byte{1}, tr)
	assert.Less(t, bytes.Compare(f, tr), 0)
}

func TestEncodeKey_ReturnedKeyIsOwnedByCaller(t *testing.T) {
	// A key must survive later encodings, and editing it must not leak into them.
	first := mustEncode(t, "abc")
	second := mustEncode(t, "xyz")
	assert.Equal(t, []byte("abc"), first)

	first[0] = 'X'
	assert.Equal(t, []byte("abc"), mustEncode(t, "abc"))
	assert.Equal(t, []byte("xyz"), second)
}
