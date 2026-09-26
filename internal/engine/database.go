package engine

import (
	"fmt"
	"manhhung2111/go-sql/internal/parser"
	"sync"
)

type Database interface {
	GetTable(name string) (Table, bool)
	CreateTable(name string, columns []parser.ColumnDefinition, ifNotExists bool) error
	RenameTable(oldName, newName string) error
	DropTable(name string, ifExists bool) error
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

	table, err := NewTable(name, columns)
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

func (d *SqlDatabase) DropTable(name string, ifExists bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.Table[name]; !exists {
		if ifExists {
			return nil
		}
		return fmt.Errorf("table %q does not exist", name)
	}

	delete(d.Table, name)
	return nil
}
