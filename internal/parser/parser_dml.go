package parser

import "fmt"

// Select statement grammar:
//
//	"SELECT" selectExpression "FROM" table ("WHERE" whereExpression)?
//	selectExpression := "*" | column ("," column)*
//	table := IDENT
//	whereExpression := orExpression
//		orExpression := andExpression ("OR" andExpression)*
//		andExpression := comparison ("AND" comparison)*
//		comparison       := primaryExpr comparisonOp primaryExpr
//		comparisonOp     := "=" | "!=" | "<" | "<=" | ">" | ">="
//		primaryExpr      := IDENT | NUMBER | STRING
func (s *sqlParser) parseSelectStatement() (SelectStatement, error) {
	selectStatement := SelectStatement{}
	columns, err := s.parseSelectExpression()
	if err != nil {
		return SelectStatement{}, err
	}

	if !s.expect(FROM) {
		return SelectStatement{}, fmt.Errorf("expected FROM, got %s", s.peek().Value)
	}

	table := s.advance()
	if table.Type != IDENT {
		return SelectStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}

	if s.expect(WHERE) {
		selectStatement.Where, err = s.parseOrExpression()
		if err != nil {
			return SelectStatement{}, err
		}
	}

	selectStatement.Columns = columns
	selectStatement.Table = table.Value

	return selectStatement, nil
}

func (s *sqlParser) parseSelectExpression() ([]string, error) {
	if s.expect(ASTERISK) {
		return []string{"*"}, nil
	}

	columns := make([]string, 0)
	for {
		column := s.advance()
		if column.Type != IDENT {
			return nil, fmt.Errorf("expected column name, got %s", column.Value)
		}
		columns = append(columns, column.Value)

		if !s.expect(COMMA) {
			break
		}
	}

	return columns, nil
}

// "Insert into" statement grammar:
//
//	"INSERT INTO" tableName ("(" column ("," column)* ")")? "VALUES" valueRow ("," valueRow)*
//	valueRow := "(" value ("," value)* ")"
//	value    := NUMBER | STRING
func (s *sqlParser) parseInsertIntoStatement() (InsertIntoStatement, error) {
	table := s.advance()
	if table.Type != IDENT {
		return InsertIntoStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}

	columns, err := s.parseInsertColumns()
	if err != nil {
		return InsertIntoStatement{}, err
	}

	if !s.expect(VALUES) {
		return InsertIntoStatement{}, fmt.Errorf("expected VALUES, got %s", s.peek().Value)
	}

	values, err := s.parseInsertValues()
	if err != nil {
		return InsertIntoStatement{}, err
	}

	return InsertIntoStatement{Table: table.Value, Columns: columns, Values: values}, nil
}

// parseInsertColumns parses an optional "(" column ("," column)* ")" list,
// returning an empty slice when no column list is given.
func (s *sqlParser) parseInsertColumns() ([]string, error) {
	if !s.expect(LPAREN) {
		return []string{}, nil
	}

	columns := make([]string, 0)
	for {
		column := s.advance()
		if column.Type != IDENT {
			return nil, fmt.Errorf("expected column name, got %s", column.Value)
		}
		columns = append(columns, column.Value)

		if !s.expect(COMMA) {
			break
		}
	}

	if !s.expect(RPAREN) {
		return nil, fmt.Errorf("expected ')' after column list, got %s", s.peek().Value)
	}

	return columns, nil
}

// parseInsertValues parses one or more comma-separated value rows.
func (s *sqlParser) parseInsertValues() ([][]Token, error) {
	values := make([][]Token, 0)
	for {
		valueRow, err := s.parseValueRow()
		if err != nil {
			return nil, err
		}
		values = append(values, valueRow)

		if !s.expect(COMMA) {
			break
		}
	}

	return values, nil
}

// parseValueRow parses a single "(" value ("," value)* ")" row.
func (s *sqlParser) parseValueRow() ([]Token, error) {
	if !s.expect(LPAREN) {
		return nil, fmt.Errorf("expected '(' before value list, got %s", s.peek().Value)
	}

	values := make([]Token, 0)
	for {
		value := s.peek()
		if value.Type != STRING && value.Type != NUMBER {
			return nil, fmt.Errorf("expected value, got %s", value.Value)
		}
		values = append(values, value)
		s.advance()

		if !s.expect(COMMA) {
			break
		}
	}

	if !s.expect(RPAREN) {
		return nil, fmt.Errorf("expected ')' after value list, got %s", s.peek().Value)
	}

	return values, nil
}

// "Update" statement grammar:
//
//	"UPDATE" table "SET" assignment ("," assignment)* ("WHERE" whereExpression)?
//	assignment := column "=" value
//	value      := NUMBER | STRING
func (s *sqlParser) parseUpdateStatement() (UpdateStatement, error) {
	table := s.advance()
	if table.Type != IDENT {
		return UpdateStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}

	if !s.expect(SET) {
		return UpdateStatement{}, fmt.Errorf("expected SET, got %s", s.peek().Value)
	}

	assignments, err := s.parseSetClause()
	if err != nil {
		return UpdateStatement{}, err
	}

	updateStatement := UpdateStatement{Table: table.Value, Set: assignments}

	if s.expect(WHERE) {
		where, err := s.parseOrExpression()
		if err != nil {
			return UpdateStatement{}, err
		}
		updateStatement.Where = where
	}

	return updateStatement, nil
}

// parseSetClause parses one or more comma-separated "column = value"
// assignments, rejecting a column assigned more than once.
func (s *sqlParser) parseSetClause() ([]Assignment, error) {
	assignments := make([]Assignment, 0)
	seen := make(map[string]bool)

	for {
		column := s.peek()
		if column.Type != IDENT {
			return nil, fmt.Errorf("expected column name, got %s", column.Value)
		}
		if seen[column.Value] {
			return nil, fmt.Errorf("duplicate assignment for column %s", column.Value)
		}
		seen[column.Value] = true
		s.advance()

		if !s.expect(EQ) {
			return nil, fmt.Errorf("expected '=', got %s", s.peek().Value)
		}

		value := s.peek()
		if value.Type != STRING && value.Type != NUMBER {
			return nil, fmt.Errorf("expected value, got %s", value.Value)
		}
		s.advance()

		assignments = append(assignments, Assignment{Column: column.Value, Value: value})

		if !s.expect(COMMA) {
			break
		}
	}

	return assignments, nil
}

// "Delete" statement grammar:
//
//	"DELETE" "FROM" table ("WHERE" whereExpression)?
func (s *sqlParser) parseDeleteStatement() (DeleteStatement, error) {
	deleteStatement := DeleteStatement{}

	table := s.advance()
	if table.Type != IDENT {
		return DeleteStatement{}, fmt.Errorf("expected table name, got %s", table.Value)
	}

	deleteStatement.Table = table.Value
	if s.expect(WHERE) {
		where, err := s.parseOrExpression()
		if err != nil {
			return DeleteStatement{}, err
		}
		deleteStatement.Where = where
	}

	return deleteStatement, nil
}
