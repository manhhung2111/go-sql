package storage

import (
	"fmt"
	"os"
)

// SyncDir fsyncs the directory at path, so entries created in it survive a
// power loss. A file's own fsync does not cover its directory entry.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening directory %q: %w", path, err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return fmt.Errorf("syncing directory %q: %w", path, syncErr)
	}
	return closeErr
}
