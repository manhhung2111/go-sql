package engine

import "manhhung2111/go-sql/internal/parser"

type Table interface {
}

type SqlTable struct {
	Name    string
	Columns []parser.ColumnDefinition
}

func NewTable(name string, columns []parser.ColumnDefinition) *SqlTable {
	return &SqlTable{Name: name, Columns: columns}
}
