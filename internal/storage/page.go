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

	// MaxRowSize is maxRowSize for callers outside this package: the largest
	// row Insert will accept, so a caller can reject an oversized row before
	// writing the first row of a batch.
	MaxRowSize = maxRowSize

	// Header field positions. The header is little-endian:
	// checksum(0:4) | page number(4:8) | slot count(8:10) | data start(10:12).
	checksumPos   = 0
	pageNumberPos = 4
	slotCountPos  = 8
	dataStartPos  = 10
)

// Page is a slotted page: a slot array grows forward from the header while
// row data grows backward from the end of the page. A deleted row keeps its
// slot (so every other row's slot index stays stable) with Offset and Length
// set to 0 — a live row can never start at offset 0, since the header
// occupies the first bytes of the page.
type Page interface {
	// InsertRow stores rowBytes and returns the slot it was given.
	InsertRow(rowBytes []byte) (slot int, err error)
	// Row returns the bytes at slot, or false for a tombstoned or
	// out-of-range slot. The result aliases the page buffer; copy it to
	// keep it past the next page mutation.
	Row(slot int) ([]byte, bool)
	// DeleteRow tombstones slot. Its space is not reclaimed.
	DeleteRow(slot int) error
	// SlotCount counts every slot ever allocated, tombstones included.
	SlotCount() int
	// PageNumber is the page's position in its file.
	PageNumber() int32
	// Encode stamps the checksum and returns the page's 16KiB buffer. The
	// buffer is the page's own — it changes with later mutations.
	Encode() []byte
}

// sqlSlottedPage wraps the page's raw on-disk bytes directly, reading and
// writing the header, slots and rows in place at their real positions.
type sqlSlottedPage struct {
	buf []byte
}

func NewPage(pageNumber int32) Page {
	p := &sqlSlottedPage{buf: make([]byte, maxPageSize)}
	binary.LittleEndian.PutUint32(p.buf[pageNumberPos:], uint32(pageNumber))
	p.setDataStart(maxPageSize) // exclusive upper bound: the page end itself
	return p
}

// DecodePage validates data as an encoded page — exact size, matching
// checksum, and a slot array whose live rows all lie inside the row area —
// and returns a Page that owns its own copy of the bytes.
func DecodePage(data []byte) (Page, error) {
	if len(data) != maxPageSize {
		return nil, fmt.Errorf("page is %d bytes, want %d", len(data), maxPageSize)
	}

	want := binary.LittleEndian.Uint32(data[checksumPos:])
	if got := crc32.ChecksumIEEE(data[pageNumberPos:]); got != want {
		return nil, fmt.Errorf("checksum mismatch: stored %#x, computed %#x", want, got)
	}

	p := &sqlSlottedPage{buf: append([]byte(nil), data...)}

	slotArrayEnd := pageHeaderSize + p.SlotCount()*slotSize
	dataStart := p.dataStart()
	if dataStart < slotArrayEnd || dataStart > maxPageSize {
		return nil, fmt.Errorf("data start %d outside [%d, %d]", dataStart, slotArrayEnd, maxPageSize)
	}

	for i := 0; i < p.SlotCount(); i++ {
		offset, length := p.slot(i)
		if offset == 0 {
			if length != 0 {
				return nil, fmt.Errorf("slot %d: tombstone with length %d", i, length)
			}
			continue
		}
		if offset < dataStart || offset+length > maxPageSize {
			return nil, fmt.Errorf("slot %d: row [%d, %d) outside row area [%d, %d)", i, offset, offset+length, dataStart, maxPageSize)
		}
	}

	return p, nil
}

func (s *sqlSlottedPage) PageNumber() int32 {
	return int32(binary.LittleEndian.Uint32(s.buf[pageNumberPos:]))
}

func (s *sqlSlottedPage) SlotCount() int {
	return int(binary.LittleEndian.Uint16(s.buf[slotCountPos:]))
}

func (s *sqlSlottedPage) dataStart() int {
	return int(binary.LittleEndian.Uint16(s.buf[dataStartPos:]))
}

func (s *sqlSlottedPage) setDataStart(v int) {
	binary.LittleEndian.PutUint16(s.buf[dataStartPos:], uint16(v))
}

func (s *sqlSlottedPage) slot(i int) (offset, length int) {
	pos := pageHeaderSize + i*slotSize
	return int(binary.LittleEndian.Uint16(s.buf[pos:])), int(binary.LittleEndian.Uint16(s.buf[pos+2:]))
}

func (s *sqlSlottedPage) setSlot(i, offset, length int) {
	pos := pageHeaderSize + i*slotSize
	binary.LittleEndian.PutUint16(s.buf[pos:], uint16(offset))
	binary.LittleEndian.PutUint16(s.buf[pos+2:], uint16(length))
}

func (s *sqlSlottedPage) InsertRow(rowBytes []byte) (int, error) {
	rowLen := len(rowBytes)
	slotIndex := s.SlotCount()

	// The new row needs its own slot too, on top of the slots and data
	// already there — hence slotIndex+1.
	if s.dataStart()-pageHeaderSize-(slotIndex+1)*slotSize < rowLen {
		return -1, fmt.Errorf("not enough space to insert row")
	}

	newStart := s.dataStart() - rowLen
	copy(s.buf[newStart:], rowBytes)
	s.setSlot(slotIndex, newStart, rowLen)
	s.setDataStart(newStart)
	binary.LittleEndian.PutUint16(s.buf[slotCountPos:], uint16(slotIndex+1))

	return slotIndex, nil
}

func (s *sqlSlottedPage) Row(slot int) ([]byte, bool) {
	if slot < 0 || slot >= s.SlotCount() {
		return nil, false
	}
	offset, length := s.slot(slot)
	if offset == 0 {
		return nil, false
	}
	// Capacity is capped so an append by the caller can't overwrite the
	// neighbouring row's bytes.
	return s.buf[offset : offset+length : offset+length], true
}

func (s *sqlSlottedPage) DeleteRow(slot int) error {
	if slot < 0 || slot >= s.SlotCount() {
		return fmt.Errorf("slot %d out of range [0, %d)", slot, s.SlotCount())
	}
	if offset, _ := s.slot(slot); offset == 0 {
		return fmt.Errorf("slot %d is already deleted", slot)
	}
	s.setSlot(slot, 0, 0)
	return nil
}

// Encode stamps the checksum over everything after its own 4 bytes, so it
// covers the header, slots and rows in one contiguous range.
func (s *sqlSlottedPage) Encode() []byte {
	binary.LittleEndian.PutUint32(s.buf[checksumPos:], crc32.ChecksumIEEE(s.buf[pageNumberPos:]))
	return s.buf
}
