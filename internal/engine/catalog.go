package engine

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

type Catalog interface {
	GetDatabase(name string) (Database, error)
	ListDatabases() []string
	CreateDatabase(name string) error
	DropDatabase(name string) error
	// Close closes every database's table files.
	Close() error
}

type SqlCatalog struct {
	mu        sync.RWMutex
	Databases map[string]Database
	files     *fileAllocator
}

func NewCatalog(dataDir DataDir) (Catalog, error) {
	files, err := newFileAllocator(dataDir)
	if err != nil {
		return nil, err
	}
	return &SqlCatalog{
		Databases: make(map[string]Database),
		files:     files,
	}, nil
}

// ListDatabases returns database names in sorted order — map iteration
// order is randomized, and callers need deterministic output.
func (c *SqlCatalog) ListDatabases() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	databases := make([]string, 0, len(c.Databases))
	for name := range c.Databases {
		databases = append(databases, name)
	}

	sort.Strings(databases)
	return databases
}

func (c *SqlCatalog) CreateDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.Databases[name]; exists {
		return fmt.Errorf("database %q already exists", name)
	}

	db, err := NewDatabase(name, c.files)
	if err != nil {
		return err
	}

	c.Databases[name] = db
	return nil
}

// DropDatabase removes the database from the catalog first, then deletes its
// files: a failure removing them leaves orphan files, never a database that
// points at missing ones.
func (c *SqlCatalog) DropDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	db, exists := c.Databases[name]
	if !exists {
		return fmt.Errorf("database %q does not exist", name)
	}

	delete(c.Databases, name)
	if err := db.Drop(); err != nil {
		return fmt.Errorf("database %q dropped, but removing its files failed: %w", name, err)
	}
	return nil
}

func (c *SqlCatalog) GetDatabase(name string) (Database, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	db, exists := c.Databases[name]
	if !exists {
		return nil, fmt.Errorf("database %q does not exist", name)
	}
	return db, nil
}

func (c *SqlCatalog) Close() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var errs []error
	for name, db := range c.Databases {
		if err := db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("database %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
