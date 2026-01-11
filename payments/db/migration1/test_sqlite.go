//go:build !test_db_postgres && test_db_sqlite

package migration1

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/payments/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates a SQLStore backed by a SQL
// database for testing.
func NewTestDB(t testing.TB, opts ...OptionModifier) (DB, TestHarness) {
	db := NewTestDBWithFixture(t, nil, opts...)
	return db, &noopTestHarness{}
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

	rawDB := sqldb.NewTestSqliteDB(t).BaseDB.DB

	return &testBatchedSQLQueries{
		db:      rawDB,
		Queries: sqlc.New(rawDB),
	}
}

// noopTestHarness is the SQL test harness implementation. Since SQL doesn't
// use a separate payment index bucket like KV, these assertions are no-ops.
type noopTestHarness struct{}

// AssertPaymentIndex is a no-op for SQL implementations.
func (h *noopTestHarness) AssertPaymentIndex(t *testing.T,
	expectedHash lntypes.Hash) {

	// No-op: SQL doesn't use a separate index bucket.
}

// AssertNoIndex is a no-op for SQL implementations.
func (h *noopTestHarness) AssertNoIndex(t *testing.T, seqNr uint64) {
	// No-op: SQL doesn't use a separate index bucket.
}
