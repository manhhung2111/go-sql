package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"manhhung2111/go-sql/internal/parser"
	"os"
	"sync"
	"syscall"
)

type Database interface {
	GetTable(name string) (Table, bool)
	CreateTable(name string, columns []parser.ColumnDefinition, ifNotExists bool) error
	RenameTable(oldName, newName string) error
	DropTable(name string, ifExists bool) error
	// Close closes every table's file.
	Close() error
	// Drop drops every table, then removes the database's directory if
	// nothing else is left in it.
	Drop() error
}

type SqlDatabase struct {
	mu    sync.RWMutex
	Name  string
	Table map[string]Table
	files *fileAllocator
	// dropped is set by Drop; the database can no longer create tables.
	dropped bool
}

// NewDatabase validates name (it becomes a directory name) and creates the
// database's directory.
func NewDatabase(name string, files *fileAllocator) (Database, error) {
	if err := validateDatabaseName(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(files.databaseDir(name), 0o755); err != nil {
		return nil, fmt.Errorf("creating directory for database %q: %w", name, err)
	}

	return &SqlDatabase{
		Name:  name,
		Table: make(map[string]Table),
		files: files,
	}, nil
}

func (d *SqlDatabase) GetTable(name string) (Table, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	table, exits := d.Table[name]
	return table, exits
}

func (d *SqlDatabase) CreateTable(name string, columns []parser.ColumnDefinition, ifNotExists bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// A handle fetched before DROP DATABASE must not create tables: they
	// would be unreachable, and their file and descriptor would leak.
	if d.dropped {
		return fmt.Errorf("database %q does not exist", d.Name)
	}

	if _, exits := d.Table[name]; exits {
		if !ifNotExists {
			return fmt.Errorf("table %q already exists", name)
		}
		return nil
	}

	// The directory can vanish while the database lives: names that differ
	// only by case share one on a case-insensitive filesystem, and dropping
	// either removes it. Re-create it before every new table file.
	if err := os.MkdirAll(d.files.databaseDir(d.Name), 0o755); err != nil {
		return fmt.Errorf("creating directory for database %q: %w", d.Name, err)
	}

	table, err := NewTable(name, columns, d.files.newTablePath(d.Name))
	if err != nil {
		return err
	}

	d.Table[name] = table
	return nil
}

func (d *SqlDatabase) RenameTable(oldName, newName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	table, exists := d.Table[oldName]
	if !exists {
		return fmt.Errorf("table %q does not exist", oldName)
	}
	if _, exists := d.Table[newName]; exists {
		return fmt.Errorf("table %q already exists", newName)
	}

	delete(d.Table, oldName)
	d.Table[newName] = table
	table.Rename(newName)
	return nil
}

// DropTable removes the table from the database first, then deletes its file:
// if the file cannot be removed the table is still gone and the file is a
// harmless orphan, never a table pointing at a missing file.
func (d *SqlDatabase) DropTable(name string, ifExists bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	table, exists := d.Table[name]
	if !exists {
		if ifExists {
			return nil
		}
		return fmt.Errorf("table %q does not exist", name)
	}

	delete(d.Table, name)
	if err := table.Drop(); err != nil {
		return fmt.Errorf("table %q dropped, but removing its file failed: %w", name, err)
	}
	return nil
}

func (d *SqlDatabase) Close() error {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var errs []error
	for name, table := range d.Table {
		if err := table.Close(); err != nil {
			errs = append(errs, fmt.Errorf("table %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (d *SqlDatabase) Drop() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var errs []error
	for name, table := range d.Table {
		if err := table.Drop(); err != nil {
			errs = append(errs, fmt.Errorf("table %q: %w", name, err))
		}
	}
	d.Table = make(map[string]Table)
	d.dropped = true

	// A non-recursive remove: it refuses a directory that still has files in
	// it. Database names that differ only by case or Unicode form can share a
	// directory on some filesystems, and this must never delete a file it did
	// not create.
	if err := os.Remove(d.files.databaseDir(d.Name)); err != nil &&
		!errors.Is(err, fs.ErrNotExist) &&
		!errors.Is(err, syscall.ENOTEMPTY) &&
		!errors.Is(err, syscall.EEXIST) {
		errs = append(errs, fmt.Errorf("removing directory of database %q: %w", d.Name, err))
	}
	return errors.Join(errs...)
}
