package engine

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalog_CreateDatabase(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.CreateDatabase("testdb")

	require.NoError(t, err)
	assert.Equal(t, []string{"testdb"}, catalog.ListDatabases())
}

func TestCatalog_CreateDatabase_AlreadyExists(t *testing.T) {
	catalog := newTestCatalog(t)
	require.NoError(t, catalog.CreateDatabase("testdb"))

	err := catalog.CreateDatabase("testdb")

	assert.EqualError(t, err, `database "testdb" already exists`)
}

func TestCatalog_DropDatabase(t *testing.T) {
	catalog := newTestCatalog(t)
	require.NoError(t, catalog.CreateDatabase("testdb"))

	err := catalog.DropDatabase("testdb")

	require.NoError(t, err)
	assert.Empty(t, catalog.ListDatabases())
}

func TestCatalog_DropDatabase_DoesNotExist(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.DropDatabase("testdb")

	assert.EqualError(t, err, `database "testdb" does not exist`)
}

func TestCatalog_ListDatabases_EmptyCatalog(t *testing.T) {
	catalog := newTestCatalog(t)

	assert.Empty(t, catalog.ListDatabases())
}

func TestCatalog_ListDatabases_SortedOrder(t *testing.T) {
	catalog := newTestCatalog(t)
	require.NoError(t, catalog.CreateDatabase("zebra"))
	require.NoError(t, catalog.CreateDatabase("apple"))
	require.NoError(t, catalog.CreateDatabase("mango"))

	assert.Equal(t, []string{"apple", "mango", "zebra"}, catalog.ListDatabases())
}

func TestCatalog_ConcurrentAccess(t *testing.T) {
	catalog := newTestCatalog(t)
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "db" + string(rune('a'+i%26))
			_ = catalog.CreateDatabase(name)
			catalog.ListDatabases()
			_ = catalog.DropDatabase(name)
		}(i)
	}
	wg.Wait()
}
