//go:build !test_db_postgres && test_db_sqlite

package paymentsdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates a SQLStore backed by a SQL
// database for testing.
func NewTestDB(t testing.TB, opts ...OptionModifier) DB {
	return NewTestDBWithFixture(t, nil, opts...)
}

// NewTestDBFixture is a no-op for the sqlite build.
func NewTestDBFixture(_ *testing.T) *sqldb.TestPgFixture {
	return nil
}

// NewTestDBWithFixture is a helper function that creates a SQLStore backed by a
// SQL database for testing.
func NewTestDBWithFixture(t testing.TB, _ *sqldb.TestPgFixture,
	opts ...OptionModifier) DB {

	store, err := NewSQLStore(
		&SQLStoreConfig{
			QueryCfg: sqldb.DefaultSQLiteConfig(),
		}, newBatchQuerier(t), opts...,
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

// NewKVTestDB is a helper function that creates an BBolt database for testing
// and there is no need to convert the interface to the KVStore because for
// some unit tests we still need access to the kvdb interface.
func NewKVTestDB(t *testing.T, opts ...OptionModifier) *KVStore {
	backend, backendCleanup, err := kvdb.GetTestBackend(
		t.TempDir(), "kvPaymentDB",
	)
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	paymentDB, err := NewKVStore(backend, opts...)
	require.NoError(t, err)

	return paymentDB
}
