//go:build test_db_postgres && !test_db_sqlite

package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newIntervalTestDB creates a Postgres backed database for the interval store
// tests.
func newIntervalTestDB(t testing.TB) *sqldb.BaseDB {
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return sqldb.NewTestPostgresDB(t, pgFixture).GetBaseDB()
}
