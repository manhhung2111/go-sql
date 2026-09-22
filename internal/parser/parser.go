package parser

import (
	"fmt"
	"strconv"
)

type Parser interface {
	Parse() (SqlStatement, error)
}

type sqlParser struct {
	tokens []Token
	pos    int
}

func NewParser(tokens []Token) Parser {
	return &sqlParser{tokens: tokens, pos: 0}
}

func (s *sqlParser) Parse() (SqlStatement, error) {
	firstToken := s.advance()

	var (
		stmt SqlStatement
		err  error
	)

	switch firstToken.Type {
	case SELECT:
		stmt, err = s.parseSelectStatement()
	case INSERT:
		if !s.expect(INTO) {
			return nil, fmt.Errorf("INTO keyword must be expected after INSERT, got %s", s.peek().Value)
		}
		stmt, err = s.parseInsertIntoStatement()
	case UPDATE:
		stmt, err = s.parseUpdateStatement()
	case DELETE:
		if !s.expect(FROM) {
			return nil, fmt.Errorf("FROM keyword must be expected after DELETE, got %s", s.peek().Value)
		}
		stmt, err = s.parseDeleteStatement()
	case CREATE:
		if s.expect(DATABASE) {
			stmt, err = s.parseCreateDatabaseStatement()
		} else if s.expect(TABLE) {
			stmt, err = s.parseCreateTableStatement()
		} else {
			return nil, fmt.Errorf("DATABASE keyword must be expected after CREATE, got %s", s.peek().Value)
		}
	case DROP:
		if !s.expect(DATABASE) {
			return nil, fmt.Errorf("DATABASE keyword must be expected after DROP, got %s", s.peek().Value)
		}

		stmt, err = s.parseDropDatabaseStatement()
	case SHOW:
		if !s.expect(DATABASES) {
			return nil, fmt.Errorf("DATABASES keyword must be expected after SHOW, got %s", s.peek().Value)
		}
		stmt, err = s.parseShowDatabasesStatement()
	default:
		return nil, fmt.Errorf("command not implemented, got %s", firstToken.Value)
	}

	if err != nil {
		return nil, err
	}

	if err := s.expectEnd(); err != nil {
		return nil, err
	}

	return stmt, nil
}

// expectEnd consumes an optional trailing SEMICOLON and rejects any tokens
// left after it, catching trailing garbage after an otherwise valid
// statement (e.g. "SELECT * FROM users EXTRA").
func (s *sqlParser) expectEnd() error {
	s.expect(SEMICOLON)

	if token := s.peek(); token.Type != EOF {
		return fmt.Errorf("unexpected token after statement, got %s", token.Value)
	}

	return nil
}

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
		columnDefinition := ColumnDefinition{}

		column := s.peek()
		if column.Type != IDENT && column.Type != STRING {
			return CreateTableStatement{}, fmt.Errorf("expected column name, got %s", column.Value)
		}
		columnDefinition.Name = column.Value
		s.advance()

		dataType := s.peek()
		if !isDataType(dataType.Type) {
			return CreateTableStatement{}, fmt.Errorf("expected data type, got %s", dataType.Value)
		}
		columnDataType, err := getDataType(dataType.Type)
		if err != nil {
			return CreateTableStatement{}, err
		}

		columnDefinition.DataType = columnDataType
		s.advance()

		if s.expect(LPAREN) {
			sizeToken := s.peek()
			if sizeToken.Type != NUMBER {
				return CreateTableStatement{}, fmt.Errorf("expected size in data type, got %s", sizeToken.Value)
			}
			s.advance()

			if !s.expect(RPAREN) {
				return CreateTableStatement{}, fmt.Errorf("expected ')' after size in data type, got %s", s.peek().Value)
			}

			size, err := strconv.Atoi(sizeToken.Value)
			if err != nil {
				return CreateTableStatement{}, fmt.Errorf("invalid size %s: %v", sizeToken.Value, err)
			}

			columnDefinition.DataType, err = applyDataTypeSize(columnDefinition.DataType, size)
			if err != nil {
				return CreateTableStatement{}, err
			}
		}

		constraints, err := s.parseColumnConstraints()
		if err != nil {
			return CreateTableStatement{}, err
		}
		columnDefinition.Constraints = constraints

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

func (s *sqlParser) parseOrExpression() (Expression, error) {
	left, err := s.parseAndExpression()
	if err != nil {
		return nil, err
	}

	if !s.expect(OR) {
		return left, nil
	}

	right, err := s.parseOrExpression()
	if err != nil {
		return nil, err
	}

	return &BinaryExpression{Left: left, Right: right, Operator: OR}, nil
}

func (s *sqlParser) parseAndExpression() (Expression, error) {
	left, err := s.parseComparison()
	if err != nil {
		return nil, err
	}

	if !s.expect(AND) {
		return left, nil
	}

	right, err := s.parseAndExpression()
	if err != nil {
		return nil, err
	}

	return &BinaryExpression{Left: left, Right: right, Operator: AND}, nil
}

func (s *sqlParser) parseComparison() (*ComparisonExpression, error) {
	left, err := s.parsePrimaryExpr()
	if err != nil {
		return nil, err
	}

	operator := s.peek()
	if !isComparisonOperator(operator.Type) {
		return nil, fmt.Errorf("expected comparison operator, got %s", operator.Value)
	}
	s.advance()

	right, err := s.parsePrimaryExpr()
	if err != nil {
		return nil, err
	}

	return &ComparisonExpression{Left: left, Operator: operator.Type, Right: right}, nil
}

func (s *sqlParser) parsePrimaryExpr() (Token, error) {
	token := s.peek()
	if token.Type != IDENT && token.Type != NUMBER && token.Type != STRING {
		return Token{}, fmt.Errorf("expected identifier, number or string, got %s", token.Value)
	}
	s.advance()
	return token, nil
}

func isComparisonOperator(t TokenType) bool {
	switch t {
	case EQ, NEQ, LT, LTE, GT, GTE:
		return true
	default:
		return false
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

func (s *sqlParser) peek() Token {
	return s.tokens[s.pos]
}

func (s *sqlParser) advance() Token {
	token := s.tokens[s.pos]
	s.pos++
	return token
}

func (s *sqlParser) expect(tokenType TokenType) bool {
	if s.peek().Type == tokenType {
		s.advance()
		return true
	}
	return false
}
