//go:build !test_db_postgres

package reputation

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestDB creates a fresh SQLite database with all migrations applied.
func newTestDB(t *testing.T) *sqldb.BaseDB {
	return sqldb.NewTestSqliteDB(t).BaseDB
}
