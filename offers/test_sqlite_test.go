//go:build !test_db_postgres

package offers

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestBaseDB creates a SQLite database for the offer store tests.
func newTestBaseDB(t *testing.T) *sqldb.BaseDB {
	return sqldb.NewTestSqliteDB(t).BaseDB
}
