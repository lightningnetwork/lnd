//go:build test_db_postgres && !test_db_sqlite

package graphdb

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates a SQLStore backed by a SQL
// database for testing.
func NewTestDB(t testing.TB) V1Store {
	return NewTestDBWithFixture(t, nil)
}

// NewTestDBFixture creates a new sqldb.TestPgFixture for testing purposes.
func NewTestDBFixture(t *testing.T) *sqldb.TestPgFixture {
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})
	return pgFixture
}

// NewTestDBWithFixture is a helper function that creates a SQLStore backed by a
// SQL database for testing.
func NewTestDBWithFixture(t testing.TB,
	pgFixture *sqldb.TestPgFixture) V1Store {

	var querier BatchedSQLQueries
	if pgFixture == nil {
		querier = newBatchQuerier(t)
	} else {
		querier = newBatchQuerierWithFixture(t, pgFixture)
	}

	store, err := NewSQLStore(
		&SQLStoreConfig{
			ChainHash: *chaincfg.MainNetParams.GenesisHash,
			QueryCfg:  sqldb.DefaultPostgresConfig(),
		}, querier,
	)
	require.NoError(t, err)

	return store
}

// newBatchQuerier creates a new BatchedSQLQueries instance for testing
// using a PostgreSQL database fixture.
func newBatchQuerier(t testing.TB) BatchedSQLQueries {
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return newBatchQuerierWithFixture(t, pgFixture)
}

// newBatchQuerierWithFixture creates a new BatchedSQLQueries instance for
// testing using a PostgreSQL database fixture.
func newBatchQuerierWithFixture(t testing.TB,
	pgFixture *sqldb.TestPgFixture) BatchedSQLQueries {

	db := sqldb.NewTestPostgresDB(t, pgFixture).BaseDB

	return sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)
}
