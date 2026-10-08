//go:build test_db_postgres

package offers

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestFixture starts one Postgres container for a test and its subtests.
// The container stops when the test ends.
func newTestFixture(t *testing.T) *sqldb.TestPgFixture {
	fixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	return fixture
}

// newTestBaseDB creates an isolated Postgres database in the container of
// fixture.
func newTestBaseDB(t *testing.T, fixture *sqldb.TestPgFixture) *sqldb.BaseDB {
	return sqldb.NewTestPostgresDB(t, fixture).BaseDB
}
