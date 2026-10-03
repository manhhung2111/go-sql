package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// removeOrphans deletes what no surviving catalog row refers to. Ids are
// global and never reused, so a table file is an orphan exactly when no
// survivor has its id, whatever directory it sits in; a directory name is
// never trusted, because on a case-insensitive filesystem two databases can
// share one. Only files named <number>.tbl are removed, and a directory is
// removed only if it is empty and no database has its exact name, with a
// non-recursive os.Remove: it can never take a file it did not create. A
// removed directory that a database still needs is recreated by its next
// CREATE TABLE.
//
// Failures are ignored: an orphan wastes space and nothing else.
func removeOrphans(files *fileAllocator, databases map[string]bool, referenced map[int64]bool) {
	root := filepath.Join(string(files.dataDir), "data")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		children, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		remaining := 0
		for _, child := range children {
			if child.IsDir() {
				remaining++
				continue
			}
			idText, isTable := strings.CutSuffix(child.Name(), tableFileExt)
			id, parseErr := strconv.ParseInt(idText, 10, 64)
			if !isTable || parseErr != nil || referenced[id] {
				remaining++
				continue
			}
			if os.Remove(filepath.Join(dir, child.Name())) != nil {
				remaining++
			}
		}
		if remaining == 0 && !databases[entry.Name()] {
			_ = os.Remove(dir)
		}
	}
}
