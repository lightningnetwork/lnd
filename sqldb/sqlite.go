//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"

	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
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

var (
	// sqliteSchemaReplacements maps schema strings to their SQLite
	// compatible replacements. Currently, no replacements are needed as our
	// SQL schema definition files are designed for SQLite compatibility.
	sqliteSchemaReplacements = map[string]string{}

	// Make sure SqliteStore implements the MigrationExecutor interface.
	_ MigrationExecutor = (*SqliteStore)(nil)

	// Make sure SqliteStore implements the DB interface.
	_ DB = (*SqliteStore)(nil)
)

// pragmaOption holds a key-value pair for a SQLite pragma setting.
type pragmaOption struct {
	name  string
	value string
}

// SqliteStore is a database store implementation that uses a sqlite backend.
type SqliteStore struct {
	cfg *SqliteConfig

	*BaseDB
}

// NewSqliteStore attempts to open a new sqlite database based on the passed
// config.
func NewSqliteStore(cfg *SqliteConfig, dbPath string) (*SqliteStore, error) {
	// The set of pragma options are accepted using query options. For now
	// we only want to ensure that foreign key constraints are properly
	// enforced.
	pragmaOptions := []pragmaOption{
		{
			name:  "foreign_keys",
			value: "on",
		},
		{
			name:  "journal_mode",
			value: "WAL",
		},
		{
			name:  "busy_timeout",
			value: "5000",
		},
		{
			// With the WAL mode, this ensures that we also do an
			// extra WAL sync after each transaction. The normal
			// sync mode skips this and gives better performance,
			// but risks durability.
			name:  "synchronous",
			value: "full",
		},
		{
			// This is used to ensure proper durability for users
			// running on Mac OS. It uses the correct fsync system
			// call to ensure items are fully flushed to disk.
			name:  "fullfsync",
			value: "true",
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

	// Construct the DSN which is just the database file name, appended
	// with the series of pragma options as a query URL string. For more
	// details on the formatting here, see the modernc.org/sqlite docs:
	// https://pkg.go.dev/modernc.org/sqlite#Driver.Open.
	dsn := fmt.Sprintf(
		"%v?%v&%v", dbPath, sqliteOptions.Encode(),
		sqliteTxLockImmediate,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// Create the migration tracker table before starting migrations to
	// ensure it can be used to track migration progress. Note that a
	// corresponding SQLC migration also creates this table, making this
	// operation a no-op in that context. Its purpose is to ensure
	// compatibility with SQLC query generation.
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

	db.SetMaxOpenConns(defaultMaxConns)
	db.SetMaxIdleConns(defaultMaxConns)
	db.SetConnMaxLifetime(connIdleLifetime)
	queries := sqlc.New(db)

	s := &SqliteStore{
		cfg: cfg,
		BaseDB: &BaseDB{
			DB:      db,
			Queries: queries,
		},
	}

	return s, nil
}

// GetBaseDB returns the underlying BaseDB instance for the SQLite store.
// It is a trivial helper method to comply with the sqldb.DB interface.
func (s *SqliteStore) GetBaseDB() *BaseDB {
	return s.BaseDB
}

// ApplyAllMigrations applies both the SQLC and custom in-code migrations to the
// SQLite database.
func (s *SqliteStore) ApplyAllMigrations(ctx context.Context,
	migrations []MigrationConfig) error {

	// Execute migrations unless configured to skip them.
	if s.cfg.SkipMigrations {
		return nil
	}

	return ApplyMigrations(ctx, s.BaseDB, s, migrations)
}

func errSqliteMigration(err error) error {
	return fmt.Errorf("error creating sqlite migration: %w", err)
}

// ExecuteMigrations runs migrations for the sqlite database, depending on the
// target given, either all migrations or up to a given version.
func (s *SqliteStore) ExecuteMigrations(target MigrationTarget) error {
	driver, err := sqlite_migrate.WithInstance(
		s.DB, &sqlite_migrate.Config{},
	)
	if err != nil {
		return errSqliteMigration(err)
	}

	// Populate the database with our set of schemas based on our embedded
	// in-memory file system.
	sqliteFS := newReplacerFS(sqlSchemas, sqliteSchemaReplacements)
	return applyMigrations(
		sqliteFS, driver, "sqlc/migrations", "sqlite", target,
	)
}

// GetSchemaVersion returns the current schema version of the SQLite database.
func (s *SqliteStore) GetSchemaVersion() (int, bool, error) {
	driver, err := sqlite_migrate.WithInstance(
		s.DB, &sqlite_migrate.Config{},
	)
	if err != nil {
		return 0, false, errSqliteMigration(err)
	}

	version, dirty, err := driver.Version()
	if err != nil {
		return 0, dirty, err
	}

	return version, dirty, nil
}

// SetSchemaVersion sets the schema version of the SQLite database.
//
// NOTE: This alters the internal database schema tracker. USE WITH CAUTION!!!
func (s *SqliteStore) SetSchemaVersion(version int, dirty bool) error {
	driver, err := sqlite_migrate.WithInstance(
		s.DB, &sqlite_migrate.Config{},
	)
	if err != nil {
		return errSqliteMigration(err)
	}

	return driver.SetVersion(version, dirty)
}

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t testing.TB) *SqliteStore {
	t.Helper()

	t.Logf("Creating new SQLite DB for testing")

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests
	dbFileName := filepath.Join(t.TempDir(), "tmp.db")
	sqlDB, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: false,
	}, dbFileName)
	require.NoError(t, err)

	require.NoError(t, sqlDB.ApplyAllMigrations(
		context.Background(), GetMigrations()),
	)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

// NewTestSqliteDBWithVersion is a helper function that creates an SQLite
// database for testing and migrates it to the given version.
func NewTestSqliteDBWithVersion(t *testing.T, version uint) *SqliteStore {
	t.Helper()

	t.Logf("Creating new SQLite DB for testing, migrating to version %d",
		version)

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests
	dbFileName := filepath.Join(t.TempDir(), "tmp.db")
	sqlDB, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: true,
	}, dbFileName)
	require.NoError(t, err)

	err = sqlDB.ExecuteMigrations(TargetVersion(version))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}
