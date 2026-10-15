package engine

// indexDef is one CREATE INDEX definition: an index named name on a column,
// by name so a column rename only has to rewrite the definition.
type indexDef struct {
	name   string
	column string
}
