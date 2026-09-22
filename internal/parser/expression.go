package parser

import "fmt"

type Expression interface{}

type BinaryExpression struct {
	Left     Expression
	Right    Expression
	Operator TokenType // Must either be "AND" or "OR"
}

type ComparisonExpression struct {
	Left     Token
	Right    Token
	Operator TokenType // Must be in ["=", "!=", "<>", ">=", ">", "<=", "<"]
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
