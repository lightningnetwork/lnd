//go:build !test_db_postgres
// +build !test_db_postgres

package sqldb

import (
	"testing"
)

// NewTestDB is a helper function that creates an SQLite database for testing.
func NewTestDB(t *testing.T) *SqliteStore {
	return NewTestSqliteDB(t)
}

// NewTestDBWithVersion is a helper function that creates an SQLite database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, version uint) *SqliteStore {
	return NewTestSqliteDBWithVersion(t, version)
}
