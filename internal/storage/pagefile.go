package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// PageFile is a file of 16KiB pages addressed by page number. It verifies and
// stamps only the format-neutral page header (checksum at 0:4, page number at
// 4:8); everything after is the owner's to lay out.
//
// Any number of ReadPage calls may run concurrently with one another. WritePage,
// AllocatePage, Sync and Close must not run concurrently with anything else, so
// the owner serializes them.
type PageFile interface {
	// ReadPage returns page n after verifying its size, checksum and page
	// number. The returned buffer is read-only: copy it to modify it.
	ReadPage(n int32) ([]byte, error)
	// WritePage stamps page number n and the checksum into a copy of buf and
	// writes it through. It does not fsync. buf must be exactly one page.
	// n may be NumPages(), which appends the page; n beyond that is an error.
	WritePage(n int32, buf []byte) error
	// AllocatePage appends an all-zero payload page with a valid header and
	// returns its number. It is for owners whose blank page is valid; the heap
	// appends with WritePage instead.
	AllocatePage() (int32, error)
	NumPages() int32
	Sync() error
	Close() error
}

type osPageFile struct {
	file     *os.File
	numPages int32
}

// CreatePageFile makes a new, empty page file. It fails if path already
// exists, so a new file can never silently pick up an old file's pages.
func CreatePageFile(path string) (PageFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating file %q: %w", path, err)
	}
	if err := SyncDir(filepath.Dir(path)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("creating file %q: %w", path, err)
	}
	return &osPageFile{file: f}, nil
}

// OpenPageFile opens an existing page file. A size that is not a whole number
// of pages means a torn or corrupt file.
func OpenPageFile(path string) (PageFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening file %q: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat file %q: %w", path, err)
	}
	if info.Size()%maxPageSize != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("file %q is %d bytes, not a multiple of the %d-byte page size (torn or corrupt)", path, info.Size(), maxPageSize)
	}

	return &osPageFile{file: f, numPages: int32(info.Size() / maxPageSize)}, nil
}

// stampPage sets buf's page number and then its checksum, which covers
// everything after its own 4 bytes. buf must be one page.
func stampPage(buf []byte, n int32) {
	binary.LittleEndian.PutUint32(buf[pageNumberPos:], uint32(n))
	binary.LittleEndian.PutUint32(buf[checksumPos:], crc32.ChecksumIEEE(buf[pageNumberPos:]))
}

func (p *osPageFile) ReadPage(n int32) ([]byte, error) {
	if n < 0 || n >= p.numPages {
		return nil, fmt.Errorf("page %d out of range [0, %d)", n, p.numPages)
	}

	buf := make([]byte, maxPageSize)
	// ReadAt may return io.EOF alongside a full read at the end of the file.
	if got, err := p.file.ReadAt(buf, int64(n)*maxPageSize); got != len(buf) && err != nil {
		return nil, fmt.Errorf("reading page %d: %w", n, err)
	}

	want := binary.LittleEndian.Uint32(buf[checksumPos:])
	if got := crc32.ChecksumIEEE(buf[pageNumberPos:]); got != want {
		return nil, fmt.Errorf("page %d: checksum mismatch: stored %#x, computed %#x", n, want, got)
	}
	if got := int32(binary.LittleEndian.Uint32(buf[pageNumberPos:])); got != n {
		return nil, fmt.Errorf("page %d: header says page %d", n, got)
	}
	return buf, nil
}

func (p *osPageFile) WritePage(n int32, buf []byte) error {
	if len(buf) != maxPageSize {
		return fmt.Errorf("page is %d bytes, want %d", len(buf), maxPageSize)
	}
	if n < 0 || n > p.numPages {
		return fmt.Errorf("page %d out of range [0, %d]", n, p.numPages)
	}

	out := append([]byte(nil), buf...)
	stampPage(out, n)
	if _, err := p.file.WriteAt(out, int64(n)*maxPageSize); err != nil {
		return fmt.Errorf("writing page %d: %w", n, err)
	}
	if n == p.numPages {
		p.numPages++
	}
	return nil
}

func (p *osPageFile) AllocatePage() (int32, error) {
	n := p.numPages
	if err := p.WritePage(n, make([]byte, maxPageSize)); err != nil {
		return 0, err
	}
	return n, nil
}

func (p *osPageFile) NumPages() int32 { return p.numPages }

func (p *osPageFile) Sync() error {
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	return nil
}

func (p *osPageFile) Close() error {
	if err := p.file.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	return nil
}
