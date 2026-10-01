package storage

import (
	"fmt"
	"iter"
	"os"
)

// RowID addresses a row by its page and slot. It stays valid for the row's
// lifetime: a delete tombstones the slot rather than shifting its neighbours.
type RowID struct {
	Page int32
	Slot int
}

// Row is one live row yielded by Scan.
type Row struct {
	ID RowID
	// Bytes is only valid until the next iteration; copy it to keep it.
	Bytes []byte
}

// File is a heap file: a sequence of 16KiB slotted pages. Insert and Delete
// write the affected page straight through but do not fsync — the caller
// calls Sync once at the end of a statement, so a many-row statement pays
// for one fsync rather than one per row.
//
// A File is not safe for concurrent use; the owning table's lock serializes
// access.
type File interface {
	Insert(rowBytes []byte) (RowID, error)
	Delete(id RowID) error
	// Scan yields every live row in page order, verifying each page's
	// checksum as it goes. A corrupt page ends the iteration with its error.
	Scan() iter.Seq2[Row, error]
	Sync() error
	Close() error
}

type sqlFile struct {
	file     *os.File
	numPages int32
	// tail caches the last page. Every write goes straight to disk, so the
	// cache always equals what is on disk; it is nil when not loaded.
	tail Page
}

// CreateFile makes a new, empty heap file. It fails if path already exists,
// so a new table can never silently pick up an old file's rows.
func CreateFile(path string) (File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating file %q: %w", path, err)
	}
	return &sqlFile{file: f}, nil
}

// OpenFile opens an existing heap file. A size that is not a whole number of
// pages means a torn or corrupt file.
func OpenFile(path string) (File, error) {
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

	return &sqlFile{file: f, numPages: int32(info.Size() / maxPageSize)}, nil
}

func (f *sqlFile) readPage(pageNumber int32) (Page, error) {
	buf := make([]byte, maxPageSize)
	// ReadAt may return io.EOF alongside a full read at the end of the file.
	if n, err := f.file.ReadAt(buf, int64(pageNumber)*maxPageSize); n != len(buf) && err != nil {
		return nil, fmt.Errorf("reading page %d: %w", pageNumber, err)
	}

	page, err := DecodePage(buf)
	if err != nil {
		return nil, fmt.Errorf("page %d: %w", pageNumber, err)
	}
	if got := page.PageNumber(); got != pageNumber {
		return nil, fmt.Errorf("page %d: header says page %d", pageNumber, got)
	}
	return page, nil
}

func (f *sqlFile) writePage(pageNumber int32, page Page) error {
	if _, err := f.file.WriteAt(page.Encode(), int64(pageNumber)*maxPageSize); err != nil {
		return fmt.Errorf("writing page %d: %w", pageNumber, err)
	}
	return nil
}

// loadTail fills the tail cache from disk if it is empty and the file has
// any pages.
func (f *sqlFile) loadTail() error {
	if f.tail != nil || f.numPages == 0 {
		return nil
	}
	page, err := f.readPage(f.numPages - 1)
	if err != nil {
		return err
	}
	f.tail = page
	return nil
}

func (f *sqlFile) Insert(rowBytes []byte) (RowID, error) {
	if len(rowBytes) > maxRowSize {
		return RowID{}, fmt.Errorf("row of %d bytes exceeds max row size of %d bytes", len(rowBytes), maxRowSize)
	}

	if err := f.loadTail(); err != nil {
		return RowID{}, err
	}

	if f.tail != nil {
		slot, err := f.tail.InsertRow(rowBytes)
		if err == nil {
			tailNumber := f.numPages - 1
			if err := f.writePage(tailNumber, f.tail); err != nil {
				// The cached page now holds a row that never reached disk;
				// drop it so the next operation reloads what is really there.
				f.tail = nil
				return RowID{}, err
			}
			return RowID{Page: tailNumber, Slot: slot}, nil
		}
		// The tail is full (a failed InsertRow changes nothing): fall
		// through to a fresh page.
	}

	page := NewPage(f.numPages)
	slot, err := page.InsertRow(rowBytes)
	if err != nil {
		return RowID{}, fmt.Errorf("row too large to fit in a page: %w", err)
	}
	if err := f.writePage(f.numPages, page); err != nil {
		return RowID{}, err
	}

	id := RowID{Page: f.numPages, Slot: slot}
	f.tail = page
	f.numPages++
	return id, nil
}

func (f *sqlFile) Delete(id RowID) error {
	if id.Page < 0 || id.Page >= f.numPages {
		return fmt.Errorf("page %d out of range [0, %d)", id.Page, f.numPages)
	}

	isTail := id.Page == f.numPages-1
	var page Page
	if isTail {
		if err := f.loadTail(); err != nil {
			return err
		}
		page = f.tail
	} else {
		var err error
		if page, err = f.readPage(id.Page); err != nil {
			return err
		}
	}

	if err := page.DeleteRow(id.Slot); err != nil {
		return fmt.Errorf("page %d: %w", id.Page, err)
	}
	if err := f.writePage(id.Page, page); err != nil {
		if isTail {
			f.tail = nil // see Insert: don't keep a change that never reached disk
		}
		return err
	}
	return nil
}

func (f *sqlFile) Scan() iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		// Snapshot the page count: rows inserted while a scan is running
		// are not part of it.
		numPages := f.numPages
		for pageNumber := int32(0); pageNumber < numPages; pageNumber++ {
			page, err := f.readPage(pageNumber)
			if err != nil {
				yield(Row{}, err)
				return
			}
			for slot := 0; slot < page.SlotCount(); slot++ {
				rowBytes, live := page.Row(slot)
				if !live {
					continue
				}
				if !yield(Row{ID: RowID{Page: pageNumber, Slot: slot}, Bytes: rowBytes}, nil) {
					return
				}
			}
		}
	}
}

func (f *sqlFile) Sync() error {
	if err := f.file.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	return nil
}

// Close closes the underlying file. There is nothing to flush: every write
// already went straight to disk.
func (f *sqlFile) Close() error {
	if err := f.file.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	return nil
}
