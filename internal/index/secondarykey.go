package index

import (
	"encoding/binary"
	"errors"
	"fmt"

	"manhhung2111/go-sql/internal/storage"
)

// secondaryRowIDSize is the page and slot that end every secondary key.
const secondaryRowIDSize = 8

// valuePart is the order-preserving, prefix-free encoding of v: no value's
// encoding is a prefix of another's, so appending anything to one never sorts
// among the entries of another value.
func valuePart(v any) ([]byte, error) {
	switch x := v.(type) {
	case int64:
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(x)^(1<<63))
		return key, nil
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case string:
		// 0x00 is written 0x00 0xFF and the value ends with 0x00 0x01: the
		// terminator sorts below every escaped byte pair and every other byte
		// after a 0x00, so a prefix sorts before its extensions.
		out := make([]byte, 0, len(x)+2)
		for i := 0; i < len(x); i++ {
			if x[i] == 0 {
				out = append(out, 0x00, 0xFF)
				continue
			}
			out = append(out, x[i])
		}
		return append(out, 0x00, 0x01), nil
	case nil:
		return nil, errors.New("cannot index a NULL value")
	default:
		return nil, fmt.Errorf("unsupported key type %T", v)
	}
}

// EncodeSecondaryKey returns the tree key for the entry of value v at row id:
// the value part, then the page and slot as 4 bytes each, big-endian. Entries of
// one value are adjacent and ordered by RowID, which is file order. Page and slot
// must be non-negative and fit in 32 bits.
func EncodeSecondaryKey(v any, id storage.RowID) ([]byte, error) {
	if id.Page < 0 || id.Slot < 0 {
		return nil, fmt.Errorf("invalid row id %+v", id)
	}
	part, err := valuePart(v)
	if err != nil {
		return nil, err
	}
	key := make([]byte, len(part)+secondaryRowIDSize)
	copy(key, part)
	binary.BigEndian.PutUint32(key[len(part):], uint32(id.Page))
	binary.BigEndian.PutUint32(key[len(part)+4:], uint32(id.Slot))
	return key, nil
}

// SecondaryEqualRange returns the half-open key range [lo, hi) that holds every
// entry of v and no other. For a predicate: col = v is [lo, hi); col >= v and
// col < v use lo as their bound; col > v and col <= v use hi.
func SecondaryEqualRange(v any) (lo, hi []byte, err error) {
	part, err := valuePart(v)
	if err != nil {
		return nil, nil, err
	}
	hi = make([]byte, 0, len(part)+secondaryRowIDSize+1)
	hi = append(hi, part...)
	for i := 0; i < secondaryRowIDSize; i++ {
		hi = append(hi, 0xFF)
	}
	hi = append(hi, 0x00)
	return part, hi, nil
}

// SecondaryKeyLen returns the length of the secondary key for v: the value part
// plus the row id. A string with zero bytes is longer than its Go length.
func SecondaryKeyLen(v any) (int, error) {
	part, err := valuePart(v)
	if err != nil {
		return 0, err
	}
	return len(part) + secondaryRowIDSize, nil
}
