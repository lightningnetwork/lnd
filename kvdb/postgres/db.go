//go:build kvdb_postgres

package postgres

import (
	"context"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
)

// sqliteCmdReplacements defines a mapping from some SQLite keywords and phrases
// to their postgres counterparts.
var sqliteCmdReplacements = sqlbase.SQLiteCmdReplacements{
	"BLOB":                "BYTEA",
	"INTEGER PRIMARY KEY": "BIGSERIAL PRIMARY KEY",
}

// newPostgresBackend returns a db object initialized with the passed backend
// config. If postgres connection cannot be established, then returns error.
func newPostgresBackend(ctx context.Context, config *Config, prefix string) (
	walletdb.DB, error) {

	cfg := &sqlbase.Config{
		DriverName:            "pgx",
		Dsn:                   config.Dsn,
		Timeout:               config.Timeout,
		Schema:                "public",
		TableNamePrefix:       prefix,
		SQLiteCmdReplacements: sqliteCmdReplacements,
		WithTxLevelLock:       config.WithGlobalLock,
	}

	return sqlbase.NewSqlBackend(ctx, cfg)
}
