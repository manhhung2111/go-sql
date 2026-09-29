package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

const (
	maxPageSize    = 1 << 14 // 16KiB
	pageHeaderSize = 4 + 4 + 2 + 2
	slotSize       = 2 + 2

	// maxRowSize is the largest a single row's encoded bytes can ever be:
	// a completely empty page, minus its header, minus the one slot that
	// row itself would need.
	maxRowSize = maxPageSize - pageHeaderSize - slotSize
)

type Page interface {
	InsertRow(rowBytes []byte) (int, error)
	Encode() []byte
}

func NewPage(pageNumber int32) Page {
	return &sqlSlottedPage{
		PageHeader: sqlPageHeader{PageNumber: pageNumber, DataStartPos: maxPageSize},
		PageData:   sqlPageData{},
	}
}

type sqlSlottedPage struct {
	PageHeader sqlPageHeader
	PageData   sqlPageData
}

type sqlPageHeader struct {
	CheckSum     uint32
	PageNumber   int32
	RowCount     uint16
	DataStartPos uint16
}

type sqlPageData struct {
	Slots []sqlSlot
	Data  []byte
}

type sqlSlot struct {
	Offset uint16
	Length uint16
}

func (s *sqlSlottedPage) InsertRow(rowBytes []byte) (int, error) {
	rowLen := len(rowBytes)
	if int(s.PageHeader.DataStartPos)-pageHeaderSize-(int(s.PageHeader.RowCount)+1)*slotSize < rowLen {
		return -1, fmt.Errorf("not enough space to insert row")
	}

	slotIndex := int(s.PageHeader.RowCount)

	s.PageHeader.RowCount++
	s.PageHeader.DataStartPos -= uint16(rowLen)

	s.PageData.Slots = append(s.PageData.Slots, sqlSlot{Offset: s.PageHeader.DataStartPos, Length: uint16(rowLen)})
	s.PageData.Data = append(s.PageData.Data, rowBytes...)

	return slotIndex, nil
}

func (s *sqlSlottedPage) Encode() []byte {
	data := make([]byte, maxPageSize)

	binary.LittleEndian.PutUint32(data[4:8], uint32(s.PageHeader.PageNumber))
	binary.LittleEndian.PutUint16(data[8:10], s.PageHeader.RowCount)
	binary.LittleEndian.PutUint16(data[10:12], s.PageHeader.DataStartPos)

	dataCursor := 0
	for i, slot := range s.PageData.Slots {
		off := pageHeaderSize + i*slotSize
		binary.LittleEndian.PutUint16(data[off:off+2], slot.Offset)
		binary.LittleEndian.PutUint16(data[off+2:off+4], slot.Length)

		rowBytes := s.PageData.Data[dataCursor : dataCursor+int(slot.Length)]
		copy(data[slot.Offset:], rowBytes)
		dataCursor += int(slot.Length)
	}

	// CheckSum is written last, over everything after its own 4 bytes,
	// so it can cover the header/slots/data in one contiguous range.
	checksum := crc32.ChecksumIEEE(data[4:])
	binary.LittleEndian.PutUint32(data[0:4], checksum)

	return data
}
