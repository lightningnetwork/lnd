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

// newSQLBaseConfig builds the shared sqlbase config used by both the regular
// and migration Postgres backends from the passed backend config and prefix.
func newSQLBaseConfig(config *Config, prefix string) *sqlbase.Config {
	return &sqlbase.Config{
		DriverName:            "pgx",
		Dsn:                   config.Dsn,
		Timeout:               config.Timeout,
		Schema:                "public",
		TableNamePrefix:       prefix,
		SQLiteCmdReplacements: sqliteCmdReplacements,
		WithTxLevelLock:       config.WithGlobalLock,
	}
}

// newPostgresBackend returns a db object initialized with the passed backend
// config. If postgres connection cannot be established, then returns error.
func newPostgresBackend(ctx context.Context, config *Config, prefix string) (
	walletdb.DB, error) {

	return sqlbase.NewSqlBackend(ctx, newSQLBaseConfig(config, prefix))
}

// NewMigrationBackend returns a Postgres backend that explicitly exposes the
// migration-only bulk KV interface.
func NewMigrationBackend(ctx context.Context, config *Config, prefix string) (
	sqlbase.MigrationBackend, error) {

	return sqlbase.NewPostgresBackend(
		ctx, newSQLBaseConfig(config, prefix),
	)
}
