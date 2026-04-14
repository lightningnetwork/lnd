//go:build test_db_postgres

package offers

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestBaseDB creates a Postgres database for the offer store tests.
func newTestBaseDB(t *testing.T) *sqldb.BaseDB {
	fixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	return sqldb.NewTestPostgresDB(t, fixture).BaseDB
}
