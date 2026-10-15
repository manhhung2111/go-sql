package engine

import (
	"fmt"
	"slices"

	"manhhung2111/go-sql/internal/parser"
)

// indexDef is one CREATE INDEX definition: an index named name on a column,
// by name so a column rename only has to rewrite the definition.
type indexDef struct {
	name   string
	column string
}

// CreateIndex records an index definition. The catalog row is the commit point;
// the table's definitions change only after it is durable. The trees are not
// built here: the next statement that needs them builds them from one scan.
func (t *SqlTable) CreateIndex(name, column string, ifNotExists bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}
	if !slices.ContainsFunc(t.Columns, func(c parser.ColumnDefinition) bool { return c.Name == column }) {
		return fmt.Errorf("unknown column %q", column)
	}
	if slices.ContainsFunc(t.indexDefs, func(d indexDef) bool { return d.name == name }) {
		if ifNotExists {
			return nil
		}
		return fmt.Errorf("index %q already exists on table %q", name, t.Name)
	}
	if err := t.store.addIndex(t.database, t.Name, name, column); err != nil {
		return err
	}
	t.indexDefs = append(slices.Clone(t.indexDefs), indexDef{name: name, column: column})
	t.dropIndexes()
	return nil
}

// DropIndex removes an index definition; the tombstone is the commit point.
func (t *SqlTable) DropIndex(name string, ifExists bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return fmt.Errorf("table %q is closed", t.Name)
	}
	at := slices.IndexFunc(t.indexDefs, func(d indexDef) bool { return d.name == name })
	if at < 0 {
		if ifExists {
			return nil
		}
		return fmt.Errorf("index %q does not exist on table %q", name, t.Name)
	}
	if err := t.store.removeIndex(t.database, t.Name, name); err != nil {
		return err
	}
	t.indexDefs = slices.Delete(slices.Clone(t.indexDefs), at, at+1)
	t.dropIndexes()
	return nil
}
