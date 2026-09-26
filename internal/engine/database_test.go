package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/parser"
)

func TestDatabase_CreateTable(t *testing.T) {
	db := NewDatabase("testdb")
	columns := []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}

	err := db.CreateTable("users", columns, false)

	require.NoError(t, err)
	table, exists := db.GetTable("users")
	require.True(t, exists)
	assert.Equal(t, columns, table.(*SqlTable).Columns)
}

func TestDatabase_CreateTable_AlreadyExists(t *testing.T) {
	db := NewDatabase("testdb")
	require.NoError(t, db.CreateTable("users", nil, false))

	err := db.CreateTable("users", nil, false)

	assert.EqualError(t, err, `table "users" already exists`)
}

func TestDatabase_CreateTable_IfNotExists_NewTable(t *testing.T) {
	db := NewDatabase("testdb")
	columns := []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}

	err := db.CreateTable("users", columns, true)

	require.NoError(t, err)
	table, exists := db.GetTable("users")
	require.True(t, exists)
	assert.Equal(t, columns, table.(*SqlTable).Columns)
}

func TestDatabase_CreateTable_IfNotExists_PreservesExistingSchema(t *testing.T) {
	db := NewDatabase("testdb")
	original := []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}
	require.NoError(t, db.CreateTable("users", original, false))

	different := []parser.ColumnDefinition{{Name: "name", DataType: parser.VarCharDataType{Size: 50}}}
	err := db.CreateTable("users", different, true)

	require.NoError(t, err)
	table, exists := db.GetTable("users")
	require.True(t, exists)
	assert.Equal(t, original, table.(*SqlTable).Columns, "IF NOT EXISTS must not overwrite the existing table")
}

func TestDatabase_GetTable_NotFound(t *testing.T) {
	db := NewDatabase("testdb")

	_, exists := db.GetTable("users")

	assert.False(t, exists)
}

func TestDatabase_ConcurrentCreateTable(t *testing.T) {
	db := NewDatabase("testdb")
	const n = 50

	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			name := "table" + string(rune('a'+i%26))
			_ = db.CreateTable(name, nil, true)
			db.GetTable(name)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
