package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"manhhung2111/go-sql/internal/parser"
)

// The schema of a table is stored as JSON in sys_tables. Every variant has a
// stable string tag; the parser's own enum values are never stored, because
// they are iota-numbered and would shift whenever a token is added.

type jsonColumn struct {
	Name        string           `json:"name"`
	Type        jsonDataType     `json:"type"`
	Constraints []jsonConstraint `json:"constraints,omitempty"`
}

type jsonDataType struct {
	Kind string `json:"kind"`
	Size int    `json:"size,omitempty"`
}

type jsonConstraint struct {
	Kind  string `json:"kind"`
	Token string `json:"token,omitempty"`
	Value string `json:"value,omitempty"`
}

// EncodeSchema renders columns as the JSON stored in sys_tables.
func EncodeSchema(columns []parser.ColumnDefinition) (string, error) {
	out := make([]jsonColumn, len(columns))
	for i, column := range columns {
		dt, err := encodeDataType(column.DataType)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", column.Name, err)
		}
		out[i] = jsonColumn{Name: column.Name, Type: dt}
		for _, constraint := range column.Constraints {
			c, err := encodeConstraint(constraint)
			if err != nil {
				return "", fmt.Errorf("column %q: %w", column.Name, err)
			}
			out[i].Constraints = append(out[i].Constraints, c)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding schema: %w", err)
	}
	return string(b), nil
}

// DecodeSchema is the inverse of EncodeSchema. Unknown tags and unknown
// fields are errors: a schema this build cannot fully understand must not be
// half-loaded.
func DecodeSchema(s string) ([]parser.ColumnDefinition, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var in []jsonColumn
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("decoding schema: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("decoding schema: unexpected data after the column list")
	}

	columns := make([]parser.ColumnDefinition, len(in))
	for i, column := range in {
		dt, err := decodeDataType(column.Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", column.Name, err)
		}
		columns[i] = parser.ColumnDefinition{Name: column.Name, DataType: dt}
		for _, c := range column.Constraints {
			constraint, err := decodeConstraint(c)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", column.Name, err)
			}
			columns[i].Constraints = append(columns[i].Constraints, constraint)
		}
	}
	return columns, nil
}

func encodeDataType(dt parser.DataType) (jsonDataType, error) {
	switch d := dt.(type) {
	case parser.CharDataType:
		return jsonDataType{Kind: "char", Size: d.Size}, nil
	case parser.VarCharDataType:
		return jsonDataType{Kind: "varchar", Size: d.Size}, nil
	case parser.TextDataType:
		return jsonDataType{Kind: "text", Size: d.Size}, nil
	case parser.SmallIntDataType:
		return jsonDataType{Kind: "smallint", Size: d.Size}, nil
	case parser.MediumIntDataType:
		return jsonDataType{Kind: "mediumint", Size: d.Size}, nil
	case parser.IntDataType:
		return jsonDataType{Kind: "int", Size: d.Size}, nil
	case parser.BigIntDataType:
		return jsonDataType{Kind: "bigint"}, nil
	case parser.BooleanDataType:
		return jsonDataType{Kind: "boolean"}, nil
	default:
		return jsonDataType{}, fmt.Errorf("unsupported data type %T", dt)
	}
}

func decodeDataType(d jsonDataType) (parser.DataType, error) {
	switch d.Kind {
	case "char":
		return parser.CharDataType{Size: d.Size}, nil
	case "varchar":
		return parser.VarCharDataType{Size: d.Size}, nil
	case "text":
		return parser.TextDataType{Size: d.Size}, nil
	case "smallint":
		return parser.SmallIntDataType{Size: d.Size}, nil
	case "mediumint":
		return parser.MediumIntDataType{Size: d.Size}, nil
	case "int":
		return parser.IntDataType{Size: d.Size}, nil
	case "bigint":
		return parser.BigIntDataType{}, nil
	case "boolean":
		return parser.BooleanDataType{}, nil
	default:
		return nil, fmt.Errorf("unknown data type %q", d.Kind)
	}
}

func encodeConstraint(constraint parser.Constraint) (jsonConstraint, error) {
	switch c := constraint.(type) {
	case parser.NotNullConstraint:
		return jsonConstraint{Kind: "not_null"}, nil
	case parser.UniqueConstraint:
		return jsonConstraint{Kind: "unique"}, nil
	case parser.PrimaryKeyConstraint:
		return jsonConstraint{Kind: "primary_key"}, nil
	case parser.DefaultConstraint:
		switch c.DefaultValue.Type {
		case parser.STRING:
			return jsonConstraint{Kind: "default", Token: "string", Value: c.DefaultValue.Value}, nil
		case parser.NUMBER:
			return jsonConstraint{Kind: "default", Token: "number", Value: c.DefaultValue.Value}, nil
		default:
			return jsonConstraint{}, fmt.Errorf("unsupported default value %q", c.DefaultValue.Value)
		}
	default:
		return jsonConstraint{}, fmt.Errorf("unsupported constraint %T", constraint)
	}
}

func decodeConstraint(c jsonConstraint) (parser.Constraint, error) {
	switch c.Kind {
	case "not_null":
		return parser.NotNullConstraint{}, nil
	case "unique":
		return parser.UniqueConstraint{}, nil
	case "primary_key":
		return parser.PrimaryKeyConstraint{}, nil
	case "default":
		switch c.Token {
		case "string":
			return parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.STRING, Value: c.Value}}, nil
		case "number":
			return parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.NUMBER, Value: c.Value}}, nil
		default:
			return nil, fmt.Errorf("unknown default token %q", c.Token)
		}
	default:
		return nil, fmt.Errorf("unknown constraint %q", c.Kind)
	}
}
