//go:build test_db_sqlite && !test_db_postgres

package offers

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestFixture returns nil, because SQLite needs no shared server.
func newTestFixture(_ *testing.T) *sqldb.TestPgFixture {
	return nil
}

// newTestBaseDB creates an isolated SQLite database. It ignores fixture.
func newTestBaseDB(t *testing.T, _ *sqldb.TestPgFixture) *sqldb.BaseDB {
	return sqldb.NewTestSqliteDB(t).BaseDB
}
