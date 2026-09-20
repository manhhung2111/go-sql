package parser

import "fmt"

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

	switch firstToken.Type {
	case SELECT:
		return s.parseSelectStatement()
	default:
		return nil, fmt.Errorf("command not implemented, got %s", firstToken.Value)
	}
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
