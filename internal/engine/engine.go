package engine

import (
	"fmt"
	"manhhung2111/go-sql/internal/parser"
)

type Engine interface {
	Execute(statement parser.SqlStatement) (Response, error)
}

type SqlEngine struct {
	Catalog Catalog
}

func NewEngine(catalog Catalog) Engine {
	return &SqlEngine{Catalog: catalog}
}

func (e *SqlEngine) Execute(statement parser.SqlStatement) (Response, error) {
	switch stmt := statement.(type) {
	case parser.CreateDatabaseStatement:
		if err := e.Catalog.CreateDatabase(stmt.Database); err != nil {
			return Response{}, err
		}

		return Response{}, nil
	case parser.DropDatabaseStatement:
		if err := e.Catalog.DropDatabase(stmt.Database); err != nil {
			return Response{}, err
		}

		return Response{}, nil
	case parser.ShowDatabasesStatement:
		databases := e.Catalog.ListDatabases()

		rows := make([][]string, len(databases))
		for i, database := range databases {
			rows[i] = []string{database}
		}

		return Response{Columns: []string{"Database"}, Rows: rows}, nil
	default:
		return Response{}, fmt.Errorf("statement not supported, got %T", stmt)
	}
}
