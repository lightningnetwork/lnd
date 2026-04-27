//go:build test_db_postgres && !test_db_sqlite

package chainparams

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb"
)

// newTestDB creates a Postgres-backed BaseDB for use in unit tests.
func newTestDB(t testing.TB) *sqldb.BaseDB {
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return sqldb.NewTestPostgresDB(t, pgFixture).GetBaseDB()
}
