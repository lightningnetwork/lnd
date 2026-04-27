//go:build js || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"fmt"
)

var (
	// Make sure SqliteStore implements the DB interface.
	_ DB = (*SqliteStore)(nil)
)

// SqliteStore is a database store implementation that uses a sqlite backend.
//
// NOTE: This specific struct implementation does not implement a real sqlite
// store, and only exists to ensure that build tag environments that do not
// support sqlite database backends still contain a struct called SqliteStore,
// to ensure that the build process doesn't error.
type SqliteStore struct {
	cfg *SqliteConfig

	*BaseDB
}

// NewSqliteStore attempts to open a new sqlite database based on the passed
// config.
func NewSqliteStore(cfg *SqliteConfig, dbPath string) (*SqliteStore, error) {
	return nil, fmt.Errorf("SQLite backend not supported on this platform")
}

// GetBaseDB returns the underlying BaseDB instance for the SQLite store.
// It is a trivial helper method to comply with the sqldb.DB interface.
func (s *SqliteStore) GetBaseDB() *BaseDB {
	return s.BaseDB
}

// ExecuteMigrations returns an error because the SQLite backend is unavailable
// on this platform.
func (s *SqliteStore) ExecuteMigrations(MigrationSet) error {

	return fmt.Errorf("SQLite backend not supported on this platform")
}
