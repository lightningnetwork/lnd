//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
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
	pragmaOptions := []struct {
		name  string
		value string
	}{
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

	db.SetMaxOpenConns(defaultMaxConns)
	db.SetMaxIdleConns(defaultMaxConns)
	db.SetConnMaxLifetime(connIdleLifetime)

	if !cfg.SkipMigrations {
		// Now that the database is open, populate the database with
		// our set of schemas based on our embedded in-memory file
		// system.
		//
		// First, we'll need to open up a new migration instance for
		// our current target database: sqlite.
		driver, err := sqlite_migrate.WithInstance(
			db, &sqlite_migrate.Config{},
		)
		if err != nil {
			return nil, err
		}

		// We use INTEGER PRIMARY KEY for sqlite, because it acts as a
		// ROWID alias which is 8 bytes big and also autoincrements.
		// It's important to use the ROWID as a primary key because the
		// key look ups are almost twice as fast
		sqliteFS := newReplacerFS(sqlSchemas, map[string]string{
			"BIGINT PRIMARY KEY": "INTEGER PRIMARY KEY",
		})

		err = applyMigrations(
			sqliteFS, driver, "sqlc/migrations", "sqlc",
		)
		if err != nil {
			return nil, err
		}
	}

	queries := sqlc.New(db)

	return &SqliteStore{
		cfg: cfg,
		BaseDB: &BaseDB{
			DB:      db,
			Queries: queries,
		},
	}, nil
}

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t *testing.T) *SqliteStore {
	t.Helper()

	t.Logf("Creating new SQLite DB for testing")

	// TODO(roasbeef): if we pass :memory: for the file name, then we get
	// an in mem version to speed up tests
	dbFileName := filepath.Join(t.TempDir(), "tmp.db")
	sqlDB, err := NewSqliteStore(&SqliteConfig{
		SkipMigrations: false,
	}, dbFileName)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}
