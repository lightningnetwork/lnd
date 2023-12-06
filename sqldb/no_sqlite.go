//go:build js || (windows && (arm || 386)) || (linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import "fmt"

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
