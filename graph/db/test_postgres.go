//go:build test_db_postgres && !test_db_sqlite

package graphdb

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates a SQLStore backed by a postgres
// database for testing.
func NewTestDB(t testing.TB) V1Store {
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

	store, err := NewSQLStore(
		&SQLStoreConfig{
			ChainHash: *chaincfg.MainNetParams.GenesisHash,
		}, executor,
	)
	require.NoError(t, err)

	return store
}
