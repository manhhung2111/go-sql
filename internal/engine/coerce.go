package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"manhhung2111/go-sql/internal/parser"
)

// coerceValue converts a raw literal Token into a native Go value
// validated against dt. nil (SQL NULL) passes through unchanged.
func coerceValue(dt parser.DataType, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	tok := value.(parser.Token)

	switch d := dt.(type) {
	case parser.CharDataType:
		return coerceString(tok, d.Size)
	case parser.VarCharDataType:
		return coerceString(tok, d.Size)
	case parser.TextDataType:
		return coerceString(tok, d.Size)
	case parser.BooleanDataType:
		return coerceBoolean(tok)
	case parser.SmallIntDataType:
		return coerceInt(tok, "SMALLINT", -32768, 32767)
	case parser.MediumIntDataType:
		return coerceInt(tok, "MEDIUMINT", -8388608, 8388607)
	case parser.IntDataType:
		return coerceInt(tok, "INT", math.MinInt32, math.MaxInt32)
	case parser.BigIntDataType:
		return coerceInt(tok, "BIGINT", math.MinInt64, math.MaxInt64)
	default:
		return nil, fmt.Errorf("unsupported data type %T", dt)
	}
}

// coerceString validates tok as a string literal, capped at maxLen
// (ignored when 0). CHAR and VARCHAR both just get a max-length check —
// no fixed-width padding, so CHAR isn't given real storage semantics yet.
func coerceString(tok parser.Token, maxLen int) (string, error) {
	if tok.Type != parser.STRING {
		return "", fmt.Errorf("expected a string value, got %s", tok.Value)
	}
	if maxLen > 0 && len(tok.Value) > maxLen {
		return "", fmt.Errorf("value %q too long for max length %d", tok.Value, maxLen)
	}
	return tok.Value, nil
}

// coerceInt validates tok as an integer literal within [min, max] — the
// SQL type's own fixed range, independent of any declared display width.
func coerceInt(tok parser.Token, typeName string, min, max int64) (int64, error) {
	if tok.Type != parser.NUMBER {
		return 0, fmt.Errorf("expected a numeric value, got %s", tok.Value)
	}
	n, err := strconv.ParseInt(tok.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value %q is not a valid integer", tok.Value)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("value %d out of range for %s", n, typeName)
	}
	return n, nil
}

// coerceBoolean accepts a NUMBER 0/1 or a STRING "true"/"false"
// (case-insensitive) — there's no dedicated boolean literal syntax in
// the parser yet.
func coerceBoolean(tok parser.Token) (bool, error) {
	switch tok.Type {
	case parser.NUMBER:
		switch tok.Value {
		case "0":
			return false, nil
		case "1":
			return true, nil
		}
	case parser.STRING:
		switch strings.ToLower(tok.Value) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("expected a boolean value, got %s", tok.Value)
}
