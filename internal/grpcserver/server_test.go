package grpcserver

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/proto/sqlpb"
)

func newTestClient(t *testing.T) sqlpb.SqlParserServiceClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { lis.Close() })

	sqlParser := parser.NewParser(parser.NewLexer())
	catalog, err := engine.NewCatalog(engine.DataDir(t.TempDir()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	sqlEngine := engine.NewEngine(catalog)

	grpcServer := grpc.NewServer()
	sqlpb.RegisterSqlParserServiceServer(grpcServer, NewServer(&config.Config{}, sqlParser, sqlEngine))
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return sqlpb.NewSqlParserServiceClient(conn)
}

func TestParseQuery_ValidStatements(t *testing.T) {
	client := newTestClient(t)

	sqls := []string{
		"CREATE DATABASE testdb",
		"SHOW DATABASES",
	}

	for _, sql := range sqls {
		t.Run(sql, func(t *testing.T) {
			resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: sql})

			require.NoError(t, err)
			assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
			assert.Empty(t, resp.GetErrorMessage())
		})
	}
}

func TestParseQuery_CreateThenDropDatabase(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "DROP DATABASE testdb"})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())
}

func TestParseQuery_ShowDatabasesReturnsRows(t *testing.T) {
	client := newTestClient(t)

	_, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE zebra"})
	require.NoError(t, err)
	_, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE apple"})
	require.NoError(t, err)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "SHOW DATABASES"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Equal(t, []string{"Database"}, resp.GetColumns())
	assert.Equal(t, [][]string{{"apple"}, {"zebra"}}, rowStrings(resp.GetRows()))
}

func TestParseQuery_ExecutionErrors(t *testing.T) {
	tests := []struct {
		name          string
		sqls          []string
		errorContains string
	}{
		{
			name:          "create database twice",
			sqls:          []string{"CREATE DATABASE testdb", "CREATE DATABASE testdb"},
			errorContains: `database "testdb" already exists`,
		},
		{
			name:          "drop database never created",
			sqls:          []string{"DROP DATABASE testdb"},
			errorContains: `database "testdb" does not exist`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t)

			var resp *sqlpb.QueryResponse
			var err error
			for _, sql := range tt.sqls {
				resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: sql})
				require.NoError(t, err)
			}

			assert.Equal(t, sqlpb.StatusCode_EXECUTION_ERROR, resp.GetCode())
			assert.Equal(t, tt.errorContains, resp.GetErrorMessage())
		})
	}
}

func TestParseQuery_CreateTable(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT)",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())
}

func TestParseQuery_CreateTable_NoDatabaseSelected(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE TABLE users (id INT)"})

	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_EXECUTION_ERROR, resp.GetCode())
	assert.Equal(t, "database name is required", resp.GetErrorMessage())
}

func TestParseQuery_CreateTable_DatabaseDoesNotExist(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT)",
		Database: "testdb",
	})

	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_EXECUTION_ERROR, resp.GetCode())
	assert.Equal(t, `database "testdb" does not exist`, resp.GetErrorMessage())
}

func TestParseQuery_InsertInto(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT, name VARCHAR(50))",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "INSERT INTO users (id, name) VALUES (1, 'bob')",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())
}

func TestParseQuery_AlterTable(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT)",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "ALTER TABLE users ADD COLUMN name VARCHAR(50)",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "ALTER TABLE users RENAME TO people",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "INSERT INTO people (id, name) VALUES (1, 'bob')",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())
}

func TestParseQuery_DropTable(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT)",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "DROP TABLE users",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "DROP TABLE users",
		Database: "testdb",
	})
	require.NoError(t, err)
	assert.Equal(t, sqlpb.StatusCode_EXECUTION_ERROR, resp.GetCode())
	assert.Equal(t, `table "users" does not exist`, resp.GetErrorMessage())
}

func TestParseQuery_Update(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT, name VARCHAR(50))",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "INSERT INTO users (id, name) VALUES (1, 'bob'), (2, 'sam')",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "UPDATE users SET name = 'robert' WHERE id = 1",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "SELECT * FROM users",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	// An UPDATE writes the new version of a row at the end of the table's file,
	// so row order after an UPDATE is unspecified.
	assert.ElementsMatch(t, [][]string{{"1", "robert"}, {"2", "sam"}}, rowStrings(resp.GetRows()))
}

func TestParseQuery_Delete(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT, name VARCHAR(50))",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "INSERT INTO users (id, name) VALUES (1, 'bob'), (2, 'sam')",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "DELETE FROM users WHERE id = 1",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Empty(t, resp.GetErrorMessage())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "SELECT * FROM users",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Equal(t, [][]string{{"2", "sam"}}, rowStrings(resp.GetRows()))
}

func TestParseQuery_Select(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: "CREATE DATABASE testdb"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "CREATE TABLE users (id INT, name VARCHAR(50))",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "INSERT INTO users (id, name) VALUES (1, 'bob'), (2, 'sam')",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())

	resp, err = client.ParseQuery(context.Background(), &sqlpb.QueryRequest{
		Sql:      "SELECT name FROM users WHERE id = 2",
		Database: "testdb",
	})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.GetCode())
	assert.Equal(t, []string{"name"}, resp.GetColumns())
	assert.Equal(t, [][]string{{"sam"}}, rowStrings(resp.GetRows()))
}

func rowStrings(rows []*sqlpb.Row) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		out[i] = row.GetValues()
	}
	return out
}

func TestParseQuery_InvalidStatements(t *testing.T) {
	client := newTestClient(t)

	tests := []struct {
		sql           string
		errorContains string
	}{
		{"SELECT id", "expected FROM, got EOF"},
		{"UPDATE users SET age = 1, age = 2", "duplicate assignment for column age"},
		{"DELETE users", "FROM keyword must be expected after DELETE, got users"},
		{"MERGE users SET x", "command not implemented, got MERGE"},
	}

	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			resp, err := client.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: tt.sql})

			require.NoError(t, err, "a parse failure must not surface as a gRPC-level error")
			assert.Equal(t, sqlpb.StatusCode_PARSE_ERROR, resp.GetCode())
			assert.Equal(t, tt.errorContains, resp.GetErrorMessage())
		})
	}
}

// Every request goes through one shared parser, so concurrent clients must not
// be able to disturb one another's statements: before the parser kept its state
// per call, this crashed the server or ran garbled statements.
func TestParseQuery_ConcurrentClients(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()
	for _, sql := range []string{"CREATE DATABASE shop"} {
		resp, err := client.ParseQuery(ctx, &sqlpb.QueryRequest{Sql: sql})
		require.NoError(t, err)
		require.Equal(t, sqlpb.StatusCode_OK, resp.Code, resp.ErrorMessage)
	}
	resp, err := client.ParseQuery(ctx, &sqlpb.QueryRequest{Sql: "CREATE TABLE items (id INT PRIMARY KEY, label VARCHAR(30))", Database: "shop"})
	require.NoError(t, err)
	require.Equal(t, sqlpb.StatusCode_OK, resp.Code, resp.ErrorMessage)

	const clients, perClient = 8, 150
	var wg sync.WaitGroup
	errs := make(chan string, clients)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < perClient; i++ {
				id := c*1000 + i
				label := fmt.Sprintf("c%d-%d", c, i)
				ins, err := client.ParseQuery(ctx, &sqlpb.QueryRequest{Sql: fmt.Sprintf("INSERT INTO items (id, label) VALUES (%d, '%s')", id, label), Database: "shop"})
				if err != nil || ins.Code != sqlpb.StatusCode_OK {
					errs <- fmt.Sprintf("insert %d: %v %v", id, err, ins)
					return
				}
				sel, err := client.ParseQuery(ctx, &sqlpb.QueryRequest{Sql: fmt.Sprintf("SELECT label FROM items WHERE id = %d", id), Database: "shop"})
				if err != nil || sel.Code != sqlpb.StatusCode_OK || len(sel.Rows) != 1 || sel.Rows[0].Values[0] != label {
					errs <- fmt.Sprintf("select %d: %v %v", id, err, sel)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	count, err := client.ParseQuery(ctx, &sqlpb.QueryRequest{Sql: "SELECT id FROM items", Database: "shop"})
	require.NoError(t, err)
	assert.Len(t, count.Rows, clients*perClient)
}
