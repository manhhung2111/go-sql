package engine

import (
	"fmt"
	"manhhung2111/go-sql/internal/parser"
)

type Engine interface {
	Execute(statement parser.SqlStatement, database string) (Response, error)
}

type SqlEngine struct {
	Catalog Catalog
}

func NewEngine(catalog Catalog) Engine {
	return &SqlEngine{Catalog: catalog}
}

// resolveDatabase requires dbName to be non-empty, then looks it up in the
// catalog — every table-level statement needs both checks before it can
// reach its table.
func (e *SqlEngine) resolveDatabase(dbName string) (Database, error) {
	if dbName == "" {
		return nil, fmt.Errorf("database name is required")
	}
	return e.Catalog.GetDatabase(dbName)
}

// resolveTable looks up name in database, turning a missing table into the
// error every table-level statement reports.
func resolveTable(database Database, name string) (Table, error) {
	table, exists := database.GetTable(name)
	if !exists {
		return nil, fmt.Errorf("table %q does not exist", name)
	}
	return table, nil
}

func (e *SqlEngine) Execute(statement parser.SqlStatement, dbName string) (Response, error) {
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

	case parser.CreateTableStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}

		if err := database.CreateTable(stmt.Table, stmt.Columns, stmt.IfNotExists); err != nil {
			return Response{}, err
		}

		return Response{}, nil

	case parser.InsertIntoStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}

		table, err := resolveTable(database, stmt.Table)
		if err != nil {
			return Response{}, err
		}

		if err := table.InsertValues(stmt.Columns, stmt.Values); err != nil {
			return Response{}, err
		}

		return Response{}, nil

	case parser.AlterTableStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}

		if rename, ok := stmt.Action.(parser.RenameTableAction); ok {
			if err := database.RenameTable(stmt.Table, rename.NewName); err != nil {
				return Response{}, err
			}
			return Response{}, nil
		}

		table, err := resolveTable(database, stmt.Table)
		if err != nil {
			return Response{}, err
		}

		if err := table.AlterColumns(stmt.Action); err != nil {
			return Response{}, err
		}

		return Response{}, nil

	case parser.DropTableStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}

		if err := database.DropTable(stmt.Table, stmt.IfExists); err != nil {
			return Response{}, err
		}

		return Response{}, nil

	case parser.SelectStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}

		table, err := resolveTable(database, stmt.Table)
		if err != nil {
			return Response{}, err
		}

		return table.Select(stmt.Columns, stmt.Where)

	case parser.DeleteStatement:
		database, err := e.resolveDatabase(dbName)
		if err != nil {
			return Response{}, err
		}

		table, err := resolveTable(database, stmt.Table)
		if err != nil {
			return Response{}, err
		}

		if err := table.Delete(stmt.Where); err != nil {
			return Response{}, err
		}

		return Response{}, nil

	default:
		return Response{}, fmt.Errorf("statement not supported, got %T", stmt)
	}
}
