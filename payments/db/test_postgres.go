//go:build test_db_postgres && !test_db_sqlite

package paymentsdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/sqldb"
)

// NewTestDB creates a new test database for Postgres.
func NewTestDB(t *testing.T, opts ...OptionModifier) PaymentDB {
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

	store := NewSQLStore(
		executor,
		clock.NewDefaultClock(),
		opts...,
	)

	return store
}
