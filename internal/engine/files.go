package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// DataDir is the root directory all table files live under:
// <DataDir>/data/<database>/<file_id>.tbl.
type DataDir string

const tableFileExt = ".tbl"

// fileAllocator hands out table file paths. A table file is named by a
// numeric id, never by the table's name, so renaming a table never touches
// disk. Ids are never reused, and are shared across every database.
type fileAllocator struct {
	dataDir DataDir
	nextID  atomic.Int64
}

// newFileAllocator continues numbering after the highest id already on disk,
// so a server restarted on a populated data directory can never pick an id
// whose file already exists.
func newFileAllocator(dataDir DataDir) (*fileAllocator, error) {
	if dataDir == "" {
		return nil, errors.New("data directory is required")
	}

	maxID, err := maxTableFileID(filepath.Join(string(dataDir), "data"))
	if err != nil {
		return nil, err
	}

	a := &fileAllocator{dataDir: dataDir}
	a.nextID.Store(maxID + 1)
	return a, nil
}

// maxTableFileID returns the highest <id>.tbl id found in any database
// directory under root, or 0 if there are none. Anything that does not look
// like a table file is ignored.
func maxTableFileID(root string) (int64, error) {
	databases, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("scanning data directory %q: %w", root, err)
	}

	var maxID int64
	for _, database := range databases {
		if !database.IsDir() {
			continue
		}
		dir := filepath.Join(root, database.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return 0, fmt.Errorf("scanning database directory %q: %w", dir, err)
		}
		for _, file := range files {
			name, isTableFile := strings.CutSuffix(file.Name(), tableFileExt)
			if !isTableFile {
				continue
			}
			if id, err := strconv.ParseInt(name, 10, 64); err == nil && id > maxID {
				maxID = id
			}
		}
	}
	return maxID, nil
}

func (a *fileAllocator) databaseDir(database string) string {
	return filepath.Join(string(a.dataDir), "data", database)
}

// newTablePath returns a path for a new table file in database's directory,
// with an id no earlier call has returned.
func (a *fileAllocator) newTablePath(database string) string {
	id := a.nextID.Add(1) - 1
	return filepath.Join(a.databaseDir(database), strconv.FormatInt(id, 10)+tableFileExt)
}

// validateDatabaseName rejects names that are not a single, plain directory
// name. The SQL lexer already limits identifiers to letters, digits and
// underscores, but a database name becomes a directory name, so the engine
// does not rely on its caller for that.
func validateDatabaseName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("invalid database name %q", name)
	}
	return nil
}
