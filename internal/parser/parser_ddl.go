package parser

import (
	"fmt"
	"strconv"
)

// "CREATE" "DATABASE" database_name;
func (s *sqlParser) parseCreateDatabaseStatement() (CreateDatabaseStatement, error) {
	database := s.advance()
	if database.Type != IDENT {
		return CreateDatabaseStatement{}, fmt.Errorf("expected database name, got %s", database.Value)
	}

	return CreateDatabaseStatement{Database: database.Value}, nil
}

// "DROP" "DATABASE" database_name;
func (s *sqlParser) parseDropDatabaseStatement() (DropDatabaseStatement, error) {
	database := s.advance()
	if database.Type != IDENT {
		return DropDatabaseStatement{}, fmt.Errorf("expected database name, got %s", database.Value)
	}

	return DropDatabaseStatement{Database: database.Value}, nil
}

// "DROP" "TABLE" ("IF" "EXISTS")? table_name;
func (s *sqlParser) parseDropTableStatement() (DropTableStatement, error) {
	dropTableStatement := DropTableStatement{}

	if s.expect(IF) {
		if !s.expect(EXISTS) {
			return DropTableStatement{}, fmt.Errorf("expected EXISTS after IF, got %s", s.peek().Value)
		}
		dropTableStatement.IfExists = true
	}

	table := s.advance()
	if table.Type != IDENT {
		return DropTableStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}
	dropTableStatement.Table = table.Value

	return dropTableStatement, nil
}

// "SHOW" "DATABASES"
func (s *sqlParser) parseShowDatabasesStatement() (ShowDatabasesStatement, error) {
	return ShowDatabasesStatement{}, nil
}

// "CREATE" "TABLE" table_name (
//
//	column1 datatype constraint,
//	column2 datatype constraint,
//	column3 datatype constraint,
//	....
//
// );
func (s *sqlParser) parseCreateTableStatement() (CreateTableStatement, error) {
	createTableStatement := CreateTableStatement{}

	if s.expect(IF) {
		if !s.expect(NOT) {
			return CreateTableStatement{}, fmt.Errorf("expected NOT after IF, got %s", s.peek().Value)
		}

		if !s.expect(EXISTS) {
			return CreateTableStatement{}, fmt.Errorf("expected EXISTS after IF NOT, got %s", s.peek().Value)
		}
		createTableStatement.IfNotExists = true
	}

	table := s.advance()
	if table.Type != IDENT {
		return CreateTableStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}
	createTableStatement.Table = table.Value

	if !s.expect(LPAREN) {
		return CreateTableStatement{}, fmt.Errorf("expected '(' before column list, got %s", s.peek().Value)
	}

	columns := make([]ColumnDefinition, 0)
	for {
		columnDefinition, err := s.parseColumnDefinition()
		if err != nil {
			return CreateTableStatement{}, err
		}

		columns = append(columns, columnDefinition)
		if !s.expect(COMMA) {
			break
		}
	}

	if !s.expect(RPAREN) {
		return CreateTableStatement{}, fmt.Errorf("expected ')' after column list, got %s", s.peek().Value)
	}

	createTableStatement.Columns = columns
	return createTableStatement, nil
}

// parseColumnDefinition parses a single "name datatype ('(' size ')')?
// constraint*" column definition, shared by CREATE TABLE's column list and
// ALTER TABLE's ADD COLUMN action.
func (s *sqlParser) parseColumnDefinition() (ColumnDefinition, error) {
	columnDefinition := ColumnDefinition{}

	column := s.peek()
	if column.Type != IDENT && column.Type != STRING {
		return ColumnDefinition{}, fmt.Errorf("expected column name, got %s", column.Value)
	}
	columnDefinition.Name = column.Value
	s.advance()

	dataType := s.peek()
	if !isDataType(dataType.Type) {
		return ColumnDefinition{}, fmt.Errorf("expected data type, got %s", dataType.Value)
	}
	columnDataType, err := getDataType(dataType.Type)
	if err != nil {
		return ColumnDefinition{}, err
	}

	columnDefinition.DataType = columnDataType
	s.advance()

	if s.expect(LPAREN) {
		sizeToken := s.peek()
		if sizeToken.Type != NUMBER {
			return ColumnDefinition{}, fmt.Errorf("expected size in data type, got %s", sizeToken.Value)
		}
		s.advance()

		if !s.expect(RPAREN) {
			return ColumnDefinition{}, fmt.Errorf("expected ')' after size in data type, got %s", s.peek().Value)
		}

		size, err := strconv.Atoi(sizeToken.Value)
		if err != nil {
			return ColumnDefinition{}, fmt.Errorf("invalid size %s: %v", sizeToken.Value, err)
		}

		columnDefinition.DataType, err = applyDataTypeSize(columnDefinition.DataType, size)
		if err != nil {
			return ColumnDefinition{}, err
		}
	}

	constraints, err := s.parseColumnConstraints()
	if err != nil {
		return ColumnDefinition{}, err
	}
	columnDefinition.Constraints = constraints

	return columnDefinition, nil
}

// parseColumnConstraints parses zero or more constraint keywords following a
// column's data type (e.g. "NOT NULL", "PRIMARY KEY", "DEFAULT 0", "UNIQUE"),
// stopping at the first token that isn't a constraint keyword without
// consuming it.
func (s *sqlParser) parseColumnConstraints() ([]Constraint, error) {
	constraints := make([]Constraint, 0)

	for {
		switch s.peek().Type {
		case NOT:
			s.advance()
			if !s.expect(NULL) {
				return nil, fmt.Errorf("expected NULL after NOT constraint, got %s", s.peek().Value)
			}
			constraints = append(constraints, NotNullConstraint{})
		case UNIQUE:
			s.advance()
			constraints = append(constraints, UniqueConstraint{})
		case PRIMARY:
			s.advance()
			if !s.expect(KEY) {
				return nil, fmt.Errorf("expected KEY after PRIMARY constraint, got %s", s.peek().Value)
			}
			constraints = append(constraints, PrimaryKeyConstraint{})
		case DEFAULT:
			s.advance()
			defaultValue := s.peek()
			if defaultValue.Type != STRING && defaultValue.Type != NUMBER {
				return nil, fmt.Errorf("expected String or Number default value for DEFAULT constraint, got %s", defaultValue.Value)
			}
			s.advance()
			constraints = append(constraints, DefaultConstraint{DefaultValue: defaultValue})
		default:
			return constraints, nil
		}
	}
}

// "ALTER" "TABLE" table_name alterAction
//
//	alterAction :=
//	    "ADD" "COLUMN"? column_definition
//	  | "DROP" "COLUMN"? column_name
//	  | "RENAME" "COLUMN" old_name "TO" new_name
//	  | "RENAME" "TO" new_table_name
func (s *sqlParser) parseAlterTableStatement() (AlterTableStatement, error) {
	table := s.advance()
	if table.Type != IDENT {
		return AlterTableStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}

	action, err := s.parseAlterAction()
	if err != nil {
		return AlterTableStatement{}, err
	}

	return AlterTableStatement{Table: table.Value, Action: action}, nil
}

func (s *sqlParser) parseAlterAction() (AlterAction, error) {
	switch {
	case s.expect(ADD):
		s.expect(COLUMN)

		column, err := s.parseColumnDefinition()
		if err != nil {
			return nil, err
		}
		return AddColumnAction{Column: column}, nil
	case s.expect(DROP):
		s.expect(COLUMN)

		column := s.advance()
		if column.Type != IDENT {
			return nil, fmt.Errorf("expected column name, got %s", column.Value)
		}
		return DropColumnAction{Column: column.Value}, nil
	case s.expect(RENAME):
		if s.expect(COLUMN) {
			oldName := s.advance()
			if oldName.Type != IDENT {
				return nil, fmt.Errorf("expected column name, got %s", oldName.Value)
			}

			if !s.expect(TO) {
				return nil, fmt.Errorf("expected TO after RENAME COLUMN %s, got %s", oldName.Value, s.peek().Value)
			}

			newName := s.advance()
			if newName.Type != IDENT {
				return nil, fmt.Errorf("expected column name, got %s", newName.Value)
			}

			return RenameColumnAction{OldName: oldName.Value, NewName: newName.Value}, nil
		}

		if !s.expect(TO) {
			return nil, fmt.Errorf("expected COLUMN or TO after RENAME, got %s", s.peek().Value)
		}

		newName := s.advance()
		if newName.Type != IDENT {
			return nil, fmt.Errorf("expected table name, got %s", newName.Value)
		}

		return RenameTableAction{NewName: newName.Value}, nil
	default:
		return nil, fmt.Errorf("expected ADD, DROP or RENAME after table name, got %s", s.peek().Value)
	}
}

func isDataType(t TokenType) bool {
	switch t {
	case CHAR, VARCHAR, TEXT, BOOLEAN, SMALLINT, MEDIUMINT, INT, BIGINT:
		return true
	default:
		return false
	}
}

func getDataType(t TokenType) (DataType, error) {
	switch t {
	case CHAR:
		return CharDataType{}, nil
	case VARCHAR:
		return VarCharDataType{}, nil
	case TEXT:
		return TextDataType{}, nil
	case BOOLEAN:
		return BooleanDataType{}, nil
	case SMALLINT:
		return SmallIntDataType{}, nil
	case MEDIUMINT:
		return MediumIntDataType{}, nil
	case INT:
		return IntDataType{}, nil
	case BIGINT:
		return BigIntDataType{}, nil
	default:
		return nil, fmt.Errorf("unknown data type %d", t)
	}
}

// applyDataTypeSize sets the "(<size>)" argument on data types that accept
// one, and rejects it on types that don't (e.g. "BOOLEAN(5)").
func applyDataTypeSize(dataType DataType, size int) (DataType, error) {
	switch dt := dataType.(type) {
	case CharDataType:
		dt.Size = size
		return dt, nil
	case VarCharDataType:
		dt.Size = size
		return dt, nil
	case TextDataType:
		dt.Size = size
		return dt, nil
	case SmallIntDataType:
		dt.Size = size
		return dt, nil
	case MediumIntDataType:
		dt.Size = size
		return dt, nil
	case IntDataType:
		dt.Size = size
		return dt, nil
	default:
		return nil, fmt.Errorf("%T does not accept a size argument", dataType)
	}
}
