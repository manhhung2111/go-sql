package parser

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func parse(t *testing.T, sql string) (SqlStatement, error) {
	t.Helper()
	tokens := NewLexer(sql).Lexing()
	return NewParser(tokens).Parse()
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

func TestParse_UnknownCommand(t *testing.T) {
	_, err := parse(t, "MERGE users SET x")

	assert.EqualError(t, err, "command not implemented, got MERGE")
}

func TestParse_WhereSingleComparison(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18")

	assert.NoError(t, err)
	ss := stmt.(SelectStatement)
	assert.Equal(t, "age > 18", whereString(ss.Where))
}

func TestParse_WhereComparisonOperators(t *testing.T) {
	tests := []struct {
		sql      string
		expected string
	}{
		{"SELECT * FROM users WHERE age = 18", "age = 18"},
		{"SELECT * FROM users WHERE age != 18", "age != 18"},
		{"SELECT * FROM users WHERE age < 18", "age < 18"},
		{"SELECT * FROM users WHERE age <= 18", "age <= 18"},
		{"SELECT * FROM users WHERE age > 18", "age > 18"},
		{"SELECT * FROM users WHERE age >= 18", "age >= 18"},
		{"SELECT * FROM users WHERE name = 'bob'", "name = bob"},
	}

	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			stmt, err := parse(t, tt.sql)
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, whereString(stmt.(SelectStatement).Where))
		})
	}
}

func TestParse_WhereAndChain(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18 AND name = 'bob'")

	assert.NoError(t, err)
	assert.Equal(t, "(age > 18 AND name = bob)", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereMultipleAndChain(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE a = 1 AND b = 2 AND c = 3")

	assert.NoError(t, err)
	assert.Equal(t, "(a = 1 AND (b = 2 AND c = 3))", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereOrChain(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18 OR age < 5")

	assert.NoError(t, err)
	assert.Equal(t, "(age > 18 OR age < 5)", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereAndBindsTighterThanOr(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users WHERE age > 18 AND name = 'bob' OR flag = 1")

	assert.NoError(t, err)
	assert.Equal(t, "((age > 18 AND name = bob) OR flag = 1)", whereString(stmt.(SelectStatement).Where))
}

func TestParse_WhereMissingOperator(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age 18")

	assert.EqualError(t, err, "expected comparison operator, got 18")
}

func TestParse_WhereMissingRightOperand(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age >")

	assert.EqualError(t, err, "expected identifier, number or string, got EOF")
}

func TestParse_WhereDanglingAnd(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age > 18 AND")

	assert.EqualError(t, err, "expected identifier, number or string, got EOF")
}

func TestParse_WhereDanglingOr(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE age > 18 OR")

	assert.EqualError(t, err, "expected identifier, number or string, got EOF")
}

func TestParse_WhereLeadingOperator(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users WHERE > 18")

	assert.EqualError(t, err, "expected identifier, number or string, got >")
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

func TestParse_TrailingSemicolonAllowed(t *testing.T) {
	stmt, err := parse(t, "SELECT * FROM users;")

	assert.NoError(t, err)
	assert.Equal(t, SelectStatement{Columns: []string{"*"}, Table: "users"}, stmt)
}

func TestParse_TrailingGarbageAfterStatement(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users EXTRA")

	assert.EqualError(t, err, "unexpected token after statement, got EXTRA")
}

func TestParse_TrailingStatementAfterSemicolonRejected(t *testing.T) {
	_, err := parse(t, "SELECT * FROM users; SELECT 1")

	assert.EqualError(t, err, "unexpected token after statement, got SELECT")
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
