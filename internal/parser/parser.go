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
//	"SELECT" selectExpression "FROM" table
//	selectExpression := "*" | column ("," column)*
//	table := IDENT
func (s *sqlParser) parseSelectStatement() (SelectStatement, error) {
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

	return SelectStatement{Columns: columns, Table: table.Value}, nil
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
