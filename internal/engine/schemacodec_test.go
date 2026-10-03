package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

var allTypesSchema = []parser.ColumnDefinition{
	{Name: "a", DataType: parser.CharDataType{Size: 3}},
	{Name: "b", DataType: parser.VarCharDataType{Size: 50}},
	{Name: "c", DataType: parser.TextDataType{Size: 9}},
	{Name: "d", DataType: parser.SmallIntDataType{Size: 5}},
	{Name: "e", DataType: parser.MediumIntDataType{Size: 7}},
	{Name: "f", DataType: parser.IntDataType{Size: 11}},
	{Name: "g", DataType: parser.BigIntDataType{}},
	{Name: "h", DataType: parser.BooleanDataType{}},
	{Name: "i", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{
		parser.PrimaryKeyConstraint{}, parser.NotNullConstraint{}, parser.UniqueConstraint{},
		parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.NUMBER, Value: "5"}},
	}},
	{Name: "j", DataType: parser.VarCharDataType{Size: 5}, Constraints: []parser.Constraint{
		parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.STRING, Value: ""}},
	}},
}

func TestSchemaCodec_RoundTripsEveryTypeAndConstraint(t *testing.T) {
	encoded, err := EncodeSchema(allTypesSchema)
	require.NoError(t, err)

	decoded, err := DecodeSchema(encoded)

	require.NoError(t, err)
	assert.Equal(t, allTypesSchema, decoded)
}

func TestSchemaCodec_DropsTokenPosition(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "a", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{
		parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.NUMBER, Value: "5", Position: 42}},
	}}}
	encoded, err := EncodeSchema(columns)
	require.NoError(t, err)

	decoded, err := DecodeSchema(encoded)

	require.NoError(t, err)
	assert.Equal(t, parser.Token{Type: parser.NUMBER, Value: "5"}, decoded[0].Constraints[0].(parser.DefaultConstraint).DefaultValue)
}

// The JSON is the on-disk format: changing it silently would orphan every
// existing catalog.
func TestSchemaCodec_FormatIsStable(t *testing.T) {
	columns := []parser.ColumnDefinition{
		{Name: "id", DataType: parser.IntDataType{Size: 11}, Constraints: []parser.Constraint{parser.PrimaryKeyConstraint{}}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}, Constraints: []parser.Constraint{
			parser.NotNullConstraint{},
			parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.STRING, Value: "anon"}},
		}},
		{Name: "ok", DataType: parser.BooleanDataType{}},
	}

	encoded, err := EncodeSchema(columns)

	require.NoError(t, err)
	assert.Equal(t, `[{"name":"id","type":{"kind":"int","size":11},"constraints":[{"kind":"primary_key"}]},`+
		`{"name":"name","type":{"kind":"varchar","size":50},"constraints":[{"kind":"not_null"},{"kind":"default","token":"string","value":"anon"}]},`+
		`{"name":"ok","type":{"kind":"boolean"}}]`, encoded)
}

func TestSchemaCodec_RejectsWhatItCannotRepresent(t *testing.T) {
	t.Run("encode: unknown data type", func(t *testing.T) {
		_, err := EncodeSchema([]parser.ColumnDefinition{{Name: "a", DataType: struct{}{}}})
		assert.ErrorContains(t, err, "unsupported data type")
	})
	t.Run("encode: unknown constraint", func(t *testing.T) {
		_, err := EncodeSchema([]parser.ColumnDefinition{{Name: "a", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{struct{}{}}}})
		assert.ErrorContains(t, err, "unsupported constraint")
	})
	t.Run("encode: default that is neither string nor number", func(t *testing.T) {
		_, err := EncodeSchema([]parser.ColumnDefinition{{Name: "a", DataType: parser.IntDataType{}, Constraints: []parser.Constraint{
			parser.DefaultConstraint{DefaultValue: parser.Token{Type: parser.IDENT, Value: "x"}},
		}}})
		assert.ErrorContains(t, err, "unsupported default")
	})
}

func TestSchemaCodec_DecodeErrors(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"unknown data type":  {`[{"name":"a","type":{"kind":"float"}}]`, `unknown data type "float"`},
		"unknown constraint": {`[{"name":"a","type":{"kind":"int"},"constraints":[{"kind":"check"}]}]`, `unknown constraint "check"`},
		"unknown token tag":  {`[{"name":"a","type":{"kind":"int"},"constraints":[{"kind":"default","token":"ident","value":"x"}]}]`, `unknown default token "ident"`},
		"unknown field":      {`[{"name":"a","type":{"kind":"int"},"extra":1}]`, "extra"},
		"not json":           {`nope`, "decoding schema"},
		"trailing data":      {`[] []`, "decoding schema"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeSchema(tt.in)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}
