package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
	return path
}

func TestLoad(t *testing.T) {
	t.Run("reads the storage data directory", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "storage:\n  data_dir: /var/lib/go-sql\n"))

		require.NoError(t, err)
		assert.Equal(t, "/var/lib/go-sql", cfg.Storage.DataDir)
	})

	t.Run("defaults the data directory when the storage section is missing", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "server:\n  port: 1\n"))

		require.NoError(t, err)
		assert.Equal(t, DefaultDataDir, cfg.Storage.DataDir)
	})

	t.Run("defaults the data directory when it is blank", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "storage:\n  data_dir: \"\"\n"))

		require.NoError(t, err)
		assert.Equal(t, DefaultDataDir, cfg.Storage.DataDir)
	})

	t.Run("keeps the server settings", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, "server:\n  host: 127.0.0.1\n  port: 50051\n"))

		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:50051", cfg.Server.Addr())
	})

	t.Run("a missing file is an error", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "nope.yml"))

		assert.ErrorContains(t, err, "read config file")
	})

	t.Run("invalid yaml is an error", func(t *testing.T) {
		_, err := Load(writeConfig(t, "server: [unclosed"))

		assert.ErrorContains(t, err, "parse config file")
	})
}

// The config file the server ships with must itself load and carry the
// settings the server needs.
func TestLoad_ShippedConfig(t *testing.T) {
	cfg, err := Load("config.yml")

	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:50051", cfg.Server.Addr())
	assert.Equal(t, "./data", cfg.Storage.DataDir)
}
