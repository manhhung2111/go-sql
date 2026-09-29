package storage

import (
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPage(t *testing.T) {
	p := NewPage(7).(*sqlSlottedPage)

	assert.Equal(t, int32(7), p.PageHeader.PageNumber)
	assert.Equal(t, uint16(0), p.PageHeader.RowCount)
	assert.Equal(t, uint16(maxPageSize), p.PageHeader.DataStartPos)
	assert.Empty(t, p.PageData.Slots)
	assert.Empty(t, p.PageData.Data)
}

func TestSqlSlottedPage_InsertRow(t *testing.T) {
	t.Run("single row", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)
		row := []byte("hello")

		idx, err := p.InsertRow(row)

		require.NoError(t, err)
		assert.Equal(t, 0, idx)
		assert.Equal(t, uint16(1), p.PageHeader.RowCount)
		assert.Equal(t, uint16(maxPageSize-len(row)), p.PageHeader.DataStartPos)
		require.Len(t, p.PageData.Slots, 1)
		assert.Equal(t, sqlSlot{Offset: uint16(maxPageSize - len(row)), Length: uint16(len(row))}, p.PageData.Slots[0])
		assert.Equal(t, row, p.PageData.Data)
	})

	t.Run("multiple rows get sequential slot indices, each closer to the header than the last", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)
		rows := [][]byte{[]byte("first"), []byte("second"), []byte("third")}

		for i, row := range rows {
			idx, err := p.InsertRow(row)
			require.NoError(t, err)
			assert.Equal(t, i, idx)
		}

		require.Len(t, p.PageData.Slots, 3)
		assert.Greater(t, p.PageData.Slots[0].Offset, p.PageData.Slots[1].Offset)
		assert.Greater(t, p.PageData.Slots[1].Offset, p.PageData.Slots[2].Offset)

		var wantData []byte
		for _, row := range rows {
			wantData = append(wantData, row...)
		}
		assert.Equal(t, wantData, p.PageData.Data)
	})

	t.Run("row exactly filling remaining space succeeds, one more byte overflows", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)

		_, err := p.InsertRow(make([]byte, 100))
		require.NoError(t, err)

		// maxNextRowSize is the largest row that could still be inserted right
		// now: it must reserve room for that row's own new slot entry, on top
		// of what earlier rows' slots and data already occupy.
		maxNextRowSize := func() int {
			return int(p.PageHeader.DataStartPos) - pageHeaderSize - (int(p.PageHeader.RowCount)+1)*slotSize
		}
		// trueFreeSpace is what's left with no pending candidate row —
		// no "+1" slot to reserve since nothing is being inserted.
		trueFreeSpace := func() int {
			return int(p.PageHeader.DataStartPos) - pageHeaderSize - int(p.PageHeader.RowCount)*slotSize
		}

		_, err = p.InsertRow(make([]byte, maxNextRowSize()))
		require.NoError(t, err)
		assert.Equal(t, 0, trueFreeSpace())

		_, err = p.InsertRow(make([]byte, 1))
		assert.Error(t, err)
	})

	t.Run("a failed insert leaves the page state unchanged", func(t *testing.T) {
		p := NewPage(0).(*sqlSlottedPage)
		_, err := p.InsertRow(make([]byte, 100))
		require.NoError(t, err)

		wantHeader := p.PageHeader
		wantSlots := append([]sqlSlot{}, p.PageData.Slots...)
		wantData := append([]byte{}, p.PageData.Data...)

		_, err = p.InsertRow(make([]byte, maxPageSize))
		require.Error(t, err)

		assert.Equal(t, wantHeader, p.PageHeader)
		assert.Equal(t, wantSlots, p.PageData.Slots)
		assert.Equal(t, wantData, p.PageData.Data)
	})
}

func TestSqlSlottedPage_Encode(t *testing.T) {
	t.Run("empty page", func(t *testing.T) {
		p := NewPage(3).(*sqlSlottedPage)

		data := p.Encode()

		require.Len(t, data, maxPageSize)
		assert.Equal(t, uint32(3), binary.LittleEndian.Uint32(data[4:8]))
		assert.Equal(t, uint16(0), binary.LittleEndian.Uint16(data[8:10]))
		assert.Equal(t, uint16(maxPageSize), binary.LittleEndian.Uint16(data[10:12]))

		wantChecksum := crc32.ChecksumIEEE(data[4:])
		assert.Equal(t, wantChecksum, binary.LittleEndian.Uint32(data[0:4]))
	})

	t.Run("rows land at their own recorded offsets, not insertion order", func(t *testing.T) {
		p := NewPage(1).(*sqlSlottedPage)
		rows := [][]byte{[]byte("alpha"), []byte("bravo-charlie")}
		for _, row := range rows {
			_, err := p.InsertRow(row)
			require.NoError(t, err)
		}

		data := p.Encode()

		for i, slot := range p.PageData.Slots {
			off := pageHeaderSize + i*slotSize
			assert.Equal(t, slot.Offset, binary.LittleEndian.Uint16(data[off:off+2]), "slot %d offset", i)
			assert.Equal(t, slot.Length, binary.LittleEndian.Uint16(data[off+2:off+4]), "slot %d length", i)

			got := data[slot.Offset : int(slot.Offset)+int(slot.Length)]
			assert.Equal(t, rows[i], got, "row %d bytes at its slot offset", i)
		}

		wantChecksum := crc32.ChecksumIEEE(data[4:])
		assert.Equal(t, wantChecksum, binary.LittleEndian.Uint32(data[0:4]))
	})
}
