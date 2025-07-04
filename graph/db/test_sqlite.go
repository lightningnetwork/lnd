//go:build !test_db_postgres && test_db_sqlite

package graphdb

import (
	"database/sql"
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newBatchQuerier creates a new BatchedSQLQueries instance for testing
// using a SQLite database.
func newBatchQuerier(t testing.TB) BatchedSQLQueries {
	db := sqldb.NewTestSqliteDB(t).BaseDB

	return sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) SQLQueries {
			return db.WithTx(tx)
		},
	)
}
