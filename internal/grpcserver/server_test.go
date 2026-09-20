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

	"manhhung2111/go-sql/proto/sqlpb"
)

func newTestClient(t *testing.T) sqlpb.SqlParserServiceClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { lis.Close() })

	grpcServer := grpc.NewServer()
	sqlpb.RegisterSqlParserServiceServer(grpcServer, New())
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
		"SELECT * FROM users",
		"SELECT * FROM users WHERE age > 18",
		"INSERT INTO users (id, name) VALUES (1, 'bob')",
		"UPDATE users SET age = 30 WHERE id = 1",
		"DELETE FROM users WHERE id = 1",
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
