//go:build !test_db_postgres && test_db_sqlite

package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newIntervalTestDB creates a SQLite backed database for the interval store
// tests.
func newIntervalTestDB(t testing.TB) *sqldb.BaseDB {
	return sqldb.NewTestSqliteDB(t).GetBaseDB()
}
