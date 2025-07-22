//go:build test_db_postgres
// +build test_db_postgres

package sqldb

import (
	"testing"
)

// isSQLite is false if the build tag is set to test_db_postgres. It is used in
// tests that compile for both SQLite and Postgres databases to determine
// which database implementation is being used.
//
// TODO(elle): once we've updated to using sqldbv2, we can remove this since
// then we will have access to the DatabaseType on the BaseDB struct at runtime.
const isSQLite = false

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
