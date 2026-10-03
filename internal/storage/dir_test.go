package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncDir(t *testing.T) {
	t.Run("an existing directory syncs", func(t *testing.T) {
		require.NoError(t, SyncDir(t.TempDir()))
	})

	t.Run("a missing directory is an error", func(t *testing.T) {
		err := SyncDir(filepath.Join(t.TempDir(), "missing"))

		assert.ErrorContains(t, err, "opening directory")
	})
}
