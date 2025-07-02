//go:build test_db_postgres || test_db_sqlite

package graphdb

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var testChain = *chaincfg.MainNetParams.GenesisHash

// TestMigrateGraphToSQL tests various deterministic cases that we want to test
// for to ensure that our migration from a graph store backed by a KV DB to a
// SQL database works as expected. At the end of each test, the DBs are compared
// and expected to have the exact same data in them.
func TestMigrateGraphToSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name          string
		write         func(t *testing.T, db *KVStore, object any)
		objects       []any
		expGraphStats graphStats
	}{
		{
			name: "empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Set up our source kvdb DB.
			kvDB := setUpKVStore(t)

			// Write the test objects to the kvdb store.
			for _, object := range test.objects {
				test.write(t, kvDB, object)
			}

			// Set up our destination SQL DB.
			sql, ok := NewTestDB(t).(*SQLStore)
			require.True(t, ok)

			// Run the migration.
			err := MigrateGraphToSQL(
				ctx, kvDB.db, sql.db, testChain,
			)
			require.NoError(t, err)

			// Validate that the two databases are now in sync.
			assertInSync(t, kvDB, sql, test.expGraphStats)
		})
	}
}

// graphStats holds expected statistics about the graph after migration.
type graphStats struct {
}

// assertInSync checks that the KVStore and SQLStore both contain the same
// graph data after migration.
func assertInSync(_ *testing.T, _ *KVStore, _ *SQLStore, stats graphStats) {
}

// setUpKVStore initializes a new KVStore for testing.
func setUpKVStore(t *testing.T) *KVStore {
	kvDB, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "graph")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	kvStore, err := NewKVStore(kvDB)
	require.NoError(t, err)

	return kvStore
}
