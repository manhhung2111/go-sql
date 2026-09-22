package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse_CreateDatabase(t *testing.T) {
	stmt, err := parse(t, "CREATE DATABASE mydb")

	assert.NoError(t, err)
	assert.Equal(t, CreateDatabaseStatement{Database: "mydb"}, stmt)
}

func TestParse_CreateMissingDatabaseKeyword(t *testing.T) {
	_, err := parse(t, "CREATE mydb")

	assert.EqualError(t, err, "DATABASE keyword must be expected after CREATE, got mydb")
}

func TestParse_CreateNonIdentDatabase(t *testing.T) {
	_, err := parse(t, "CREATE DATABASE 123")

	assert.EqualError(t, err, "expected database name, got 123")
}

func TestParse_DropDatabase(t *testing.T) {
	stmt, err := parse(t, "DROP DATABASE mydb")

	assert.NoError(t, err)
	assert.Equal(t, DropDatabaseStatement{Database: "mydb"}, stmt)
}

func TestParse_DropMissingDatabaseOrTableKeyword(t *testing.T) {
	_, err := parse(t, "DROP mydb")

	assert.EqualError(t, err, "DATABASE or TABLE keyword must be expected after DROP, got mydb")
}

func TestParse_DropNonIdentDatabase(t *testing.T) {
	_, err := parse(t, "DROP DATABASE 123")

	assert.EqualError(t, err, "expected database name, got 123")
}

func TestParse_DropTable(t *testing.T) {
	stmt, err := parse(t, "DROP TABLE users")

	assert.NoError(t, err)
	assert.Equal(t, DropTableStatement{Table: "users"}, stmt)
}

func TestParse_DropTableNonIdentTable(t *testing.T) {
	_, err := parse(t, "DROP TABLE 123")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_DropTableIfExists(t *testing.T) {
	stmt, err := parse(t, "DROP TABLE IF EXISTS users")

	assert.NoError(t, err)
	assert.Equal(t, DropTableStatement{Table: "users", IfExists: true}, stmt)
}

func TestParse_DropTableMissingExistsAfterIf(t *testing.T) {
	_, err := parse(t, "DROP TABLE IF users")

	assert.EqualError(t, err, "expected EXISTS after IF, got users")
}

func TestParse_ShowDatabases(t *testing.T) {
	stmt, err := parse(t, "SHOW DATABASES")

	assert.NoError(t, err)
	assert.Equal(t, ShowDatabasesStatement{}, stmt)
}

func TestParse_ShowMissingDatabasesKeyword(t *testing.T) {
	_, err := parse(t, "SHOW TABLES")

	assert.EqualError(t, err, "DATABASES keyword must be expected after SHOW, got TABLES")
}

func TestParse_CreateTableSingleColumn(t *testing.T) {
	stmt, err := parse(t, "CREATE TABLE foo (id INT)")

	assert.NoError(t, err)
	tbl := stmt.(CreateTableStatement)
	assert.Equal(t, "foo", tbl.Table)
	assert.False(t, tbl.IfNotExists)
	assert.Equal(t, []string{"id INT(0)"}, columnDefinitionStrings(tbl.Columns))
}

func TestParse_CreateTableMultipleColumnsWithSizesAndConstraints(t *testing.T) {
	stmt, err := parse(t, "CREATE TABLE foo (id INT PRIMARY KEY, name VARCHAR(50) NOT NULL, active BOOLEAN DEFAULT 1)")

	assert.NoError(t, err)
	tbl := stmt.(CreateTableStatement)
	assert.Equal(t, "foo", tbl.Table)
	assert.Equal(t, []string{
		"id INT(0) PRIMARY KEY",
		"name VARCHAR(50) NOT NULL",
		"active BOOLEAN DEFAULT 1",
	}, columnDefinitionStrings(tbl.Columns))
}

func TestParse_CreateTableMultipleConstraintsSameColumn(t *testing.T) {
	stmt, err := parse(t, "CREATE TABLE foo (id INT NOT NULL UNIQUE PRIMARY KEY)")

	assert.NoError(t, err)
	tbl := stmt.(CreateTableStatement)
	assert.Equal(t, []string{"id INT(0) NOT NULL UNIQUE PRIMARY KEY"}, columnDefinitionStrings(tbl.Columns))
}

func TestParse_CreateTableIfNotExists(t *testing.T) {
	stmt, err := parse(t, "CREATE TABLE IF NOT EXISTS foo (id INT)")

	assert.NoError(t, err)
	tbl := stmt.(CreateTableStatement)
	assert.True(t, tbl.IfNotExists)
}

func TestParse_CreateTableQuotedColumnName(t *testing.T) {
	stmt, err := parse(t, `CREATE TABLE foo ('id' INT)`)

	assert.NoError(t, err)
	tbl := stmt.(CreateTableStatement)
	assert.Equal(t, []string{"id INT(0)"}, columnDefinitionStrings(tbl.Columns))
}

func TestParse_CreateTableMissingNotAfterIf(t *testing.T) {
	_, err := parse(t, "CREATE TABLE IF EXISTS foo (id INT)")

	assert.EqualError(t, err, "expected NOT after IF, got EXISTS")
}

func TestParse_CreateTableMissingExistsAfterIfNot(t *testing.T) {
	_, err := parse(t, "CREATE TABLE IF NOT foo (id INT)")

	assert.EqualError(t, err, "expected EXISTS after IF NOT, got foo")
}

func TestParse_CreateTableNonIdentTableName(t *testing.T) {
	_, err := parse(t, "CREATE TABLE 123 (id INT)")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_CreateTableMissingOpenParen(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo id INT)")

	assert.EqualError(t, err, "expected '(' before column list, got id")
}

func TestParse_CreateTableNonIdentColumnName(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo (123 INT)")

	assert.EqualError(t, err, "expected column name, got 123")
}

func TestParse_CreateTableUnknownDataType(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo (id UNKNOWNTYPE)")

	assert.EqualError(t, err, "expected data type, got UNKNOWNTYPE")
}

func TestParse_CreateTableNonNumberSize(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo (id VARCHAR(abc))")

	assert.EqualError(t, err, "expected size in data type, got abc")
}

func TestParse_CreateTableMissingCloseParenAfterSize(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo (id VARCHAR(50 name INT)")

	assert.EqualError(t, err, "expected ')' after size in data type, got name")
}

func TestParse_CreateTableUnsupportedSizeDataType(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo (id BOOLEAN(5))")

	assert.EqualError(t, err, "parser.BooleanDataType does not accept a size argument")
}

func TestParse_CreateTableMissingNullAfterNot(t *testing.T) {
	tests := []struct {
		sql string
		err string
	}{
		{"CREATE TABLE foo (id INT NOT", "expected NULL after NOT constraint, got EOF"},
		{"CREATE TABLE foo (id INT NOT 5)", "expected NULL after NOT constraint, got 5"},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			_, err := parse(t, tt.sql)
			assert.EqualError(t, err, tt.err)
		})
	}
}

func TestParse_CreateTableMissingKeyAfterPrimary(t *testing.T) {
	tests := []struct {
		sql string
		err string
	}{
		{"CREATE TABLE foo (id INT PRIMARY", "expected KEY after PRIMARY constraint, got EOF"},
		{"CREATE TABLE foo (id INT PRIMARY 5)", "expected KEY after PRIMARY constraint, got 5"},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			_, err := parse(t, tt.sql)
			assert.EqualError(t, err, tt.err)
		})
	}
}

func TestParse_CreateTableNonLiteralDefaultValue(t *testing.T) {
	_, err := parse(t, "CREATE TABLE foo (id INT DEFAULT name)")

	assert.EqualError(t, err, "expected String or Number default value for DEFAULT constraint, got name")
}

func TestParse_CreateTableMissingCloseParenAfterColumnList(t *testing.T) {
	tests := []string{
		"CREATE TABLE foo (id INT",
		"CREATE TABLE foo (id INT, name VARCHAR(50)",
	}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			_, err := parse(t, sql)
			assert.EqualError(t, err, "expected ')' after column list, got EOF")
		})
	}
}

func TestParse_AlterTableAddColumn(t *testing.T) {
	stmt, err := parse(t, "ALTER TABLE users ADD COLUMN age INT NOT NULL")

	assert.NoError(t, err)
	assert.Equal(t, AlterTableStatement{
		Table: "users",
		Action: AddColumnAction{
			Column: ColumnDefinition{
				Name:        "age",
				DataType:    IntDataType{},
				Constraints: []Constraint{NotNullConstraint{}},
			},
		},
	}, stmt)
}

func TestParse_AlterTableAddColumnWithoutColumnKeyword(t *testing.T) {
	stmt, err := parse(t, "ALTER TABLE users ADD age INT")

	assert.NoError(t, err)
	alt := stmt.(AlterTableStatement)
	assert.Equal(t, AddColumnAction{
		Column: ColumnDefinition{Name: "age", DataType: IntDataType{}, Constraints: []Constraint{}},
	}, alt.Action)
}

func TestParse_AlterTableDropColumn(t *testing.T) {
	stmt, err := parse(t, "ALTER TABLE users DROP COLUMN age")

	assert.NoError(t, err)
	assert.Equal(t, AlterTableStatement{
		Table:  "users",
		Action: DropColumnAction{Column: "age"},
	}, stmt)
}

func TestParse_AlterTableDropColumnWithoutColumnKeyword(t *testing.T) {
	stmt, err := parse(t, "ALTER TABLE users DROP age")

	assert.NoError(t, err)
	assert.Equal(t, AlterTableStatement{
		Table:  "users",
		Action: DropColumnAction{Column: "age"},
	}, stmt)
}

func TestParse_AlterTableRenameColumn(t *testing.T) {
	stmt, err := parse(t, "ALTER TABLE users RENAME COLUMN age TO years")

	assert.NoError(t, err)
	assert.Equal(t, AlterTableStatement{
		Table:  "users",
		Action: RenameColumnAction{OldName: "age", NewName: "years"},
	}, stmt)
}

func TestParse_AlterTableRenameTo(t *testing.T) {
	stmt, err := parse(t, "ALTER TABLE users RENAME TO people")

	assert.NoError(t, err)
	assert.Equal(t, AlterTableStatement{
		Table:  "users",
		Action: RenameTableAction{NewName: "people"},
	}, stmt)
}

func TestParse_AlterTableMissingTableKeyword(t *testing.T) {
	_, err := parse(t, "ALTER users ADD COLUMN age INT")

	assert.EqualError(t, err, "TABLE keyword must be expected after ALTER, got users")
}

func TestParse_AlterTableNonIdentTableName(t *testing.T) {
	_, err := parse(t, "ALTER TABLE 123 ADD COLUMN age INT")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_AlterTableUnknownAction(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users MODIFY age INT")

	assert.EqualError(t, err, "expected ADD, DROP or RENAME after table name, got MODIFY")
}

func TestParse_AlterTableAddColumnNonIdentName(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users ADD COLUMN 123 INT")

	assert.EqualError(t, err, "expected column name, got 123")
}

func TestParse_AlterTableDropColumnNonIdentName(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users DROP 123")

	assert.EqualError(t, err, "expected column name, got 123")
}

func TestParse_AlterTableRenameColumnMissingTo(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users RENAME COLUMN age")

	assert.EqualError(t, err, "expected TO after RENAME COLUMN age, got EOF")
}

func TestParse_AlterTableRenameColumnNonIdentNewName(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users RENAME COLUMN age TO 123")

	assert.EqualError(t, err, "expected column name, got 123")
}

func TestParse_AlterTableRenameMissingColumnOrTo(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users RENAME 123")

	assert.EqualError(t, err, "expected COLUMN or TO after RENAME, got 123")
}

func TestParse_AlterTableRenameToNonIdentName(t *testing.T) {
	_, err := parse(t, "ALTER TABLE users RENAME TO 123")

	assert.EqualError(t, err, "expected table name, got 123")
}
