//go:build test_db_postgres && !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)) && !(netbsd || openbsd)

package sqldb

import (
	"testing"
)

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t *testing.T, sets []MigrationSet) *PostgresStore {
	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return NewTestPostgresDB(t, pgFixture, sets)
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, set MigrationSet,
	version uint) *PostgresStore {

	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return NewTestPostgresDBWithVersion(t, pgFixture, set, version)
}
