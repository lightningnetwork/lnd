//go:build test_db_postgres && !test_db_sqlite

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
	pgFixture *sqldb.TestPgFixture, opts ...OptionModifier) DB {

	var querier BatchedSQLQueries
	if pgFixture == nil {
		querier = newBatchQuerier(t)
	} else {
		querier = newBatchQuerierWithFixture(t, pgFixture)
	}

	store, err := NewSQLStore(
		&SQLStoreConfig{
			QueryCfg: sqldb.DefaultPostgresConfig(),
		}, querier, opts...,
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

	rawDB := sqldb.NewTestPostgresDB(t, pgFixture).BaseDB.DB

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
