//go:build test_db_postgres
// +build test_db_postgres

package sqldb

import (
	"testing"
)

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t *testing.T) *PostgresStore {
	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return NewTestPostgresDB(t, pgFixture)
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, version uint) *PostgresStore {
	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return NewTestPostgresDBWithVersion(t, pgFixture, version)
}
