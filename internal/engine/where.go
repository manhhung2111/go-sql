package engine

import (
	"fmt"
	"strconv"

	"manhhung2111/go-sql/internal/parser"
)

// evalWhere reports whether row satisfies where. A nil where (no WHERE
// clause) matches every row. columnIndex/columns describe the schema row
// was drawn from, needed to resolve column references inside where.
func evalWhere(where parser.Expression, row []any, columnIndex map[string]int, columns []parser.ColumnDefinition) (bool, error) {
	if where == nil {
		return true, nil
	}
	return evalExpr(where, row, columnIndex, columns)
}

func evalExpr(expr parser.Expression, row []any, columnIndex map[string]int, columns []parser.ColumnDefinition) (bool, error) {
	switch e := expr.(type) {
	case *parser.BinaryExpression:
		left, err := evalExpr(e.Left, row, columnIndex, columns)
		if err != nil {
			return false, err
		}
		right, err := evalExpr(e.Right, row, columnIndex, columns)
		if err != nil {
			return false, err
		}
		switch e.Operator {
		case parser.AND:
			return left && right, nil
		case parser.OR:
			return left || right, nil
		default:
			return false, fmt.Errorf("unsupported logical operator")
		}
	case *parser.ComparisonExpression:
		return evalComparison(e, row, columnIndex, columns)
	default:
		return false, fmt.Errorf("unsupported where expression, got %T", expr)
	}
}

// evalComparison resolves both sides of cmp and applies its operator.
// Whichever side is a column reference tells us the DataType to coerce
// the other side's literal against; when both sides are columns, the
// already-coerced row values are compared directly.
func evalComparison(cmp *parser.ComparisonExpression, row []any, columnIndex map[string]int, columns []parser.ColumnDefinition) (bool, error) {
	leftVal, leftType, leftIsColumn, err := resolveOperand(cmp.Left, row, columnIndex, columns)
	if err != nil {
		return false, err
	}
	rightVal, rightType, rightIsColumn, err := resolveOperand(cmp.Right, row, columnIndex, columns)
	if err != nil {
		return false, err
	}

	switch {
	case leftIsColumn && !rightIsColumn:
		rightVal, err = coerceValue(leftType, rightVal)
	case rightIsColumn && !leftIsColumn:
		leftVal, err = coerceValue(rightType, leftVal)
	case !leftIsColumn && !rightIsColumn:
		if leftVal, err = coerceLiteral(leftVal.(parser.Token)); err == nil {
			rightVal, err = coerceLiteral(rightVal.(parser.Token))
		}
	}
	if err != nil {
		return false, err
	}

	return compareValues(leftVal, rightVal, cmp.Operator)
}

// resolveOperand resolves one side of a comparison. An IDENT names a
// column, resolved to its already-coerced row value and DataType; anything
// else is a literal Token whose coercion is deferred until the other side's
// type (if any) is known.
func resolveOperand(tok parser.Token, row []any, columnIndex map[string]int, columns []parser.ColumnDefinition) (value any, dataType parser.DataType, isColumn bool, err error) {
	if tok.Type != parser.IDENT {
		return tok, nil, false, nil
	}
	idx, ok := columnIndex[tok.Value]
	if !ok {
		return nil, nil, false, fmt.Errorf("unknown column %q", tok.Value)
	}
	return row[idx], columns[idx].DataType, true, nil
}

// coerceLiteral coerces a literal compared against another literal — there
// is no column DataType to validate against, so a NUMBER becomes a plain
// int64 and a STRING keeps its raw value.
func coerceLiteral(tok parser.Token) (any, error) {
	switch tok.Type {
	case parser.NUMBER:
		n, err := strconv.ParseInt(tok.Value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("value %q is not a valid integer", tok.Value)
		}
		return n, nil
	case parser.STRING:
		return tok.Value, nil
	default:
		return nil, fmt.Errorf("expected a number or string literal, got %s", tok.Value)
	}
}

// compareValues applies op to two already-coerced native values. A NULL on
// either side never matches, regardless of op.
func compareValues(left, right any, op parser.TokenType) (bool, error) {
	if left == nil || right == nil {
		return false, nil
	}

	switch l := left.(type) {
	case int64:
		r, ok := right.(int64)
		if !ok {
			return false, fmt.Errorf("cannot compare %T and %T", left, right)
		}
		return compareOrdered(l, r, op)
	case string:
		r, ok := right.(string)
		if !ok {
			return false, fmt.Errorf("cannot compare %T and %T", left, right)
		}
		return compareOrdered(l, r, op)
	case bool:
		r, ok := right.(bool)
		if !ok {
			return false, fmt.Errorf("cannot compare %T and %T", left, right)
		}
		switch op {
		case parser.EQ:
			return l == r, nil
		case parser.NEQ:
			return l != r, nil
		default:
			return false, fmt.Errorf("only = and != are supported for boolean comparisons")
		}
	default:
		return false, fmt.Errorf("unsupported comparison operand type %T", left)
	}
}

// compareOrdered applies op to two values of an orderable native type
// (int64 or string).
func compareOrdered[T int64 | string](l, r T, op parser.TokenType) (bool, error) {
	switch op {
	case parser.EQ:
		return l == r, nil
	case parser.NEQ:
		return l != r, nil
	case parser.LT:
		return l < r, nil
	case parser.LTE:
		return l <= r, nil
	case parser.GT:
		return l > r, nil
	case parser.GTE:
		return l >= r, nil
	default:
		return false, fmt.Errorf("unsupported comparison operator")
	}
}
