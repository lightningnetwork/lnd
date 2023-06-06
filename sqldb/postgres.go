package sqldb

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Read migrations from files. // nolint:lll
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

const (
	dsnTemplate = "postgres://%v:%v@%v:%d/%v?sslmode=%v"
)

var (
	// DefaultPostgresFixtureLifetime is the default maximum time a Postgres
	// test fixture is being kept alive. After that time the docker
	// container will be terminated forcefully, even if the tests aren't
	// fully executed yet. So this time needs to be chosen correctly to be
	// longer than the longest expected individual test run time.
	DefaultPostgresFixtureLifetime = 10 * time.Minute
)

// PostgresConfig holds the postgres database configuration.
//
//nolint:lll
type PostgresConfig struct {
	SkipMigrations     bool   `long:"skipmigrations" description:"Skip applying migrations on startup."`
	Host               string `long:"host" description:"Database server hostname."`
	Port               int    `long:"port" description:"Database server port."`
	User               string `long:"user" description:"Database user."`
	Password           string `long:"password" description:"Database user's password."`
	DBName             string `long:"dbname" description:"Database name to use."`
	MaxOpenConnections int    `long:"maxconnections" description:"Max open connections to keep alive to the database server."`
	RequireSSL         bool   `long:"requiressl" description:"Whether to require using SSL (mode: require) when connecting to the server."`
}

// DSN returns the dns to connect to the database.
func (s *PostgresConfig) DSN(hidePassword bool) string {
	var sslMode = "disable"
	if s.RequireSSL {
		sslMode = "require"
	}

	password := s.Password
	if hidePassword {
		// Placeholder used for logging the DSN safely.
		password = "****"
	}

	return fmt.Sprintf(dsnTemplate, s.User, password, s.Host, s.Port,
		s.DBName, sslMode)
}

// PostgresStore is a database store implementation that uses a Postgres
// backend.
type PostgresStore struct {
	cfg *PostgresConfig

	*BaseDB
}

// NewPostgresStore creates a new store that is backed by a Postgres database
// backend.
func NewPostgresStore(cfg *PostgresConfig) (*PostgresStore, error) {
	log.Infof("Using SQL database '%s'", cfg.DSN(true))

	rawDB, err := sql.Open("pgx", cfg.DSN(false))
	if err != nil {
		return nil, err
	}

	maxConns := defaultMaxConns
	if cfg.MaxOpenConnections > 0 {
		maxConns = cfg.MaxOpenConnections
	}

	rawDB.SetMaxOpenConns(maxConns)
	rawDB.SetMaxIdleConns(maxConns)
	rawDB.SetConnMaxLifetime(connIdleLifetime)

	if !cfg.SkipMigrations {
		// Now that the database is open, populate the database with
		// our set of schemas based on our embedded in-memory file
		// system.
		//
		// First, we'll need to open up a new migration instance for
		// our current target database: sqlite.
		driver, err := postgres_migrate.WithInstance(
			rawDB, &postgres_migrate.Config{},
		)
		if err != nil {
			return nil, err
		}

		postgresFS := newReplacerFS(sqlSchemas, map[string]string{
			"BLOB":                "BYTEA",
			"INTEGER PRIMARY KEY": "SERIAL PRIMARY KEY",
			"TIMESTAMP":           "TIMESTAMP WITHOUT TIME ZONE",
		})

		err = applyMigrations(
			postgresFS, driver, "sqlc/migrations", cfg.DBName,
		)
		if err != nil {
			return nil, err
		}
	}

	queries := sqlc.New(rawDB)

	return &PostgresStore{
		cfg: cfg,
		BaseDB: &BaseDB{
			DB:      rawDB,
			Queries: queries,
		},
	}, nil
}

// NewTestPostgresDB is a helper function that creates a Postgres database for
// testing.
func NewTestPostgresDB(t *testing.T) *PostgresStore {
	t.Helper()

	t.Logf("Creating new Postgres DB for testing")

	sqlFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	store, err := NewPostgresStore(sqlFixture.GetConfig())
	require.NoError(t, err)

	t.Cleanup(func() {
		sqlFixture.TearDown(t)
	})

	return store
}
