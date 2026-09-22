package parser

import "fmt"

type Parser interface {
	Parse() (SqlStatement, error)
}

type SqlStatement interface{}

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
		if s.expect(DATABASE) {
			stmt, err = s.parseDropDatabaseStatement()
		} else if s.expect(TABLE) {
			stmt, err = s.parseDropTableStatement()
		} else {
			return nil, fmt.Errorf("DATABASE or TABLE keyword must be expected after DROP, got %s", s.peek().Value)
		}
	case SHOW:
		if !s.expect(DATABASES) {
			return nil, fmt.Errorf("DATABASES keyword must be expected after SHOW, got %s", s.peek().Value)
		}
		stmt, err = s.parseShowDatabasesStatement()
	case ALTER:
		if !s.expect(TABLE) {
			return nil, fmt.Errorf("TABLE keyword must be expected after ALTER, got %s", s.peek().Value)
		}
		stmt, err = s.parseAlterTableStatement()
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
