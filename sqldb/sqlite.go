//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/lightningnetwork/lnd/fn/v2"
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
	Config *SqliteConfig

	DbPath string

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
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	s := &SqliteStore{
		Config: cfg,
		DbPath: dbPath,
		BaseDB: &BaseDB{
			DB:             db,
			BackendType:    BackendTypeSqlite,
			SkipMigrations: cfg.SkipMigrations,
		},
	}

	return s, nil
}

// GetBaseDB returns the underlying BaseDB instance for the SQLite store.
// It is a trivial helper method to comply with the sqldb.DB interface.
func (s *SqliteStore) GetBaseDB() *BaseDB {
	return s.BaseDB
}

func errSqliteMigration(err error) error {
	return fmt.Errorf("error creating sqlite migration: %w", err)
}

// backupSqliteDatabase creates a backup of the given SQLite database.
func backupSqliteDatabase(srcDB *sql.DB, dbFullFilePath string) error {
	if srcDB == nil {
		return fmt.Errorf("backup source database is nil")
	}

	// Create a database backup file full path from the given source
	// database full file path.
	//
	// Get the current time and format it as a Unix timestamp in
	// nanoseconds.
	timestamp := time.Now().UnixNano()

	// Add the timestamp to the backup name.
	backupFullFilePath := fmt.Sprintf(
		"%s.%d.backup", dbFullFilePath, timestamp,
	)

	log.Infof("Creating backup of database file: %v -> %v",
		dbFullFilePath, backupFullFilePath)

	// Create the database backup.
	vacuumIntoQuery := "VACUUM INTO ?;"
	stmt, err := srcDB.Prepare(vacuumIntoQuery)
	if err != nil {
		return err
	}
	defer stmt.Close()

	_, err = stmt.Exec(backupFullFilePath)
	if err != nil {
		return err
	}

	return nil
}

// backupAndMigrate is a helper function that creates a database backup before
// initiating the migration, and then migrates the database to the latest
// version.
func (s *SqliteStore) backupAndMigrate(mig *migrate.Migrate,
	currentDbVersion int, maxMigrationVersion uint) error {

	// Determine if a database migration is necessary given the current
	// database version and the maximum migration version.
	versionUpgradePending := currentDbVersion < int(maxMigrationVersion)
	if !versionUpgradePending {
		log.Infof("Current database version is up-to-date, skipping "+
			"migration attempt and backup creation "+
			"(current_db_version=%v, max_migration_version=%v)",
			currentDbVersion, maxMigrationVersion)
		return nil
	}

	// At this point, we know that a database migration is necessary.
	// Create a backup of the database before starting the migration.
	if !s.Config.SkipMigrationDbBackup {
		log.Infof("Creating database backup (before applying " +
			"migration(s))")

		err := backupSqliteDatabase(s.DB, s.DbPath)
		if err != nil {
			return err
		}
	} else {
		log.Infof("Skipping database backup creation before applying " +
			"migration(s)")
	}

	log.Infof("Applying migrations to database")
	return mig.Up()
}

// ExecuteMigrations runs migrations for the sqlite database, depending on the
// target given, either all migrations or up to a given version.
func (s *SqliteStore) ExecuteMigrations(target MigrationTarget,
	stream MigrationStream) error {

	driver, err := sqlite_migrate.WithInstance(
		s.DB, &sqlite_migrate.Config{
			MigrationsTable: stream.MigrateTableName,
		},
	)
	if err != nil {
		return errSqliteMigration(err)
	}

	opts := &migrateOptions{
		latestVersion: fn.Some(stream.LatestMigrationVersion),
	}

	if stream.MakePostMigrationChecks != nil {
		postMigSteps, err := stream.MakePostMigrationChecks(s.BaseDB)
		if err != nil {
			return errPostgresMigration(err)
		}
		opts.postStepCallbacks = postMigSteps
	}

	// Populate the database with our set of schemas based on our embedded
	// in-memory file system.
	sqliteFS := newReplacerFS(stream.Schemas, sqliteSchemaReplacements)
	return applyMigrations(
		sqliteFS, driver, stream.SQLFileDirectory, "sqlite", target,
		opts,
	)
}

func (s *SqliteStore) DefaultTarget() MigrationTarget {
	return s.backupAndMigrate
}

func (s *SqliteStore) SkipMigrations() bool {
	return s.Config.SkipMigrations
}

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t *testing.T, streams []MigrationStream) *SqliteStore {
	t.Helper()

	t.Logf("Creating new SQLite DB for testing")

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests
	dbFileName := filepath.Join(t.TempDir(), "tmp.db")
	sqlDB, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: false,
	}, dbFileName)
	require.NoError(t, err)

	require.NoError(t, ApplyAllMigrations(sqlDB, streams))

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

// NewTestSqliteDBFromPath is a helper function that creates a SQLite database
// for testing from a given database file path.
func NewTestSqliteDBFromPath(t *testing.T, dbPath string,
	streams []MigrationStream) *SqliteStore {

	t.Helper()

	t.Logf("Creating new SQLite DB for testing, using DB path %s", dbPath)

	sqlDB, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: false,
	}, dbPath)
	require.NoError(t, err)

	require.NoError(t, ApplyAllMigrations(sqlDB, streams))

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

// NewTestSqliteDBWithVersion is a helper function that creates an SQLite
// database for testing and migrates it to the given version.
func NewTestSqliteDBWithVersion(t *testing.T, stream MigrationStream,
	version uint) *SqliteStore {

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

	err = sqlDB.ExecuteMigrations(TargetVersion(version), stream)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}
