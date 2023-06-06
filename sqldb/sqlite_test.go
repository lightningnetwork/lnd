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
