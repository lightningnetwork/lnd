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

	// defaultMaxIdleConns is the number of permitted idle connections.
	defaultMaxIdleConns = 6

	// defaultConnMaxIdleTime is the amount of time a connection can be
	// idle before it is closed.
	defaultConnMaxIdleTime = 5 * time.Minute

	// defaultConnMaxLifetime is the maximum amount of time a connection can
	// be reused for before it is closed.
	defaultConnMaxLifetime = 10 * time.Minute
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

	// SkipMigrationDbBackup if true, then a backup of the database will not
	// be created before applying migrations.
	SkipMigrationDbBackup bool `long:"skipmigrationdbbackup" description:"Skip creating a backup of the database before applying migrations."`
}

// PostgresConfig holds the postgres database configuration.
//
//nolint:ll
type PostgresConfig struct {
	Dsn                string        `long:"dsn" description:"Database connection string."`
	Timeout            time.Duration `long:"timeout" description:"Database connection timeout. Set to zero to disable."`
	MaxOpenConnections int           `long:"maxconnections" description:"Max open connections to keep alive to the database server. Set to zero for unlimited."`
	MaxIdleConnections int           `long:"maxidleconnections" description:"Max number of idle connections to keep in the connection pool. Set to zero for unlimited."`
	ConnMaxLifetime    time.Duration `long:"connmaxlifetime" description:"Max amount of time a connection can be reused for before it is closed. Valid time units are {s, m, h}."`
	ConnMaxIdleTime    time.Duration `long:"connmaxidletime" description:"Max amount of time a connection can be idle for before it is closed. Valid time units are {s, m, h}."`
	RequireSSL         bool          `long:"requiressl" description:"Whether to require using SSL (mode: require) when connecting to the server."`
	SkipMigrations     bool          `long:"skipmigrations" description:"Skip applying migrations on startup."`
}

func (p *PostgresConfig) Validate() error {
	if p.Dsn == "" {
		return fmt.Errorf("DSN is required")
	}

	// Parse the DSN as a URL.
	_, err := url.Parse(p.Dsn)
	if err != nil {
		return fmt.Errorf("invalid DSN: %w", err)
	}

	return nil
}
