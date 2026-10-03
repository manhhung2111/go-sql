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
	store     *catalogStore
}

func NewCatalog(dataDir DataDir) (Catalog, error) {
	files, err := newFileAllocator(dataDir)
	if err != nil {
		return nil, err
	}
	store, err := openCatalogStore(files)
	if err != nil {
		return nil, err
	}
	loaded, err := store.load()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("loading catalog: %w", err)
	}
	c := &SqlCatalog{Databases: make(map[string]Database, len(loaded.databases)), store: store}
	for name, tables := range loaded.databases {
		c.Databases[name] = newLoadedDatabase(name, store, tables)
	}

	// Every survivor is open; whatever no survivor refers to is an orphan. The
	// allocator scanned ids before this, so a removed orphan's id is never
	// handed out again.
	databases := make(map[string]bool, len(loaded.databases))
	referenced := make(map[int64]bool)
	for name, tables := range loaded.databases {
		databases[name] = true
		for _, table := range tables {
			referenced[table.fileID] = true
		}
	}
	removeOrphans(files, databases, referenced)
	return c, nil
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

	db, err := NewDatabase(name, c.store)
	if err != nil {
		return err
	}

	c.Databases[name] = db
	return nil
}

// DropDatabase commits by tombstoning the database's catalog row; only then
// does the database leave memory and its files go. A failure removing the
// files leaves orphans, never a database that points at missing ones.
func (c *SqlCatalog) DropDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	db, exists := c.Databases[name]
	if !exists {
		return fmt.Errorf("database %q does not exist", name)
	}

	// The database row is the commit point. If it cannot be tombstoned the
	// database is untouched.
	if err := c.store.removeDatabase(name); err != nil {
		return fmt.Errorf("dropping database %q: %w", name, err)
	}
	delete(c.Databases, name)

	// From here the database is gone; what is left is clean-up.
	if err := errors.Join(c.store.removeDatabaseTables(name), db.Drop()); err != nil {
		return fmt.Errorf("database %q dropped, but cleaning up after it failed: %w", name, err)
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
	errs = append(errs, c.store.Close())
	return errors.Join(errs...)
}
