package engine

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func codecColumns() []parser.ColumnDefinition {
	return []parser.ColumnDefinition{
		{Name: "id", DataType: parser.BigIntDataType{}},
		{Name: "name", DataType: parser.VarCharDataType{Size: 50}},
		{Name: "active", DataType: parser.BooleanDataType{}},
	}
}

func TestRowCodec_RoundTrip(t *testing.T) {
	columns := codecColumns()
	tests := []struct {
		name string
		row  []any
	}{
		{"all values", []any{int64(7), "bob", true}},
		{"false boolean", []any{int64(1), "x", false}},
		{"negative int", []any{int64(-42), "neg", true}},
		{"min int64", []any{int64(math.MinInt64), "min", false}},
		{"max int64", []any{int64(math.MaxInt64), "max", true}},
		{"empty string", []any{int64(1), "", true}},
		{"unicode string", []any{int64(1), "héllo wörld 日本語", true}},
		{"NULL first", []any{nil, "bob", true}},
		{"NULL middle", []any{int64(1), nil, true}},
		{"NULL last", []any{int64(1), "bob", nil}},
		{"all NULL", []any{nil, nil, nil}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeRow(columns, tc.row)
			require.NoError(t, err)

			decoded, err := DecodeRow(columns, encoded)

			require.NoError(t, err)
			assert.Equal(t, tc.row, decoded)
		})
	}
}

func TestRowCodec_EveryDataType(t *testing.T) {
	columns := []parser.ColumnDefinition{
		{Name: "c", DataType: parser.CharDataType{Size: 10}},
		{Name: "v", DataType: parser.VarCharDataType{Size: 10}},
		{Name: "t", DataType: parser.TextDataType{}},
		{Name: "b", DataType: parser.BooleanDataType{}},
		{Name: "s", DataType: parser.SmallIntDataType{}},
		{Name: "m", DataType: parser.MediumIntDataType{}},
		{Name: "i", DataType: parser.IntDataType{}},
		{Name: "g", DataType: parser.BigIntDataType{}},
	}
	row := []any{"c", "v", "t", true, int64(-1), int64(2), int64(3), int64(4)}

	encoded, err := EncodeRow(columns, row)
	require.NoError(t, err)
	decoded, err := DecodeRow(columns, encoded)

	require.NoError(t, err)
	assert.Equal(t, row, decoded)
}

func TestEncodeRow_Layout(t *testing.T) {
	encoded, err := EncodeRow(codecColumns(), []any{int64(7), "bob", nil})
	require.NoError(t, err)

	want := []byte{
		12, 0, 8, 0, // id: starts at byte 12, 8 bytes long
		20, 0, 3, 0, // name: starts at byte 20, 3 bytes long
		0xFF, 0xFF, 0, 0, // active: NULL
		7, 0, 0, 0, 0, 0, 0, 0, // id payload, little-endian
		'b', 'o', 'b', // name payload
	}
	assert.Equal(t, want, encoded)
}

func TestEncodeRow_EmptyStringIsNotNull(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "s", DataType: parser.TextDataType{}}}

	empty, err := EncodeRow(columns, []any{""})
	require.NoError(t, err)
	null, err := EncodeRow(columns, []any{nil})
	require.NoError(t, err)

	assert.Equal(t, []byte{4, 0, 0, 0}, empty, "a real offset with length 0")
	assert.Equal(t, []byte{0xFF, 0xFF, 0, 0}, null, "the NULL sentinel")
}

func TestRowCodec_ZeroColumns(t *testing.T) {
	encoded, err := EncodeRow(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, encoded)

	decoded, err := DecodeRow(nil, encoded)
	require.NoError(t, err)
	assert.Empty(t, decoded)
}

func TestEncodeRow_Errors(t *testing.T) {
	tests := []struct {
		name    string
		columns []parser.ColumnDefinition
		row     []any
		wantErr string
	}{
		{"too few values", codecColumns(), []any{int64(1)}, "row has 1 values, table has 3 columns"},
		{"too many values", codecColumns(), []any{int64(1), "a", true, "extra"}, "row has 4 values, table has 3 columns"},
		{"string in an int column", codecColumns(), []any{"seven", "bob", true}, `column "id": expected int64, got string`},
		{"plain int instead of int64", codecColumns(), []any{7, "bob", true}, `column "id": expected int64, got int`},
		{"int in a string column", codecColumns(), []any{int64(1), int64(2), true}, `column "name": expected string, got int64`},
		{"string in a bool column", codecColumns(), []any{int64(1), "bob", "true"}, `column "active": expected bool, got string`},
		{
			"unsupported data type, even for NULL",
			[]parser.ColumnDefinition{{Name: "x", DataType: 42}},
			[]any{nil},
			`column "x": unsupported data type int`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EncodeRow(tc.columns, tc.row)

			assert.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestEncodeRow_SizeLimit(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "s", DataType: parser.TextDataType{}}}
	atLimit := strings.Repeat("a", maxCodecRowLen-rowEntrySize)

	encoded, err := EncodeRow(columns, []any{atLimit})
	require.NoError(t, err)
	assert.Len(t, encoded, maxCodecRowLen)
	decoded, err := DecodeRow(columns, encoded)
	require.NoError(t, err)
	assert.Equal(t, []any{atLimit}, decoded)

	_, err = EncodeRow(columns, []any{atLimit + "a"})
	assert.ErrorContains(t, err, "row too large")
}

func TestDecodeRow_RejectsCorruptBytes(t *testing.T) {
	columns := codecColumns()
	// valid is 24 bytes: a 12-byte offset table, then id at 12 (8 bytes),
	// name at 20 (3 bytes) and active at 23 (1 byte).
	valid, err := EncodeRow(columns, []any{int64(7), "bob", true})
	require.NoError(t, err)
	require.Len(t, valid, 24)

	tests := []struct {
		name    string
		mutate  func(b []byte) []byte
		wantErr string
	}{
		{"shorter than its offset table", func(b []byte) []byte { return b[:11] }, "shorter than its"},
		{"payload cut short", func(b []byte) []byte { return b[:len(b)-1] }, `column "active"`},
		{"offset points into the offset table", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[0:], 4)
			return b
		}, `column "id"`},
		{"offset beyond the row", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[4:], 1000)
			return b
		}, `column "name"`},
		{"int with the wrong length", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[2:], 4)
			return b
		}, `column "id": expected 8 bytes`},
		{"bool with the wrong length", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[10:], 0)
			return b
		}, `column "active": expected 1 byte`},
		{"bool that is neither 0 nor 1", func(b []byte) []byte {
			b[23] = 2
			return b
		}, `column "active": invalid boolean byte 2`},
		{"NULL entry that has a length", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[8:], nullOffset)
			binary.LittleEndian.PutUint16(b[10:], 1)
			return b
		}, `column "active": NULL entry has length 1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.mutate(append([]byte(nil), valid...))

			_, err := DecodeRow(columns, b)

			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestDecodeRow_UnsupportedDataType(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "x", DataType: 42}}

	_, err := DecodeRow(columns, []byte{0xFF, 0xFF, 0, 0})

	assert.EqualError(t, err, `column "x": unsupported data type int`)
}

func TestDecodeRow_StringsDoNotAliasTheInput(t *testing.T) {
	columns := []parser.ColumnDefinition{{Name: "s", DataType: parser.TextDataType{}}}
	encoded, err := EncodeRow(columns, []any{"hello"})
	require.NoError(t, err)

	decoded, err := DecodeRow(columns, encoded)
	require.NoError(t, err)
	for i := range encoded {
		encoded[i] = 'X' // a scan's row bytes are only valid until the next row
	}

	assert.Equal(t, []any{"hello"}, decoded)
}
