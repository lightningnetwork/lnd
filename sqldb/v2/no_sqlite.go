//go:build js || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"context"
	"fmt"
)

var (
	// Make sure SqliteStore implements the DB interface.
	_ DB = (*SqliteStore)(nil)
)

// SqliteStore is a database store implementation that uses a sqlite backend.
type SqliteStore struct {
	cfg *SqliteConfig

	*BaseDB
}

// NewSqliteStore attempts to open a new sqlite database based on the passed
// config.
func NewSqliteStore(cfg *SqliteConfig, dbPath string) (*SqliteStore, error) {
	return nil, fmt.Errorf("SQLite backend not supported in WebAssembly")
}

// GetBaseDB returns the underlying BaseDB instance for the SQLite store.
// It is a trivial helper method to comply with the sqldb.DB interface.
func (s *SqliteStore) GetBaseDB() *BaseDB {
	return s.BaseDB
}

// ApplyAllMigrations applies both the SQLC and custom in-code migrations to
// the SQLite database.
func (s *SqliteStore) ApplyAllMigrations(context.Context,
	[]MigrationConfig) error {

	return fmt.Errorf("SQLite backend not supported in WebAssembly")
}
