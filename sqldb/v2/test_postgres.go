//go:build test_db_postgres

package sqldb

import (
	"testing"
)

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t *testing.T, streams []MigrationStream) *PostgresStore {
	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)

	return NewTestPostgresDB(t, pgFixture, streams)
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, version uint,
	stream MigrationStream) *PostgresStore {

	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)

	return NewTestPostgresDBWithVersion(t, pgFixture, stream, version)
}
