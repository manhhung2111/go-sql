# Index key codec Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `internal/index` with `EncodeKey`, which turns a coerced column value (`int64`, `string`, `bool`) into bytes whose `bytes.Compare` order equals the value order.

**Architecture:** One pure function in a new package. Integers are 8 bytes big-endian with the sign bit flipped, strings are their raw bytes, bools are one byte. NULL and unsupported types return errors. No tree, no engine change.

**Tech Stack:** Go 1.26.4, `encoding/binary`, testify (`assert`/`require`).

**Spec:** `docs/specs/2026-10-12-index-key-codec-design.md`

## Global Constraints

- Go 1.26.4; `internal/index` imports nothing from `internal/engine`, `internal/parser` or `internal/storage`.
- Single-column keys only; no type tag, no decode function, no key length limit.
- Strings compare byte-wise and case-sensitively. NULL (`nil`) is an error, never a key.
- Error messages: lowercase, no trailing punctuation, `%T` for the offending type.
- The returned slice is freshly allocated and owned by the caller.
- Tests use testify; table-driven where there is a matrix. Every test is seen failing before the code that makes it pass.

## Review Focus

- `-1` against `0` (the sign boundary): the encodings must be `7f ff ... ff` then `80 00 ... 00`, so `-1` sorts first. Pinned in Task 1.
- The empty string is a valid key and sorts before every other string. Pinned in Task 2.
- A string with embedded zero bytes keeps byte order and is not truncated. Pinned in Task 2.
- A caller modifying a returned key must not affect later encodings of the same value. Pinned in Task 2.
- A typed nil pointer (`(*int64)(nil)`) is an unsupported type, not NULL. Pinned in Task 1.

---

### Task 1: Package scaffold, int64 keys and error cases

**Files:**
- Create: `internal/index/keycodec.go`
- Test: `internal/index/keycodec_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func EncodeKey(v any) ([]byte, error)`. After this task it handles `int64`, and returns an error for `nil` and for every type that is not `int64`, `string` or `bool`. Task 2 adds `string` and `bool`.

- [ ] **Step 1: Write the stub**

Create `internal/index/keycodec.go`:

```go
// Package index holds the B-tree index and its key encoding.
package index

// EncodeKey returns the key bytes for v, a coerced column value: int64,
// string or bool. Keys of the same type compare under bytes.Compare in the
// same order as the values.
func EncodeKey(v any) ([]byte, error) {
	return nil, nil
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/index/keycodec_test.go`:

```go
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
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/index/ -v`
Expected: FAIL. The int tests fail on their assertions (`mustEncode` gets `nil, nil`, so `assert.Len` fails with length 0), and `TestEncodeKey_Errors` fails with "An error is expected but got nil". The package compiles.

- [ ] **Step 4: Implement int64 and the error cases**

Replace `internal/index/keycodec.go`:

```go
// Package index holds the B-tree index and its key encoding.
package index

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// EncodeKey returns the key bytes for v, a coerced column value: int64,
// string or bool. Keys of the same type compare under bytes.Compare in the
// same order as the values. NULL is not indexed, so nil is an error, and so
// is any other type. The returned slice belongs to the caller.
func EncodeKey(v any) ([]byte, error) {
	switch x := v.(type) {
	case int64:
		// Flipping the sign bit makes two's complement order match unsigned
		// big-endian byte order: negatives become 0x00.. to 0x7f.., the rest 0x80.. up.
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(x)^(1<<63))
		return key, nil
	case nil:
		return nil, errors.New("cannot index a NULL value")
	default:
		return nil, fmt.Errorf("unsupported key type %T", v)
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/index/ -v`
Expected: PASS for all four tests (the error table shows four subtests).

- [ ] **Step 6: Commit**

```bash
git add internal/index/keycodec.go internal/index/keycodec_test.go
git commit -m "feat(index): encode int64 keys in byte order" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: String and bool keys

**Files:**
- Modify: `internal/index/keycodec.go`
- Test: `internal/index/keycodec_test.go`

**Interfaces:**
- Consumes: `EncodeKey` and the `mustEncode` test helper from Task 1.
- Produces: `EncodeKey` now also accepts `string` (raw bytes) and `bool` (one byte). This completes the spec's API.

- [ ] **Step 1: Write the failing tests**

Append to `internal/index/keycodec_test.go`:

```go
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
	key := mustEncode(t, "abc")
	key[0] = 'X'
	assert.Equal(t, []byte("abc"), mustEncode(t, "abc"))
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/index/ -run 'String|Bool|Owned' -v`
Expected: FAIL. `mustEncode` fails with `unsupported key type string` / `bool` (the new tests stop at `require.NoError`). The Task 1 tests are not selected.

- [ ] **Step 3: Implement string and bool**

In `internal/index/keycodec.go`, add these two cases before `case nil:`:

```go
	case string:
		// []byte(x) copies, so the key never aliases the string or a later key.
		return []byte(x), nil
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
```

- [ ] **Step 4: Run the whole package**

Run: `go test -race ./internal/index/ -v`
Expected: PASS, including the Task 1 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/index/keycodec.go internal/index/keycodec_test.go
git commit -m "feat(index): encode string and bool keys" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Mutation checks and the full suite

**Files:**
- Modify (temporarily, then reverted): `internal/index/keycodec.go`

**Interfaces:**
- Consumes: the finished `EncodeKey`.
- Produces: nothing new. This task proves the tests can fail; it ends with a clean working tree.

- [ ] **Step 1: Mutation: drop the sign-bit flip**

Change `uint64(x)^(1<<63)` to `uint64(x)` in `internal/index/keycodec.go`. Run `go test ./internal/index/`.
Expected: FAIL in `Int64OrderAtBoundaries`, `Int64SignBoundaryBytes` and `Int64PairwiseOrderMatchesValues`. Restore with `git checkout internal/index/keycodec.go`.

- [ ] **Step 2: Mutation: little-endian integers**

Change `binary.BigEndian.PutUint64` to `binary.LittleEndian.PutUint64`. Run `go test ./internal/index/`.
Expected: FAIL in the same int tests. Restore with `git checkout internal/index/keycodec.go`.

- [ ] **Step 3: Mutation: both bools encode the same byte**

Change `return []byte{1}, nil` to `return []byte{0}, nil`. Run `go test ./internal/index/`.
Expected: FAIL in `TestEncodeKey_Bool`. Restore with `git checkout internal/index/keycodec.go`.

- [ ] **Step 4: Mutation: the key aliases shared storage**

Replace `return []byte(x), nil` in the string case with a version that returns a package-level reused buffer, for example declare `var scratch []byte` and use `scratch = append(scratch[:0], x...); return scratch, nil`. Run `go test ./internal/index/`.
Expected: FAIL in `TestEncodeKey_ReturnedKeyIsOwnedByCaller`. Restore with `git checkout internal/index/keycodec.go`.

- [ ] **Step 5: Run the full suite on the restored code**

Run: `git status --short` (expected: clean), then `go test -race ./... && go vet ./... && gofmt -l .`
Expected: all tests pass, `go vet` prints nothing, `gofmt -l .` prints nothing.

No commit: this task changes nothing that is kept.
