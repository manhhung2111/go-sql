package wiring

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"manhhung2111/go-sql/internal/config"
)

func TestInitializeServer(t *testing.T) {
	t.Run("builds a server over the configured data directory", func(t *testing.T) {
		cfg := &config.Config{Storage: config.StorageConfig{DataDir: t.TempDir()}}

		server, err := InitializeServer(cfg)

		require.NoError(t, err)
		assert.NotNil(t, server)
	})

	t.Run("fails when no data directory is configured", func(t *testing.T) {
		_, err := InitializeServer(&config.Config{})

		assert.ErrorContains(t, err, "data directory is required")
	})
}
