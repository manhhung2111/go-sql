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
	case string:
		// []byte(x) copies, so the key never aliases the string or a later key.
		return []byte(x), nil
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case nil:
		return nil, errors.New("cannot index a NULL value")
	default:
		return nil, fmt.Errorf("unsupported key type %T", v)
	}
}
