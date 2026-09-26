package engine

import (
	"fmt"
	"manhhung2111/go-sql/internal/parser"
	"sync"
)

type Database interface {
	GetTable(name string) (Table, bool)
	CreateTable(name string, columns []parser.ColumnDefinition, ifNotExists bool) error
}

type SqlDatabase struct {
	mu    sync.RWMutex
	Name  string
	Table map[string]Table
}

func NewDatabase(name string) Database {
	return &SqlDatabase{
		Name:  name,
		Table: make(map[string]Table),
	}
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

	if _, exits := d.Table[name]; exits {
		if !ifNotExists {
			return fmt.Errorf("table %q already exists", name)
		}
		return nil
	}

	d.Table[name] = NewTable(name, columns)
	return nil
}
