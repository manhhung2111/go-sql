package parser

import (
	"strings"
	"unicode"
)

type Lexer interface {
	Lexing() []Token
}

type sqlLexer struct {
	runes []rune
	pos   int
}

func NewLexer(sqlStatement string) Lexer {
	return &sqlLexer{runes: []rune(sqlStatement)}
}

func getTokenType(value string) TokenType {
	if tokenType, exists := tokenTypeMapping[strings.ToUpper(value)]; exists {
		return tokenType
	}
	return IDENT
}

// peek returns the rune at the given lookahead offset from pos without
// consuming it, or 0 if that position is past the end of input.
func (l *sqlLexer) peek(offset int) rune {
	i := l.pos + offset
	if i >= len(l.runes) {
		return 0
	}
	return l.runes[i]
}

func (l *sqlLexer) atEnd() bool {
	return l.pos >= len(l.runes)
}

// advance consumes and returns the current rune.
func (l *sqlLexer) advance() rune {
	r := l.runes[l.pos]
	l.pos++
	return r
}

// skipWhitespace moves pos forward onto the next non-whitespace rune,
// or to end of input.
func (l *sqlLexer) skipWhitespace() {
	for !l.atEnd() && unicode.IsSpace(l.peek(0)) {
		l.pos++
	}
}

func (l *sqlLexer) Lexing() []Token {
	l.pos = 0 // Lexing is re-runnable: reset scan position each call
	tokens := make([]Token, 0)

	for {
		l.skipWhitespace()
		if l.atEnd() {
			break
		}

		start := l.pos
		switch r := l.peek(0); {
		// TODO: handle negative number
		case unicode.IsDigit(r):
			tokens = append(tokens, l.scanNumber(start))
		case r == '\'' || r == '"':
			tokens = append(tokens, l.scanString(start))
		case unicode.IsLetter(r) || r == '_':
			tokens = append(tokens, l.scanIdentifier(start))
		default:
			tokens = append(tokens, l.scanSymbol(start))
		}
	}

	tokens = append(tokens, Token{Type: EOF, Value: "EOF", Position: len(l.runes)})
	return tokens
}

func (l *sqlLexer) scanNumber(start int) Token {
	for !l.atEnd() && (unicode.IsDigit(l.peek(0)) || l.peek(0) == '.') {
		l.pos++
	}
	value := string(l.runes[start:l.pos])
	return Token{Type: NUMBER, Value: value, Position: start}
}

func (l *sqlLexer) scanString(start int) Token {
	quote := l.advance() // consume opening quote
	for !l.atEnd() && l.peek(0) != quote {
		l.pos++
	}

	if l.atEnd() {
		// ran out of input before finding the closing quote
		return Token{Type: ILLEGAL, Value: string(l.runes[start:l.pos]), Position: start}
	}

	value := string(l.runes[start+1 : l.pos])
	l.pos++ // consume closing quote
	return Token{Type: STRING, Value: value, Position: start}
}

func (l *sqlLexer) scanIdentifier(start int) Token {
	for !l.atEnd() && (unicode.IsLetter(l.peek(0)) || unicode.IsDigit(l.peek(0)) || l.peek(0) == '_') {
		l.pos++
	}
	value := string(l.runes[start:l.pos])
	return Token{Type: getTokenType(value), Value: value, Position: start}
}

func (l *sqlLexer) scanSymbol(start int) Token {
	if t, ok := tokenTypeMapping[string([]rune{l.peek(0), l.peek(1)})]; ok { // !=, <>, <=, >=
		l.pos += 2
		return Token{Type: t, Value: string(l.runes[start:l.pos]), Position: start}
	}

	one := string(l.advance())
	if t, ok := tokenTypeMapping[one]; ok {
		return Token{Type: t, Value: one, Position: start}
	}
	return Token{Type: ILLEGAL, Value: one, Position: start}
}
