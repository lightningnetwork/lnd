//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // Register relevant drivers.
)

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t testing.TB, streams []MigrationStream) *SqliteStore {
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
