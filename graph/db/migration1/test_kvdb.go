//go:build !test_db_sqlite && !test_db_postgres

package migration1

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates an BBolt database for testing.
func NewTestDB(t testing.TB) V1Store {
	backend, backendCleanup, err := kvdb.GetTestBackend(t.TempDir(), "cgr")
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	graphStore, err := NewKVStore(backend)
	require.NoError(t, err)

	return graphStore
}
