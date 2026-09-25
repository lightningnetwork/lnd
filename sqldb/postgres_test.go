//go:build test_db_postgres
// +build test_db_postgres

package sqldb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// isSQLite is false if the build tag is set to test_db_postgres. It is used in
// tests that compile for both SQLite and Postgres databases to determine
// which database implementation is being used.
//
// TODO(elle): once we've updated to using sqldbv2, we can remove this since
// then we will have access to the DatabaseType on the BaseDB struct at runtime.
const isSQLite = false

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t *testing.T) *PostgresStore {
	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return NewTestPostgresDB(t, pgFixture)
}

// NewTestDBWithVersion is a helper function that creates a Postgres database
// for testing and migrates it to the given version.
func NewTestDBWithVersion(t *testing.T, version uint) *PostgresStore {
	pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	return NewTestPostgresDBWithVersion(t, pgFixture, version)
}

// TestPostgresMigrateDriverReleasesConns asserts that the schema version
// methods release their connection.
func TestPostgresMigrateDriverReleasesConns(t *testing.T) {
	t.Parallel()

	store := NewTestDB(t)

	serverConns := func() int {
		var n int
		err := store.DB.QueryRowContext(
			t.Context(), "SELECT count(*) FROM pg_stat_activity "+
				"WHERE datname = current_database()",
		).Scan(&n)
		require.NoError(t, err)

		return n
	}
	before := serverConns()

	version, dirty, err := store.GetSchemaVersion()
	require.NoError(t, err)
	require.NoError(t, store.SetSchemaVersion(version, dirty))

	require.Zero(t, store.DB.Stats().InUse)

	// The server ends a backend some time after its client closes it.
	require.Eventually(t, func() bool {
		return serverConns() <= before
	}, 10*time.Second, 100*time.Millisecond)
}

// TestPostgresMigrationSmallConnPool asserts that all migrations finish on a
// fresh database with a pool of two connections.
func TestPostgresMigrationSmallConnPool(t *testing.T) {
	t.Parallel()

	fixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	dbName := randomDBName(t)
	_, err := fixture.db.ExecContext(t.Context(), "CREATE DATABASE "+dbName)
	require.NoError(t, err)

	cfg := fixture.GetConfig(dbName)
	cfg.MaxConnections = 2

	store, err := NewPostgresStore(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.DB.Close())
	})

	// A leak blocks ApplyAllMigrations forever, so the test uses a deadline
	// and does not hang the test binary.
	done := make(chan error, 1)
	go func() {
		done <- store.ApplyAllMigrations(t.Context(), GetMigrations())
	}()

	select {
	case err := <-done:
		require.NoError(t, err)

	case <-time.After(time.Minute):
		t.Fatal("migrations deadlocked with a small connection pool")
	}
}
