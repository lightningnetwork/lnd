//go:build test_db_postgres

package reputation

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestDB creates a fresh Postgres database with all migrations applied.
func newTestDB(t *testing.T) *sqldb.BaseDB {
	fixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	return sqldb.NewTestPostgresDB(t, fixture).BaseDB
}
