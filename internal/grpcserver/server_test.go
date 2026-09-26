package grpcserver

import (
	"context"
	"net"
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
	sqlEngine := engine.NewEngine(engine.NewCatalog())

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
		{
			name:          "statement not yet supported",
			sqls:          []string{"SELECT * FROM users"},
			errorContains: "statement not supported, got parser.SelectStatement",
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
