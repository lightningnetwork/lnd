//go:build !test_db_postgres

package sqldb

import (
	"testing"
)

// isSQLite is true if the build tag is set to test_db_sqlite. It is used in
// tests that compile for both SQLite and Postgres databases to determine
// which database implementation is being used.
//
// TODO(elle): once we've updated to using sqldbv2, we can remove this since
// then we will have access to the DatabaseType on the BaseDB struct at runtime.
const isSQLite = true

// NewTestDB is a helper function that creates an SQLite database for testing.
func NewTestDB(t *testing.T, streams []MigrationStream) *SqliteStore {
	return NewTestSqliteDB(t, streams)
}

// NewTestDBWithVersion is a helper function that creates an SQLite database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, stream MigrationStream,
	version uint) *SqliteStore {

	return NewTestSqliteDBWithVersion(t, stream, version)
}
