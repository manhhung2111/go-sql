package engine

import (
	"fmt"
	"sort"
	"sync"
)

type Catalog interface {
	GetDatabase(name string) (Database, error)
	ListDatabases() []string
	CreateDatabase(name string) error
	DropDatabase(name string) error
}

type SqlCatalog struct {
	mu        sync.RWMutex
	Databases map[string]Database
}

func NewCatalog() Catalog {
	return &SqlCatalog{
		Databases: make(map[string]Database),
	}
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

	c.Databases[name] = NewDatabase(name)
	return nil
}

func (c *SqlCatalog) DropDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.Databases[name]; !exists {
		return fmt.Errorf("database %q does not exist", name)
	}

	delete(c.Databases, name)
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
