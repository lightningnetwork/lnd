//go:build test_db_postgres && !test_db_sqlite

package graphdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates a SQLStore backed by a postgres
// database for testing. At the moment, it embeds a KVStore but once the
// SQLStore fully implements the V1Store interface, the KVStore will be removed.
func NewTestDB(t testing.TB) V1Store {
	backend, backendCleanup, err := kvdb.GetTestBackend(t.TempDir(), "cgr")
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	graphStore, err := NewKVStore(backend)
	require.NoError(t, err)

	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	db := sqldb.NewTestPostgresDB(t, pgFixture).BaseDB

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)

	store, err := NewSQLStore(executor, graphStore)
	require.NoError(t, err)

	return store
}
