//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqlite

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	_ "modernc.org/sqlite" // Register relevant drivers.
)

const (
	// sqliteOptionPrefix is the string prefix sqlite uses to set various
	// options. This is used in the following format:
	//   * sqliteOptionPrefix || option_name = option_value.
	sqliteOptionPrefix = "_pragma"

	// sqliteTxLockImmediate is a dsn option used to ensure that write
	// transactions are started immediately.
	sqliteTxLockImmediate = "_txlock=immediate"
)

// pragmaOption holds a key-value pair for a SQLite pragma setting.
type pragmaOption struct {
	name  string
	value string
}

// NewSqliteBackend returns a db object initialized with the passed backend
// config. If a sqlite connection cannot be established, then an error is
// returned.
func NewSqliteBackend(ctx context.Context, cfg *Config, dbPath, fileName,
	prefix string) (walletdb.DB, error) {

	// First, we add a set of mandatory pragma options to the query.
	pragmaOptions := []pragmaOption{
		{
			name: "busy_timeout",
			value: fmt.Sprintf(
				"%d", cfg.BusyTimeout.Milliseconds(),
			),
		},
		{
			name:  "foreign_keys",
			value: "on",
		},
		{
			name:  "journal_mode",
			value: "WAL",
		},
		{
			name:  "auto_vacuum",
			value: "incremental",
		},
	}

	sqliteOptions := make(url.Values)
	for _, option := range pragmaOptions {
		sqliteOptions.Add(
			sqliteOptionPrefix,
			fmt.Sprintf("%v=%v", option.name, option.value),
		)
	}

	// Then we add any user specified pragma options. Note that these can
	// be of the form: "key=value", "key(N)" or "key".
	for _, option := range cfg.PragmaOptions {
		sqliteOptions.Add(sqliteOptionPrefix, option)
	}

	// Construct the DSN which is just the database file name, appended
	// with the series of pragma options as a query URL string. For more
	// details on the formatting here, see the modernc.org/sqlite docs:
	// https://pkg.go.dev/modernc.org/sqlite#Driver.Open.
	dsn := fmt.Sprintf(
		"%v?%v&%v", filepath.Join(dbPath, fileName),
		sqliteOptions.Encode(), sqliteTxLockImmediate,
	)
	sqlCfg := &sqlbase.Config{
		DriverName:      "sqlite",
		Dsn:             dsn,
		Timeout:         cfg.Timeout,
		TableNamePrefix: prefix,
	}

	return sqlbase.NewSqlBackend(ctx, sqlCfg)
}
