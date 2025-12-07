package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	pgx_migrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Read migrations from files. // nolint:ll
	_ "github.com/jackc/pgx/v5"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

var (
	// DefaultPostgresFixtureLifetime is the default maximum time a Postgres
	// test fixture is being kept alive. After that time the docker
	// container will be terminated forcefully, even if the tests aren't
	// fully executed yet. So this time needs to be chosen correctly to be
	// longer than the longest expected individual test run time.
	DefaultPostgresFixtureLifetime = 10 * time.Minute

	// postgresSchemaReplacements is a map of schema strings that need to be
	// replaced for postgres. This is needed because we write the schemas to
	// work with sqlite primarily but in sqlc's own dialect, and postgres
	// has some differences.
	postgresSchemaReplacements = map[string]string{
		"BLOB":                "BYTEA",
		"INTEGER PRIMARY KEY": "BIGSERIAL PRIMARY KEY",
		// We need this space in front of the TIMESTAMP keyword to
		// avoid replacing words which just have the word "TIMESTAMP" in
		// them.
		" TIMESTAMP": " TIMESTAMP WITHOUT TIME ZONE",
	}

	// Make sure PostgresStore implements the MigrationExecutor interface.
	_ MigrationExecutor = (*PostgresStore)(nil)

	// Make sure PostgresStore implements the DB interface.
	_ DB = (*PostgresStore)(nil)
)

// replacePasswordInDSN takes a DSN string and returns it with the password
// replaced by "***".
func replacePasswordInDSN(dsn string) (string, error) {
	// Parse the DSN as a URL
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}

	// Check if the URL has a user info part
	if u.User != nil {
		username := u.User.Username()

		// Reconstruct user info with "***" as password
		userInfo := username + ":***@"

		// Rebuild the DSN with the modified user info
		sanitizeDSN := strings.Replace(
			dsn, u.User.String()+"@", userInfo, 1,
		)

		return sanitizeDSN, nil
	}

	// Return the original DSN if no user info is present
	return dsn, nil
}

// getDatabaseNameFromDSN extracts the database name from a DSN string.
func getDatabaseNameFromDSN(dsn string) (string, error) {
	// Parse the DSN as a URL
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}

	// The database name is the last segment of the path. Trim leading slash
	// and return the last segment.
	return path.Base(u.Path), nil
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
	sanitizedDSN, err := replacePasswordInDSN(cfg.Dsn)
	if err != nil {
		return nil, err
	}
	log.Infof("Using SQL database '%s'", sanitizedDSN)

	db, err := sql.Open("pgx", cfg.Dsn)
	if err != nil {
		return nil, err
	}

	// Ensure the migration tracker table exists before running migrations.
	// This table tracks migration progress and ensures compatibility with
	// SQLC query generation. If the table is already created by an SQLC
	// migration, this operation becomes a no-op.
	migrationTrackerSQL := `
	CREATE TABLE IF NOT EXISTS migration_tracker (
		version INTEGER UNIQUE NOT NULL,
		migration_time TIMESTAMP NOT NULL
	);`

	_, err = db.Exec(migrationTrackerSQL)
	if err != nil {
		return nil, fmt.Errorf("error creating migration tracker: %w",
			err)
	}

	maxConns := defaultMaxConns
	if cfg.MaxConnections > 0 {
		maxConns = cfg.MaxConnections
	}

	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(connIdleLifetime)

	queries := sqlc.New(db)

	return &PostgresStore{
		cfg: cfg,
		BaseDB: &BaseDB{
			DB:      db,
			Queries: queries,
		},
	}, nil
}

// GetBaseDB returns the underlying BaseDB instance for the Postgres store.
// It is a trivial helper method to comply with the sqldb.DB interface.
func (s *PostgresStore) GetBaseDB() *BaseDB {
	return s.BaseDB
}

// ApplyAllMigrations applies both the SQLC and custom in-code migrations to the
// Postgres database.
func (s *PostgresStore) ApplyAllMigrations(ctx context.Context,
	migrations []MigrationConfig) error {

	// Execute migrations unless configured to skip them.
	if s.cfg.SkipMigrations {
		return nil
	}

	return ApplyMigrations(ctx, s.BaseDB, s, migrations)
}

func errPostgresMigration(err error) error {
	return fmt.Errorf("error creating postgres migration: %w", err)
}

// ExecuteMigrations runs migrations for the Postgres database, depending on the
// target given, either all migrations or up to a given version.
func (s *PostgresStore) ExecuteMigrations(target MigrationTarget) error {
	dbName, err := getDatabaseNameFromDSN(s.cfg.Dsn)
	if err != nil {
		return err
	}

	driver, err := pgx_migrate.WithInstance(s.DB, &pgx_migrate.Config{})
	if err != nil {
		return errPostgresMigration(err)
	}

	// Populate the database with our set of schemas based on our embedded
	// in-memory file system.
	postgresFS := newReplacerFS(sqlSchemas, postgresSchemaReplacements)
	return applyMigrations(
		postgresFS, driver, "sqlc/migrations", dbName, target,
	)
}

// GetSchemaVersion returns the current schema version of the Postgres database.
func (s *PostgresStore) GetSchemaVersion() (int, bool, error) {
	driver, err := pgx_migrate.WithInstance(s.DB, &pgx_migrate.Config{})
	if err != nil {
		return 0, false, errPostgresMigration(err)

	}

	version, dirty, err := driver.Version()
	if err != nil {
		return 0, false, err
	}

	return version, dirty, nil
}

// SetSchemaVersion sets the schema version of the Postgres database.
//
// NOTE: This alters the internal database schema tracker. USE WITH CAUTION!!!
func (s *PostgresStore) SetSchemaVersion(version int, dirty bool) error {
	driver, err := pgx_migrate.WithInstance(s.DB, &pgx_migrate.Config{})
	if err != nil {
		return errPostgresMigration(err)
	}

	return driver.SetVersion(version, dirty)
}
