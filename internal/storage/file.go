package storage

import (
	"fmt"
	"os"
)

type File interface {
	InsertRow(rowBytes []byte) error
	Close() error
}

type sqlFile struct {
	file        *os.File
	currentPage Page
	nextPageNum int32
}

func NewFile(path string) (File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening file %q: %w", path, err)
	}
	return &sqlFile{file: f}, nil
}

func (f *sqlFile) InsertRow(rowBytes []byte) error {
	if len(rowBytes) > maxRowSize {
		return fmt.Errorf("row of %d bytes exceeds max row size of %d bytes", len(rowBytes), maxRowSize)
	}

	if f.currentPage == nil {
		f.currentPage = NewPage(f.nextPageNum)
		f.nextPageNum++
	}

	if _, err := f.currentPage.InsertRow(rowBytes); err != nil {
		if err := f.flushCurrentPage(); err != nil {
			return err
		}
		f.currentPage = NewPage(f.nextPageNum)
		f.nextPageNum++
		if _, err := f.currentPage.InsertRow(rowBytes); err != nil {
			return fmt.Errorf("row too large to fit in a page: %w", err)
		}
	}
	return nil
}

func (f *sqlFile) flushCurrentPage() error {
	if _, err := f.file.Write(f.currentPage.Encode()); err != nil {
		return fmt.Errorf("writing page %d: %w", f.nextPageNum-1, err)
	}
	return nil
}

// Close flushes the current page, if one has any rows in it, then closes
// the underlying file. Without this, the last page — everything inserted
// since the previous rollover — would never reach disk, since InsertRow
// only flushes a page when a new row forces a rollover to the next one.
func (f *sqlFile) Close() error {
	var flushErr error
	if f.currentPage != nil {
		flushErr = f.flushCurrentPage()
	}

	if closeErr := f.file.Close(); closeErr != nil {
		if flushErr != nil {
			return fmt.Errorf("%w (also failed to close file: %v)", flushErr, closeErr)
		}
		return fmt.Errorf("closing file: %w", closeErr)
	}

	return flushErr
}
