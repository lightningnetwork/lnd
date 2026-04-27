//go:build !test_db_postgres && test_db_sqlite

package chainparams

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestDB creates a SQLite-backed BaseDB for use in unit tests.
func newTestDB(t testing.TB) *sqldb.BaseDB {
	return sqldb.NewTestSqliteDB(t).GetBaseDB()
}
