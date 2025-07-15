//go:build !test_db_postgres && test_db_sqlite

package paymentsdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/sqldb"
)

// NewTestDB creates a new test database for SQLite.
func NewTestDB(t *testing.T, opts ...OptionModifier) PaymentDB {
	db := sqldb.NewTestSqliteDB(t)

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
