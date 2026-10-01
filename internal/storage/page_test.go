package storage

import (
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restamp recomputes data's checksum, so a test can corrupt the structure
// of an encoded page and still have it pass the checksum check — isolating
// the structural validation under test.
func restamp(data []byte) {
	binary.LittleEndian.PutUint32(data[checksumPos:], crc32.ChecksumIEEE(data[pageNumberPos:]))
}

func TestNewPage(t *testing.T) {
	p := NewPage(7).(*sqlSlottedPage)

	assert.Equal(t, int32(7), p.PageNumber())
	assert.Equal(t, 0, p.SlotCount())
	assert.Equal(t, maxPageSize, p.dataStart())
}

func TestSqlSlottedPage_InsertRow(t *testing.T) {
	t.Run("single row", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)
		row := []byte("hello")

		slot, err := p.InsertRow(row)

		require.NoError(t, err)
		assert.Equal(t, 0, slot)
		assert.Equal(t, 1, p.SlotCount())
		assert.Equal(t, maxPageSize-len(row), p.dataStart())

		offset, length := p.slot(0)
		assert.Equal(t, maxPageSize-len(row), offset)
		assert.Equal(t, len(row), length)

		got, ok := p.Row(0)
		require.True(t, ok)
		assert.Equal(t, row, got)
	})

	t.Run("multiple rows get sequential slots, each closer to the header than the last", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)
		rows := [][]byte{[]byte("first"), []byte("second"), []byte("third")}

		for i, row := range rows {
			slot, err := p.InsertRow(row)
			require.NoError(t, err)
			assert.Equal(t, i, slot)
		}

		for i, row := range rows {
			got, ok := p.Row(i)
			require.True(t, ok, "slot %d", i)
			assert.Equal(t, row, got, "slot %d", i)
		}

		offset0, _ := p.slot(0)
		offset1, _ := p.slot(1)
		offset2, _ := p.slot(2)
		assert.Greater(t, offset0, offset1)
		assert.Greater(t, offset1, offset2)
	})

	t.Run("an empty row is a live row, not a tombstone", func(t *testing.T) {
		p := NewPage(0)

		slot, err := p.InsertRow(nil)
		require.NoError(t, err)

		got, ok := p.Row(slot)
		assert.True(t, ok)
		assert.Empty(t, got)
	})

	t.Run("row exactly filling remaining space succeeds, one more byte overflows", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)

		_, err := p.InsertRow(make([]byte, 100))
		require.NoError(t, err)

		// The largest row that still fits must also reserve room for its
		// own new slot entry, on top of what earlier rows already occupy.
		maxNextRowSize := p.dataStart() - pageHeaderSize - (p.SlotCount()+1)*slotSize
		_, err = p.InsertRow(make([]byte, maxNextRowSize))
		require.NoError(t, err)

		// With that row in, the slot array and the row area meet exactly.
		assert.Equal(t, p.dataStart(), pageHeaderSize+p.SlotCount()*slotSize)

		_, err = p.InsertRow(nil)
		assert.Error(t, err, "even an empty row needs a slot")
	})

	t.Run("a failed insert leaves the page unchanged", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)
		_, err := p.InsertRow(make([]byte, 100))
		require.NoError(t, err)
		want := append([]byte(nil), p.Encode()...)

		_, err = p.InsertRow(make([]byte, maxPageSize))
		require.Error(t, err)

		assert.Equal(t, want, p.Encode())
	})
}

func TestSqlSlottedPage_Row(t *testing.T) {
	t.Run("out-of-range slots are not live", func(t *testing.T) {
		p := NewPage(0)
		_, err := p.InsertRow([]byte("x"))
		require.NoError(t, err)

		for _, slot := range []int{-1, 1, 100} {
			_, ok := p.Row(slot)
			assert.False(t, ok, "slot %d", slot)
		}
	})

	t.Run("appending to a returned row cannot overwrite its neighbour", func(t *testing.T) {
		p := NewPage(0)
		_, err := p.InsertRow([]byte("aaaa"))
		require.NoError(t, err)
		_, err = p.InsertRow([]byte("bbbb"))
		require.NoError(t, err)

		first, _ := p.Row(0)
		_ = append(first, 'Z', 'Z', 'Z', 'Z')

		second, _ := p.Row(1)
		assert.Equal(t, []byte("bbbb"), second)
		first, _ = p.Row(0)
		assert.Equal(t, []byte("aaaa"), first)
	})
}

func TestSqlSlottedPage_DeleteRow(t *testing.T) {
	t.Run("tombstones the slot and leaves every other row alone", func(t *testing.T) {
		p := NewPage(0)
		for _, row := range []string{"one", "two", "three"} {
			_, err := p.InsertRow([]byte(row))
			require.NoError(t, err)
		}

		require.NoError(t, p.DeleteRow(1))

		assert.Equal(t, 3, p.SlotCount(), "tombstones keep their slot")
		_, ok := p.Row(1)
		assert.False(t, ok)
		got, ok := p.Row(0)
		require.True(t, ok)
		assert.Equal(t, []byte("one"), got)
		got, ok = p.Row(2)
		require.True(t, ok)
		assert.Equal(t, []byte("three"), got)
	})

	t.Run("deleting twice, or out of range, is an error", func(t *testing.T) {
		p := NewPage(0)
		_, err := p.InsertRow([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, p.DeleteRow(0))

		assert.Error(t, p.DeleteRow(0))
		assert.Error(t, p.DeleteRow(1))
		assert.Error(t, p.DeleteRow(-1))
	})

	t.Run("an insert after a delete takes a new slot instead of reusing the old one", func(t *testing.T) {
		p := NewPage(0)
		_, err := p.InsertRow([]byte("old"))
		require.NoError(t, err)
		require.NoError(t, p.DeleteRow(0))

		slot, err := p.InsertRow([]byte("new"))
		require.NoError(t, err)

		assert.Equal(t, 1, slot)
		_, ok := p.Row(0)
		assert.False(t, ok)
	})
}

func TestSqlSlottedPage_Encode(t *testing.T) {
	t.Run("empty page", func(t *testing.T) {
		p := NewPage(3)

		data := p.Encode()

		require.Len(t, data, maxPageSize)
		assert.Equal(t, uint32(3), binary.LittleEndian.Uint32(data[pageNumberPos:]))
		assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(data[slotCountPos:]))
		assert.Equal(t, uint16(maxPageSize), binary.LittleEndian.Uint16(data[dataStartPos:]))
		assert.Equal(t, crc32.ChecksumIEEE(data[pageNumberPos:]), binary.LittleEndian.Uint32(data[checksumPos:]))
	})

	t.Run("rows sit at the offsets their slots record", func(t *testing.T) {
		p := NewPage(1).(*sqlSlottedPage)
		rows := [][]byte{[]byte("alpha"), []byte("bravo-charlie")}
		for _, row := range rows {
			_, err := p.InsertRow(row)
			require.NoError(t, err)
		}

		data := p.Encode()

		for i, row := range rows {
			pos := pageHeaderSize + i*slotSize
			offset := int(binary.LittleEndian.Uint16(data[pos:]))
			length := int(binary.LittleEndian.Uint16(data[pos+2:]))
			assert.Equal(t, len(row), length, "slot %d length", i)
			assert.Equal(t, row, data[offset:offset+length], "row %d bytes at its slot offset", i)
		}
		assert.Equal(t, crc32.ChecksumIEEE(data[pageNumberPos:]), binary.LittleEndian.Uint32(data[checksumPos:]))
	})

	t.Run("a mutation after Encode is covered by the next Encode's checksum", func(t *testing.T) {
		p := NewPage(0)
		_ = p.Encode()
		_, err := p.InsertRow([]byte("later"))
		require.NoError(t, err)

		data := p.Encode()

		assert.Equal(t, crc32.ChecksumIEEE(data[pageNumberPos:]), binary.LittleEndian.Uint32(data[checksumPos:]))
	})
}

func TestDecodePage(t *testing.T) {
	newEncoded := func(t *testing.T) []byte {
		t.Helper()
		p := NewPage(5)
		for _, row := range []string{"alpha", "bravo", "charlie"} {
			_, err := p.InsertRow([]byte(row))
			require.NoError(t, err)
		}
		require.NoError(t, p.DeleteRow(1))
		return append([]byte(nil), p.Encode()...)
	}

	t.Run("round trip keeps rows, tombstones and page number", func(t *testing.T) {
		got, err := DecodePage(newEncoded(t))
		require.NoError(t, err)

		assert.Equal(t, int32(5), got.PageNumber())
		assert.Equal(t, 3, got.SlotCount())
		row, ok := got.Row(0)
		require.True(t, ok)
		assert.Equal(t, []byte("alpha"), row)
		_, ok = got.Row(1)
		assert.False(t, ok)
		row, ok = got.Row(2)
		require.True(t, ok)
		assert.Equal(t, []byte("charlie"), row)
	})

	t.Run("a decoded page can keep taking inserts", func(t *testing.T) {
		got, err := DecodePage(newEncoded(t))
		require.NoError(t, err)

		slot, err := got.InsertRow([]byte("delta"))
		require.NoError(t, err)

		assert.Equal(t, 3, slot)
		row, ok := got.Row(3)
		require.True(t, ok)
		assert.Equal(t, []byte("delta"), row)
	})

	t.Run("the decoded page owns its bytes", func(t *testing.T) {
		data := newEncoded(t)
		got, err := DecodePage(data)
		require.NoError(t, err)

		for i := range data {
			data[i] = 0xFF
		}

		row, ok := got.Row(0)
		require.True(t, ok)
		assert.Equal(t, []byte("alpha"), row)
	})

	t.Run("wrong size", func(t *testing.T) {
		_, err := DecodePage(make([]byte, maxPageSize-1))
		assert.ErrorContains(t, err, "bytes")
	})

	t.Run("a flipped byte fails the checksum", func(t *testing.T) {
		data := newEncoded(t)
		data[maxPageSize-1] ^= 0x01

		_, err := DecodePage(data)

		assert.ErrorContains(t, err, "checksum mismatch")
	})

	// These corrupt the page's structure and re-stamp the checksum, so each
	// one reaches the structural checks rather than the checksum.
	structural := []struct {
		name    string
		corrupt func(data []byte)
		wantErr string
	}{
		{
			name: "data start below the slot array",
			corrupt: func(data []byte) {
				binary.LittleEndian.PutUint16(data[dataStartPos:], pageHeaderSize)
			},
			wantErr: "data start",
		},
		{
			name: "slot count larger than the page can hold",
			corrupt: func(data []byte) {
				binary.LittleEndian.PutUint16(data[slotCountPos:], maxPageSize)
			},
			wantErr: "data start",
		},
		{
			name: "live row starting before the row area",
			corrupt: func(data []byte) {
				binary.LittleEndian.PutUint16(data[pageHeaderSize:], pageHeaderSize+3*slotSize)
			},
			wantErr: "slot 0",
		},
		{
			name: "live row running past the page end",
			corrupt: func(data []byte) {
				binary.LittleEndian.PutUint16(data[pageHeaderSize+2:], 100)
			},
			wantErr: "slot 0",
		},
		{
			name: "tombstone that still has a length",
			corrupt: func(data []byte) {
				binary.LittleEndian.PutUint16(data[pageHeaderSize+slotSize+2:], 3)
			},
			wantErr: "tombstone",
		},
	}
	for _, tc := range structural {
		t.Run(tc.name, func(t *testing.T) {
			data := newEncoded(t)
			tc.corrupt(data)
			restamp(data)

			_, err := DecodePage(data)

			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
