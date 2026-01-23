//go:build !test_db_postgres

package sqldb

import (
	"testing"
)

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
