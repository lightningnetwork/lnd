//go:build !test_db_postgres && test_db_sqlite

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

// NewTestDBFixture is a no-op for the sqlite build.
func NewTestDBFixture(_ *testing.T) *sqldb.TestPgFixture {
	return nil
}

// NewTestDBWithFixture is a helper function that creates a SQLStore backed by a
// SQL database for testing.
func NewTestDBWithFixture(t testing.TB, _ *sqldb.TestPgFixture) V1Store {
	store, err := NewSQLStore(
		&SQLStoreConfig{
			ChainHash: *chaincfg.MainNetParams.GenesisHash,
			QueryCfg:  sqldb.DefaultSQLiteConfig(),
		}, newBatchQuerier(t),
	)
	require.NoError(t, err)
	return store
}

// newBatchQuerier creates a new BatchedSQLQueries instance for testing
// using a SQLite database.
func newBatchQuerier(t testing.TB) BatchedSQLQueries {
	return newBatchQuerierWithFixture(t, nil)
}

// newBatchQuerierWithFixture creates a new BatchedSQLQueries instance for
// testing using a SQLite database.
func newBatchQuerierWithFixture(t testing.TB,
	_ *sqldb.TestPgFixture) BatchedSQLQueries {

	db := sqldb.NewTestSqliteDB(t).BaseDB

	return sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)
}
