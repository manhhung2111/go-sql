package engine

import (
	"encoding/binary"
	"fmt"

	"manhhung2111/go-sql/internal/parser"
)

const (
	// rowEntrySize is one column's slot in the row's offset table:
	// offset uint16 + length uint16.
	rowEntrySize = 2 + 2
	// nullOffset marks a NULL column. A real payload can never start there
	// because maxCodecRowLen keeps every offset below it.
	nullOffset = 0xFFFF
	// maxCodecRowLen is the largest encoded row: offsets and lengths are
	// uint16, and 0xFFFF is reserved for NULL.
	maxCodecRowLen = nullOffset - 1
)

type valueKind int

const (
	kindString valueKind = iota
	kindInt
	kindBool
)

// kindOf maps a column's DataType to how its values are stored. Every integer
// type is an int64 in memory (coerceValue range-checks each against its own
// SQL range), so they all share one 8-byte encoding.
func kindOf(dt parser.DataType) (valueKind, error) {
	switch dt.(type) {
	case parser.CharDataType, parser.VarCharDataType, parser.TextDataType:
		return kindString, nil
	case parser.BooleanDataType:
		return kindBool, nil
	case parser.SmallIntDataType, parser.MediumIntDataType, parser.IntDataType, parser.BigIntDataType:
		return kindInt, nil
	default:
		return 0, fmt.Errorf("unsupported data type %T", dt)
	}
}

// EncodeRow turns a row of coerced values (string, int64, bool or nil for
// NULL, in column order) into bytes: an (offset, length) entry per column,
// then the payloads. Offsets are measured from the start of the returned
// slice. The column count is not stored; DecodeRow is given the schema.
func EncodeRow(columns []parser.ColumnDefinition, row []any) ([]byte, error) {
	if len(row) != len(columns) {
		return nil, fmt.Errorf("row has %d values, table has %d columns", len(row), len(columns))
	}

	headerSize := len(columns) * rowEntrySize
	if headerSize > maxCodecRowLen {
		return nil, fmt.Errorf("row too large: %d columns need a %d-byte offset table, limit is %d", len(columns), headerSize, maxCodecRowLen)
	}

	// The offset table is zero-filled up front and filled in as each payload
	// is appended, so every column's offset is the running length at that
	// moment.
	out := make([]byte, headerSize)
	for i, col := range columns {
		kind, err := kindOf(col.DataType)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", col.Name, err)
		}

		entry := i * rowEntrySize
		if row[i] == nil {
			binary.LittleEndian.PutUint16(out[entry:], nullOffset)
			continue
		}

		start := len(out)
		switch kind {
		case kindString:
			s, ok := row[i].(string)
			if !ok {
				return nil, fmt.Errorf("column %q: expected string, got %T", col.Name, row[i])
			}
			out = append(out, s...)
		case kindInt:
			n, ok := row[i].(int64)
			if !ok {
				return nil, fmt.Errorf("column %q: expected int64, got %T", col.Name, row[i])
			}
			out = binary.LittleEndian.AppendUint64(out, uint64(n))
		case kindBool:
			b, ok := row[i].(bool)
			if !ok {
				return nil, fmt.Errorf("column %q: expected bool, got %T", col.Name, row[i])
			}
			if b {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		}
		if len(out) > maxCodecRowLen {
			return nil, fmt.Errorf("row too large: %d bytes exceeds the limit of %d", len(out), maxCodecRowLen)
		}

		binary.LittleEndian.PutUint16(out[entry:], uint16(start))
		binary.LittleEndian.PutUint16(out[entry+2:], uint16(len(out)-start))
	}

	return out, nil
}

// DecodeRow is EncodeRow's inverse. The bytes come from disk, so every offset
// and length is validated against the buffer: corrupt input is an error,
// never a panic or an out-of-range read. Strings are copied, so the result
// stays valid after b is reused.
func DecodeRow(columns []parser.ColumnDefinition, b []byte) ([]any, error) {
	headerSize := len(columns) * rowEntrySize
	if len(b) < headerSize {
		return nil, fmt.Errorf("row is %d bytes, shorter than its %d-byte offset table", len(b), headerSize)
	}

	row := make([]any, len(columns))
	for i, col := range columns {
		kind, err := kindOf(col.DataType)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", col.Name, err)
		}

		entry := i * rowEntrySize
		offset := int(binary.LittleEndian.Uint16(b[entry:]))
		length := int(binary.LittleEndian.Uint16(b[entry+2:]))

		if offset == nullOffset {
			if length != 0 {
				return nil, fmt.Errorf("column %q: NULL entry has length %d", col.Name, length)
			}
			continue // row[i] stays nil
		}
		if offset < headerSize || offset+length > len(b) {
			return nil, fmt.Errorf("column %q: bytes [%d, %d) outside row payload [%d, %d)", col.Name, offset, offset+length, headerSize, len(b))
		}

		payload := b[offset : offset+length]
		switch kind {
		case kindString:
			row[i] = string(payload)
		case kindInt:
			if length != 8 {
				return nil, fmt.Errorf("column %q: expected 8 bytes for an integer, got %d", col.Name, length)
			}
			row[i] = int64(binary.LittleEndian.Uint64(payload))
		case kindBool:
			if length != 1 {
				return nil, fmt.Errorf("column %q: expected 1 byte for a boolean, got %d", col.Name, length)
			}
			if payload[0] > 1 {
				return nil, fmt.Errorf("column %q: invalid boolean byte %d", col.Name, payload[0])
			}
			row[i] = payload[0] == 1
		}
	}

	return row, nil
}
