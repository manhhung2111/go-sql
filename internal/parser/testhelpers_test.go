package parser

import (
	"fmt"
	"strings"
	"testing"
)

func parse(t *testing.T, sql string) (SqlStatement, error) {
	t.Helper()
	return NewParser(NewLexer()).Parse(sql)
}

// valueStrings flattens [][]Token down to [][]string so tests can assert on
// value content without hard-coding token Position values.
func valueStrings(rows [][]Token) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		values := make([]string, len(row))
		for j, tok := range row {
			values[j] = tok.Value
		}
		out[i] = values
	}
	return out
}

// setStrings renders []Assignment as "column=value" pairs so tests can
// assert on content without hard-coding token Position values.
func setStrings(set []Assignment) []string {
	out := make([]string, len(set))
	for i, a := range set {
		out[i] = fmt.Sprintf("%s=%s", a.Column, a.Value.Value)
	}
	return out
}

// columnDefinitionStrings renders []ColumnDefinition as canonical
// "name TYPE(size) CONSTRAINT..." strings so tests can assert on column
// shape without hard-coding the token Position values inside DEFAULT
// constraints.
func columnDefinitionStrings(columns []ColumnDefinition) []string {
	out := make([]string, len(columns))
	for i, c := range columns {
		def := fmt.Sprintf("%s %s", c.Name, dataTypeString(c.DataType))
		if len(c.Constraints) > 0 {
			def += " " + constraintStrings(c.Constraints)
		}
		out[i] = def
	}
	return out
}

func dataTypeString(dt DataType) string {
	switch d := dt.(type) {
	case CharDataType:
		return fmt.Sprintf("CHAR(%d)", d.Size)
	case VarCharDataType:
		return fmt.Sprintf("VARCHAR(%d)", d.Size)
	case TextDataType:
		return fmt.Sprintf("TEXT(%d)", d.Size)
	case BooleanDataType:
		return "BOOLEAN"
	case SmallIntDataType:
		return fmt.Sprintf("SMALLINT(%d)", d.Size)
	case MediumIntDataType:
		return fmt.Sprintf("MEDIUMINT(%d)", d.Size)
	case IntDataType:
		return fmt.Sprintf("INT(%d)", d.Size)
	case BigIntDataType:
		return "BIGINT"
	default:
		return fmt.Sprintf("unexpected data type %T", dt)
	}
}

func constraintStrings(constraints []Constraint) string {
	parts := make([]string, len(constraints))
	for i, c := range constraints {
		switch v := c.(type) {
		case NotNullConstraint:
			parts[i] = "NOT NULL"
		case UniqueConstraint:
			parts[i] = "UNIQUE"
		case PrimaryKeyConstraint:
			parts[i] = "PRIMARY KEY"
		case DefaultConstraint:
			parts[i] = fmt.Sprintf("DEFAULT %s", v.DefaultValue.Value)
		default:
			parts[i] = fmt.Sprintf("unexpected constraint %T", c)
		}
	}
	return strings.Join(parts, " ")
}

var operatorSymbols = map[TokenType]string{
	EQ: "=", NEQ: "!=", LT: "<", LTE: "<=", GT: ">", GTE: ">=", AND: "AND", OR: "OR",
}

// whereString renders a WHERE expression tree as a canonical string so tests
// can assert on its shape without hard-coding token Position values.
func whereString(e Expression) string {
	switch v := e.(type) {
	case nil:
		return "<nil>"
	case *ComparisonExpression:
		return fmt.Sprintf("%s %s %s", v.Left.Value, operatorSymbols[v.Operator], v.Right.Value)
	case *BinaryExpression:
		return fmt.Sprintf("(%s %s %s)", whereString(v.Left), operatorSymbols[v.Operator], whereString(v.Right))
	default:
		return fmt.Sprintf("unexpected expression type %T", e)
	}
}
