package storage

import (
	"fmt"
	"iter"
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
// Any number of Scans may run concurrently with one another: a scan only
// reads. Insert, Delete, Sync and Close must not run concurrently with
// anything else, so the owning table takes its write lock for them and only
// its read lock for a scan.
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
	pages PageFile
	// tail caches the last page. Every write goes straight to disk, so the
	// cache always equals what is on disk; it is nil when not loaded.
	tail Page
}

// CreateFile makes a new, empty heap file. It fails if path already exists,
// so a new table can never silently pick up an old file's rows.
func CreateFile(path string) (File, error) {
	pages, err := CreatePageFile(path)
	if err != nil {
		return nil, err
	}
	return &sqlFile{pages: pages}, nil
}

// OpenFile opens an existing heap file. A size that is not a whole number of
// pages means a torn or corrupt file.
func OpenFile(path string) (File, error) {
	pages, err := OpenPageFile(path)
	if err != nil {
		return nil, err
	}
	return &sqlFile{pages: pages}, nil
}

// readPage returns page n as a private, mutable copy: the page file's buffer
// is read-only, and the tail and Delete paths change the page they read.
func (f *sqlFile) readPage(n int32) (Page, error) {
	buf, err := f.pages.ReadPage(n)
	if err != nil {
		return nil, err
	}
	if err := validateLayout(buf); err != nil {
		return nil, fmt.Errorf("page %d: %w", n, err)
	}
	return &sqlSlottedPage{buf: append([]byte(nil), buf...)}, nil
}

// loadTail fills the tail cache from disk if it is empty and the file has
// any pages.
func (f *sqlFile) loadTail() error {
	if f.tail != nil || f.pages.NumPages() == 0 {
		return nil
	}
	page, err := f.readPage(f.pages.NumPages() - 1)
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
			tailNumber := f.pages.NumPages() - 1
			if err := f.pages.WritePage(tailNumber, f.tail.Encode()); err != nil {
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

	newNumber := f.pages.NumPages()
	page := NewPage(newNumber)
	slot, err := page.InsertRow(rowBytes)
	if err != nil {
		return RowID{}, fmt.Errorf("row too large to fit in a page: %w", err)
	}
	if err := f.pages.WritePage(newNumber, page.Encode()); err != nil {
		return RowID{}, err
	}

	f.tail = page
	return RowID{Page: newNumber, Slot: slot}, nil
}

func (f *sqlFile) Delete(id RowID) error {
	numPages := f.pages.NumPages()
	if id.Page < 0 || id.Page >= numPages {
		return fmt.Errorf("page %d out of range [0, %d)", id.Page, numPages)
	}

	isTail := id.Page == numPages-1
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
	if err := f.pages.WritePage(id.Page, page.Encode()); err != nil {
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
		numPages := f.pages.NumPages()
		for pageNumber := int32(0); pageNumber < numPages; pageNumber++ {
			// A scan only reads, so it views the page buffer in place instead
			// of copying it; the buffer is fresh per read and never mutated.
			buf, err := f.pages.ReadPage(pageNumber)
			if err == nil {
				if err = validateLayout(buf); err != nil {
					err = fmt.Errorf("page %d: %w", pageNumber, err)
				}
			}
			if err != nil {
				yield(Row{}, err)
				return
			}
			page := &sqlSlottedPage{buf: buf}
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

func (f *sqlFile) Sync() error { return f.pages.Sync() }

// Close closes the underlying file. There is nothing to flush: every write
// already went straight to disk.
func (f *sqlFile) Close() error { return f.pages.Close() }
