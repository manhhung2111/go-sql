package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse_SelectWildcard(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{
		Columns: []string{"*"},
		Table:   "users",
	}, stmt)
}

func TestParse_SelectSingleColumn(t *testing.T) {
	stmt, err := parse(t, "SELECT id FROM users")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{
		Columns: []string{"id"},
		Table:   "users",
	}, stmt)
}

func TestParse_SelectMultipleColumns(t *testing.T) {
	stmt, err := parse(t, "SELECT id, name, age FROM users")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{
		Columns: []string{"id", "name", "age"},
		Table:   "users",
	}, stmt)
}

func TestParse_MissingFrom(t *testing.T) {
	_, err := parse(t, "SELECT id")

	assert.EqualError(t, err, "expected FROM, got EOF")
}

func TestParse_TrailingCommaInColumnList(t *testing.T) {
	_, err := parse(t, "SELECT id, FROM users")

	assert.EqualError(t, err, "expected column name, got FROM")
}

func TestParse_NonIdentColumn(t *testing.T) {
	_, err := parse(t, "SELECT 1 FROM users")

	assert.EqualError(t, err, "expected column name, got 1")
}

func TestParse_NonIdentTable(t *testing.T) {
	_, err := parse(t, "SELECT * FROM 123")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_InsertWithColumnsSingleRow(t *testing.T) {
	stmt, err := parse(t, "INSERT INTO users (id, name) VALUES (1, 'bob')")

	assert.NoError(t, err)
	ins := stmt.(InsertIntoStatement)
	assert.Equal(t, "users", ins.Table)
	assert.Equal(t, []string{"id", "name"}, ins.Columns)
	assert.Equal(t, [][]string{{"1", "bob"}}, valueStrings(ins.Values))
}

func TestParse_InsertWithColumnsMultipleRows(t *testing.T) {
	stmt, err := parse(t, "INSERT INTO users (id, name) VALUES (1, 'bob'), (2, 'sam')")

	assert.NoError(t, err)
	ins := stmt.(InsertIntoStatement)
	assert.Equal(t, "users", ins.Table)
	assert.Equal(t, []string{"id", "name"}, ins.Columns)
	assert.Equal(t, [][]string{{"1", "bob"}, {"2", "sam"}}, valueStrings(ins.Values))
}

func TestParse_InsertWithoutColumnList(t *testing.T) {
	stmt, err := parse(t, "INSERT INTO users VALUES (1, 'bob')")

	assert.NoError(t, err)
	ins := stmt.(InsertIntoStatement)
	assert.Equal(t, "users", ins.Table)
	assert.Equal(t, []string{}, ins.Columns)
	assert.Equal(t, [][]string{{"1", "bob"}}, valueStrings(ins.Values))
}

func TestParse_InsertMissingInto(t *testing.T) {
	_, err := parse(t, "INSERT users (id) VALUES (1)")

	assert.EqualError(t, err, "INTO keyword must be expected after INSERT, got users")
}

func TestParse_InsertNonIdentTable(t *testing.T) {
	_, err := parse(t, "INSERT INTO 123 VALUES (1)")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_InsertEmptyColumnList(t *testing.T) {
	_, err := parse(t, "INSERT INTO users () VALUES (1)")

	assert.EqualError(t, err, "expected column name, got )")
}

func TestParse_InsertNonIdentColumn(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (1) VALUES (1)")

	assert.EqualError(t, err, "expected column name, got 1")
}

func TestParse_InsertMissingCloseParenAfterColumns(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id, name VALUES (1, 'bob')")

	assert.EqualError(t, err, "expected ')' after column list, got VALUES")
}

func TestParse_InsertMissingValuesKeyword(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id) (1)")

	assert.EqualError(t, err, "expected VALUES, got (")
}

func TestParse_InsertMissingOpenParenBeforeRow(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id) VALUES 1)")

	assert.EqualError(t, err, "expected '(' before value list, got 1")
}

func TestParse_InsertMissingCloseParenAfterRow(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id) VALUES (1")

	assert.EqualError(t, err, "expected ')' after value list, got EOF")
}

func TestParse_InsertNonLiteralValue(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id) VALUES (id)")

	assert.EqualError(t, err, "expected value, got id")
}

func TestParse_InsertTrailingCommaInColumnList(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id,) VALUES (1)")

	assert.EqualError(t, err, "expected column name, got )")
}

func TestParse_InsertTrailingCommaInValueRow(t *testing.T) {
	_, err := parse(t, "INSERT INTO users (id) VALUES (1,)")

	assert.EqualError(t, err, "expected value, got )")
}

func TestParse_InsertEmptyColumnListLowercaseValuesDoubleQuotedStrings(t *testing.T) {
	_, err := parse(t, `INSERT INTO users () values ("abc", "DEF")`)

	assert.EqualError(t, err, "expected column name, got )")
}

func TestParse_UpdateSingleAssignment(t *testing.T) {
	stmt, err := parse(t, "UPDATE users SET age = 30")

	assert.NoError(t, err)
	upd := stmt.(UpdateStatement)
	assert.Equal(t, "users", upd.Table)
	assert.Equal(t, []string{"age=30"}, setStrings(upd.Set))
	assert.Nil(t, upd.Where)
}

func TestParse_UpdateMultipleAssignments(t *testing.T) {
	stmt, err := parse(t, "UPDATE users SET age = 30, name = 'bob'")

	assert.NoError(t, err)
	upd := stmt.(UpdateStatement)
	assert.Equal(t, "users", upd.Table)
	assert.Equal(t, []string{"age=30", "name=bob"}, setStrings(upd.Set))
}

func TestParse_UpdateWithWhere(t *testing.T) {
	stmt, err := parse(t, "UPDATE users SET age = 30 WHERE id = 1")

	assert.NoError(t, err)
	upd := stmt.(UpdateStatement)
	assert.Equal(t, "users", upd.Table)
	assert.Equal(t, []string{"age=30"}, setStrings(upd.Set))
	assert.Equal(t, "id = 1", whereString(upd.Where))
}

func TestParse_UpdateNonIdentTable(t *testing.T) {
	_, err := parse(t, "UPDATE 123 SET age = 30")

	assert.EqualError(t, err, "expected table name, got 123")
}

func TestParse_UpdateMissingSet(t *testing.T) {
	_, err := parse(t, "UPDATE users age = 30")

	assert.EqualError(t, err, "expected SET, got age")
}

func TestParse_UpdateNonIdentColumn(t *testing.T) {
	_, err := parse(t, "UPDATE users SET 1 = 30")

	assert.EqualError(t, err, "expected column name, got 1")
}

func TestParse_UpdateMissingEquals(t *testing.T) {
	_, err := parse(t, "UPDATE users SET age 30")

	assert.EqualError(t, err, "expected '=', got 30")
}

func TestParse_UpdateNonLiteralValue(t *testing.T) {
	_, err := parse(t, "UPDATE users SET age = name")

	assert.EqualError(t, err, "expected value, got name")
}

func TestParse_UpdateDuplicateColumn(t *testing.T) {
	_, err := parse(t, "UPDATE users SET age = 1, age = 2")

	assert.EqualError(t, err, "duplicate assignment for column age")
}

func TestParse_UpdateTrailingComma(t *testing.T) {
	_, err := parse(t, "UPDATE users SET age = 30,")

	assert.EqualError(t, err, "expected column name, got EOF")
}

func TestParse_DeleteWithoutWhere(t *testing.T) {
	stmt, err := parse(t, "DELETE FROM users")

	assert.NoError(t, err)
	assert.Equal(t, DeleteStatement{Table: "users"}, stmt)
}

func TestParse_DeleteWithWhere(t *testing.T) {
	stmt, err := parse(t, "DELETE FROM users WHERE age > 18")

	assert.NoError(t, err)
	del := stmt.(DeleteStatement)
	assert.Equal(t, "users", del.Table)
	assert.Equal(t, "age > 18", whereString(del.Where))
}

func TestParse_DeleteMissingFrom(t *testing.T) {
	_, err := parse(t, "DELETE users")

	assert.EqualError(t, err, "FROM keyword must be expected after DELETE, got users")
}

func TestParse_DeleteNonIdentTable(t *testing.T) {
	_, err := parse(t, "DELETE FROM 123")

	assert.EqualError(t, err, "expected table name, got 123")
}
