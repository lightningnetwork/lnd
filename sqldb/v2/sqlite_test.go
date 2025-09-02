//go:build !test_db_postgres

package sqldb

import (
	"testing"
)

// NewTestDB is a helper function that creates an SQLite database for testing.
func NewTestDB(t *testing.T, sets []MigrationSet) *SqliteStore {
	return NewTestSqliteDB(t, sets)
}

// NewTestDBWithVersion is a helper function that creates an SQLite database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, set MigrationSet,
	version uint) *SqliteStore {

	return NewTestSqliteDBWithVersion(t, set, version)
}
