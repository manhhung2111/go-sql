package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"manhhung2111/go-sql/internal/parser"
)

func TestCoerceValue_Nil(t *testing.T) {
	for _, dt := range []parser.DataType{
		parser.CharDataType{}, parser.VarCharDataType{}, parser.TextDataType{},
		parser.BooleanDataType{}, parser.SmallIntDataType{}, parser.MediumIntDataType{},
		parser.IntDataType{}, parser.BigIntDataType{},
	} {
		v, err := coerceValue(dt, nil)
		assert.NoError(t, err)
		assert.Nil(t, v)
	}
}

func TestCoerceValue_String(t *testing.T) {
	v, err := coerceValue(parser.VarCharDataType{Size: 5}, strTok("bob"))
	assert.NoError(t, err)
	assert.Equal(t, "bob", v)
}

func TestCoerceValue_String_WrongKind(t *testing.T) {
	_, err := coerceValue(parser.VarCharDataType{}, numTok("1"))
	assert.EqualError(t, err, "expected a string value, got 1")
}

func TestCoerceValue_String_TooLong(t *testing.T) {
	_, err := coerceValue(parser.VarCharDataType{Size: 3}, strTok("robert"))
	assert.EqualError(t, err, `value "robert" too long for max length 3`)
}

func TestCoerceValue_Int(t *testing.T) {
	v, err := coerceValue(parser.IntDataType{}, numTok("42"))
	assert.NoError(t, err)
	assert.Equal(t, int64(42), v)
}

func TestCoerceValue_Int_WrongKind(t *testing.T) {
	_, err := coerceValue(parser.IntDataType{}, strTok("abc"))
	assert.EqualError(t, err, "expected a numeric value, got abc")
}

func TestCoerceValue_Int_NotAnInteger(t *testing.T) {
	_, err := coerceValue(parser.IntDataType{}, numTok("18.5"))
	assert.EqualError(t, err, `value "18.5" is not a valid integer`)
}

func TestCoerceValue_SmallInt_OutOfRange(t *testing.T) {
	_, err := coerceValue(parser.SmallIntDataType{}, numTok("32768"))
	assert.EqualError(t, err, "value 32768 out of range for SMALLINT")
}

func TestCoerceValue_MediumInt_OutOfRange(t *testing.T) {
	_, err := coerceValue(parser.MediumIntDataType{}, numTok("8388608"))
	assert.EqualError(t, err, "value 8388608 out of range for MEDIUMINT")
}

func TestCoerceValue_Int_OutOfRange(t *testing.T) {
	_, err := coerceValue(parser.IntDataType{}, numTok("2147483648"))
	assert.EqualError(t, err, "value 2147483648 out of range for INT")
}

func TestCoerceValue_Int_SizeIsDisplayOnly(t *testing.T) {
	// A declared size (display width) must never reject a value that
	// fits the type's real range.
	v, err := coerceValue(parser.IntDataType{Size: 2}, numTok("2147483647"))
	assert.NoError(t, err)
	assert.Equal(t, int64(2147483647), v)
}

func TestCoerceValue_BigInt(t *testing.T) {
	v, err := coerceValue(parser.BigIntDataType{}, numTok("9223372036854775807"))
	assert.NoError(t, err)
	assert.Equal(t, int64(9223372036854775807), v)
}

func TestCoerceValue_Boolean_FromNumber(t *testing.T) {
	v, err := coerceValue(parser.BooleanDataType{}, numTok("1"))
	assert.NoError(t, err)
	assert.Equal(t, true, v)

	v, err = coerceValue(parser.BooleanDataType{}, numTok("0"))
	assert.NoError(t, err)
	assert.Equal(t, false, v)
}

func TestCoerceValue_Boolean_FromString(t *testing.T) {
	v, err := coerceValue(parser.BooleanDataType{}, strTok("TRUE"))
	assert.NoError(t, err)
	assert.Equal(t, true, v)

	v, err = coerceValue(parser.BooleanDataType{}, strTok("false"))
	assert.NoError(t, err)
	assert.Equal(t, false, v)
}

func TestCoerceValue_Boolean_Invalid(t *testing.T) {
	_, err := coerceValue(parser.BooleanDataType{}, numTok("2"))
	assert.EqualError(t, err, "expected a boolean value, got 2")

	_, err = coerceValue(parser.BooleanDataType{}, strTok("yes"))
	assert.EqualError(t, err, "expected a boolean value, got yes")
}
