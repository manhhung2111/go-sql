package parser

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The server shares one lexer and one parser across all requests, so both must
// give every call the answer it would give alone.
func hammer(t *testing.T, call func(g, i int) error) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1500; i++ {
				if err := call(g, i); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

func TestParser_ConcurrentParsesDoNotInterfere(t *testing.T) {
	p := NewParser(NewLexer())

	hammer(t, func(g, i int) error {
		id := g*100000 + i
		switch g % 3 {
		case 0:
			stmt, err := p.Parse(fmt.Sprintf("SELECT id, name FROM users WHERE id = %d", id))
			if err != nil {
				return err
			}
			sel, ok := stmt.(SelectStatement)
			if !ok || sel.Table != "users" || whereString(sel.Where) != fmt.Sprintf("id = %d", id) {
				return fmt.Errorf("got %#v for id %d", stmt, id)
			}
		case 1:
			stmt, err := p.Parse(fmt.Sprintf("INSERT INTO items (id, label) VALUES (%d, 'abc'), (%d, 'defgh')", id, id+1))
			if err != nil {
				return err
			}
			ins, ok := stmt.(InsertIntoStatement)
			if !ok || ins.Table != "items" || len(ins.Values) != 2 {
				return fmt.Errorf("got %#v for id %d", stmt, id)
			}
		default:
			stmt, err := p.Parse(fmt.Sprintf("DELETE FROM logs WHERE n >= %d", id))
			if err != nil {
				return err
			}
			del, ok := stmt.(DeleteStatement)
			if !ok || del.Table != "logs" {
				return fmt.Errorf("got %#v for id %d", stmt, id)
			}
		}
		return nil
	})
}

func TestLexer_ConcurrentCallsDoNotInterfere(t *testing.T) {
	l := NewLexer()

	hammer(t, func(g, i int) error {
		sql := fmt.Sprintf("SELECT %d FROM t%d WHERE x = 'v%d'", i, g, i)
		tokens := l.Lexing(sql)
		want := []string{"SELECT", fmt.Sprint(i), "FROM", fmt.Sprintf("t%d", g), "WHERE", "x", "=", fmt.Sprintf("v%d", i), "EOF"}
		if len(tokens) != len(want) {
			return fmt.Errorf("%q lexed to %d tokens, want %d", sql, len(tokens), len(want))
		}
		for k, tok := range tokens {
			if tok.Value != want[k] {
				return fmt.Errorf("%q token %d is %q, want %q", sql, k, tok.Value, want[k])
			}
		}
		return nil
	})
}

func TestParser_ConcurrentParseOfBadSQLReturnsErrorsNotPanics(t *testing.T) {
	p := NewParser(NewLexer())
	require.NotPanics(t, func() {
		hammer(t, func(g, i int) error {
			_, err := p.Parse(fmt.Sprintf("SELEC %d FROM", i))
			if err == nil {
				return fmt.Errorf("a bad statement parsed")
			}
			return nil
		})
	})
}
