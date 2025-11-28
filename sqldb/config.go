package sqldb

import (
	"fmt"
	"net/url"
	"time"
)

const (
	// defaultMaxConns is the number of permitted active and idle
	// connections. We want to limit this so it isn't unlimited. We use the
	// same value for the number of idle connections as, this can speed up
	// queries given a new connection doesn't need to be established each
	// time.
	defaultMaxConns = 25

	// connIdleLifetime is the amount of time a connection can be idle.
	connIdleLifetime = 5 * time.Minute
)

// SqliteConfig holds all the config arguments needed to interact with our
// sqlite DB.
//
//nolint:ll
type SqliteConfig struct {
	Timeout        time.Duration `long:"timeout" description:"The time after which a database query should be timed out."`
	BusyTimeout    time.Duration `long:"busytimeout" description:"The maximum amount of time to wait for a database connection to become available for a query."`
	MaxConnections int           `long:"maxconnections" description:"The maximum number of open connections to the database. Set to zero for unlimited."`
	PragmaOptions  []string      `long:"pragmaoptions" description:"A list of pragma options to set on a database connection. For example, 'auto_vacuum=incremental'. Note that the flag must be specified multiple times if multiple options are to be set."`
	SkipMigrations bool          `long:"skipmigrations" description:"Skip applying migrations on startup."`
	QueryConfig    `group:"query" namespace:"query"`
}

// Validate checks that the SqliteConfig values are valid.
func (p *SqliteConfig) Validate() error {
	if err := p.QueryConfig.Validate(true); err != nil {
		return fmt.Errorf("invalid query config: %w", err)
	}

	return nil
}

// PostgresConfig holds the postgres database configuration.
//
//nolint:ll
type PostgresConfig struct {
	Dsn                     string        `long:"dsn" description:"Database connection string."`
	Timeout                 time.Duration `long:"timeout" description:"Database connection timeout. Set to zero to disable."`
	MaxConnections          int           `long:"maxconnections" description:"The maximum number of open connections to the database. Set to zero for unlimited."`
	SkipMigrations          bool          `long:"skipmigrations" description:"Skip applying migrations on startup."`
	ChannelDBWithGlobalLock bool          `long:"channeldb-with-global-lock" description:"Use a global lock for channeldb access. This ensures only a single writer at a time but reduces concurrency. This is a temporary workaround until the revocation log is migrated to a native sql schema."`
	WalletDBWithGlobalLock  bool          `long:"walletdb-with-global-lock" description:"Use a global lock for wallet database access. This ensures only a single writer at a time but reduces concurrency. This is a temporary workaround until the wallet subsystem is upgraded to a native sql schema."`
	QueryConfig             `group:"query" namespace:"query"`
}

// Validate checks that the PostgresConfig values are valid.
func (p *PostgresConfig) Validate() error {
	if p.Dsn == "" {
		return fmt.Errorf("DSN is required")
	}

	// Parse the DSN as a URL.
	_, err := url.Parse(p.Dsn)
	if err != nil {
		return fmt.Errorf("invalid DSN: %w", err)
	}

	if err := p.QueryConfig.Validate(false); err != nil {
		return fmt.Errorf("invalid query config: %w", err)
	}

	return nil
}
