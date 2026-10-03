package wiring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
	"manhhung2111/go-sql/internal/grpcserver"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/proto/sqlpb"
)

func TestInitializeServer(t *testing.T) {
	t.Run("builds a server over the configured data directory", func(t *testing.T) {
		cfg := &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}}

		server, cleanup, err := InitializeServer(cfg)

		require.NoError(t, err)
		t.Cleanup(cleanup)
		assert.NotNil(t, server)
	})

	t.Run("fails when no data directory is configured", func(t *testing.T) {
		_, _, err := InitializeServer(&config.Config{})

		assert.ErrorContains(t, err, "data directory is required")
	})
}

func TestInitializeServer_DataSurvivesARestart(t *testing.T) {
	cfg := &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}}
	query := func(server *grpcserver.Server, database, sql string) *sqlpb.QueryResponse {
		resp, err := server.ParseQuery(context.Background(), &sqlpb.QueryRequest{Sql: sql, Database: database})
		require.NoError(t, err)
		require.Equal(t, sqlpb.StatusCode_OK, resp.Code, resp.ErrorMessage)
		return resp
	}

	server, cleanup, err := InitializeServer(cfg)
	require.NoError(t, err)
	query(server, "", "CREATE DATABASE shop")
	query(server, "shop", "CREATE TABLE users (id INT PRIMARY KEY)")
	query(server, "shop", "INSERT INTO users (id) VALUES (1)")
	cleanup()

	server, cleanup, err = InitializeServer(cfg)
	require.NoError(t, err)
	defer cleanup()

	resp := query(server, "shop", "SELECT id FROM users")
	require.Len(t, resp.Rows, 1)
	assert.Equal(t, []string{"1"}, resp.Rows[0].Values)
}

func TestProvideCatalog_CleanupClosesTheCatalog(t *testing.T) {
	catalog, cleanup, err := ProvideCatalog(engine.DataDir(t.TempDir()))
	require.NoError(t, err)
	require.NoError(t, catalog.CreateDatabase("shop"))
	db, err := catalog.GetDatabase("shop")
	require.NoError(t, err)
	require.NoError(t, db.CreateTable("users", []parser.ColumnDefinition{{Name: "id", DataType: parser.IntDataType{}}}, false))
	table, _ := db.GetTable("users")

	cleanup()

	_, err = table.Select([]string{"*"}, nil)
	assert.ErrorContains(t, err, "is closed")
}
