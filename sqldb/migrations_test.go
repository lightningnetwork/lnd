package sqldb

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	pgx_migrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// makeMigrationTestDB is a type alias for a function that creates a new test
// database and returns the base database and a function that executes selected
// migrations.
type makeMigrationTestDB = func(*testing.T, uint) (*BaseDB,
	func(MigrationTarget) error)

// TestMigrations is a meta test runner that runs all migration tests.
func TestMigrations(t *testing.T) {
	sqliteTestDB := func(t *testing.T, version uint) (*BaseDB,
		func(MigrationTarget) error) {

		db := NewTestSqliteDBWithVersion(t, version)

		return db.BaseDB, db.ExecuteMigrations
	}

	postgresTestDB := func(t *testing.T, version uint) (*BaseDB,
		func(MigrationTarget) error) {

		pgFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
		t.Cleanup(func() {
			pgFixture.TearDown(t)
		})

		db := NewTestPostgresDBWithVersion(
			t, pgFixture, version,
		)

		return db.BaseDB, db.ExecuteMigrations
	}

	tests := []struct {
		name string
		test func(*testing.T, makeMigrationTestDB)
	}{
		{
			name: "TestInvoiceExpiryMigration",
			test: testInvoiceExpiryMigration,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name+"_SQLite", func(t *testing.T) {
			test.test(t, sqliteTestDB)
		})

		t.Run(test.name+"_Postgres", func(t *testing.T) {
			test.test(t, postgresTestDB)
		})

	}
}

// TestInvoiceExpiryMigration tests that the migration from version 3 to 4
// correctly sets the expiry value of normal invoices to 86400 seconds and
// 2592000 seconds for AMP invoices.
func testInvoiceExpiryMigration(t *testing.T, makeDB makeMigrationTestDB) {
	t.Parallel()
	ctxb := t.Context()

	// Create a new database that already has the first version of the
	// native invoice schema.
	db, migrate := makeDB(t, 3)

	// Add a few invoices. For simplicity we reuse the payment hash as the
	// payment address and payment request hash instead of setting them to
	// NULL (to not run into uniqueness constraints). Note that SQLC
	// currently doesn't support nullable blob fields porperly. A workaround
	// is in progress: https://github.com/sqlc-dev/sqlc/issues/3149

	// Add an invoice where is_amp will be set to false.
	hash1 := []byte{1, 2, 3}
	_, err := db.InsertInvoice(ctxb, sqlc.InsertInvoiceParams{
		Hash:               hash1,
		PaymentAddr:        hash1,
		PaymentRequestHash: hash1,
		Expiry:             -123,
		IsAmp:              false,
	})
	require.NoError(t, err)

	// Add an invoice where is_amp will be set to false.
	hash2 := []byte{4, 5, 6}
	_, err = db.InsertInvoice(ctxb, sqlc.InsertInvoiceParams{
		Hash:               hash2,
		PaymentAddr:        hash2,
		PaymentRequestHash: hash2,
		Expiry:             -456,
		IsAmp:              true,
	})
	require.NoError(t, err)

	// Now, we'll attempt to execute the migration that will fix the expiry
	// values by inserting 86400 seconds for non AMP and 2592000 seconds for
	// AMP invoices.
	err = migrate(TargetVersion(4))

	invoices, err := db.FilterInvoices(ctxb, sqlc.FilterInvoicesParams{
		AddIndexGet: SQLInt64(1),
		NumLimit:    100,
	})

	const (
		// 1 day in seconds.
		expiry = int32(86400)
		// 30 days in seconds.
		expiryAMP = int32(2592000)
	)

	expected := []sqlc.Invoice{
		{
			ID:                 1,
			Hash:               hash1,
			PaymentAddr:        hash1,
			PaymentRequestHash: hash1,
			Expiry:             expiry,
		},
		{
			ID:                 2,
			Hash:               hash2,
			PaymentAddr:        hash2,
			PaymentRequestHash: hash2,
			Expiry:             expiryAMP,
			IsAmp:              true,
		},
	}

	for i := range invoices {
		// Override the timestamp location as the sql driver will scan
		// the timestamp with no location and we can't create such
		// timestamps in Golang using the standard time package.
		invoices[i].CreatedAt = invoices[i].CreatedAt.UTC()

		// Override the preimage as depending on the driver it is either
		// scanned as nil or an empty byte slice.
		require.Len(t, invoices[i].Preimage, 0)
		invoices[i].Preimage = nil
	}

	require.NoError(t, err)
	require.Equal(t, expected, invoices)
}

// TestCustomMigration tests that a custom in-code migrations are correctly
// executed during the migration process.
func TestCustomMigration(t *testing.T) {
	var customMigrationLog []string

	logMigration := func(name string) {
		customMigrationLog = append(customMigrationLog, name)
	}

	// Some migrations to use for both the failure and success tests. Note
	// that the migrations are not in order to test that they are executed
	// in the correct order.
	migrations := []MigrationConfig{
		{
			Name:          "1",
			Version:       1,
			SchemaVersion: 1,
			MigrationFn: func(*sqlc.Queries) error {
				logMigration("1")

				return nil
			},
		},
		{
			Name:          "2",
			Version:       2,
			SchemaVersion: 1,
			MigrationFn: func(*sqlc.Queries) error {
				logMigration("2")

				return nil
			},
		},
		{
			Name:          "3",
			Version:       3,
			SchemaVersion: 2,
			MigrationFn: func(*sqlc.Queries) error {
				logMigration("3")

				return nil
			},
		},
	}

	tests := []struct {
		name                  string
		migrations            []MigrationConfig
		expectedSuccess       bool
		expectedMigrationLog  []string
		expectedSchemaVersion int
		expectedVersion       int
	}{
		{
			name:                  "success",
			migrations:            migrations,
			expectedSuccess:       true,
			expectedMigrationLog:  []string{"1", "2", "3"},
			expectedSchemaVersion: 2,
			expectedVersion:       3,
		},
		{
			name: "unordered migrations",
			migrations: append([]MigrationConfig{
				{
					Name:          "4",
					Version:       4,
					SchemaVersion: 3,
					MigrationFn: func(*sqlc.Queries) error {
						logMigration("4")

						return nil
					},
				},
			}, migrations...),
			expectedSuccess:       false,
			expectedMigrationLog:  nil,
			expectedSchemaVersion: 0,
		},
		{
			name: "failure of migration 4",
			migrations: append(migrations, MigrationConfig{
				Name:          "4",
				Version:       4,
				SchemaVersion: 3,
				MigrationFn: func(*sqlc.Queries) error {
					return fmt.Errorf("migration 4 failed")
				},
			}),
			expectedSuccess:      false,
			expectedMigrationLog: []string{"1", "2", "3"},
			// Since schema migration is a separate step we expect
			// that migrating up to 3 succeeded.
			expectedSchemaVersion: 3,
			// We still remain on version 3 though.
			expectedVersion: 3,
		},
		{
			name: "success of migration 4",
			migrations: append(migrations, MigrationConfig{
				Name:          "4",
				Version:       4,
				SchemaVersion: 3,
				MigrationFn: func(*sqlc.Queries) error {
					logMigration("4")

					return nil
				},
			}),
			expectedSuccess:       true,
			expectedMigrationLog:  []string{"1", "2", "3", "4"},
			expectedSchemaVersion: 3,
			expectedVersion:       4,
		},
	}

	ctxb := t.Context()
	for _, test := range tests {
		// checkSchemaVersion checks the database schema version against
		// the expected version.
		getSchemaVersion := func(t *testing.T,
			driver database.Driver, dbName string) int {

			sqlMigrate, err := migrate.NewWithInstance(
				"migrations", nil, dbName, driver,
			)
			require.NoError(t, err)

			version, _, err := sqlMigrate.Version()
			if err != migrate.ErrNilVersion {
				require.NoError(t, err)
			}

			return int(version)
		}

		t.Run("SQLite "+test.name, func(t *testing.T) {
			customMigrationLog = nil

			// First instantiate the database and run the migrations
			// including the custom migrations.
			t.Logf("Creating new SQLite DB for testing migrations")

			dbFileName := filepath.Join(t.TempDir(), "tmp.db")
			var (
				db  *SqliteStore
				err error
			)

			// Run the migration 3 times to test that the migrations
			// are idempotent.
			for i := 0; i < 3; i++ {
				db, err = NewSqliteStore(&SqliteConfig{
					SkipMigrations: false,
				}, dbFileName)
				require.NoError(t, err)

				dbToCleanup := db.DB
				t.Cleanup(func() {
					require.NoError(
						t, dbToCleanup.Close(),
					)
				})

				err = db.ApplyAllMigrations(
					ctxb, test.migrations,
				)
				if test.expectedSuccess {
					require.NoError(t, err)
				} else {
					require.Error(t, err)

					// Also repoen the DB without migrations
					// so we can read versions.
					db, err = NewSqliteStore(&SqliteConfig{
						SkipMigrations: true,
					}, dbFileName)
					require.NoError(t, err)
				}

				require.Equal(t,
					test.expectedMigrationLog,
					customMigrationLog,
				)

				// Create the migration executor to be able to
				// query the current schema version.
				driver, err := sqlite_migrate.WithInstance(
					db.DB, &sqlite_migrate.Config{},
				)
				require.NoError(t, err)

				require.Equal(
					t, test.expectedSchemaVersion,
					getSchemaVersion(t, driver, ""),
				)

				// Check the migraton version in the database.
				version, err := db.GetDatabaseVersion(ctxb)
				if test.expectedSchemaVersion != 0 {
					require.NoError(t, err)
				} else {
					require.Equal(t, sql.ErrNoRows, err)
				}

				require.Equal(
					t, test.expectedVersion, int(version),
				)
			}
		})

		t.Run("Postgres "+test.name, func(t *testing.T) {
			customMigrationLog = nil

			// First create a temporary Postgres database to run
			// the migrations on.
			fixture := NewTestPgFixture(
				t, DefaultPostgresFixtureLifetime,
			)
			t.Cleanup(func() {
				fixture.TearDown(t)
			})

			dbName := randomDBName(t)

			// Next instantiate the database and run the migrations
			// including the custom migrations.
			t.Logf("Creating new Postgres DB '%s' for testing "+
				"migrations", dbName)

			_, err := fixture.db.ExecContext(
				t.Context(), "CREATE DATABASE "+dbName,
			)
			require.NoError(t, err)

			cfg := fixture.GetConfig(dbName)
			var db *PostgresStore

			// Run the migration 3 times to test that the migrations
			// are idempotent.
			for i := 0; i < 3; i++ {
				cfg.SkipMigrations = false
				db, err = NewPostgresStore(cfg)
				require.NoError(t, err)

				err = db.ApplyAllMigrations(
					ctxb, test.migrations,
				)
				if test.expectedSuccess {
					require.NoError(t, err)
				} else {
					require.Error(t, err)

					// Also repoen the DB without migrations
					// so we can read versions.
					cfg.SkipMigrations = true
					db, err = NewPostgresStore(cfg)
					require.NoError(t, err)
				}

				require.Equal(t,
					test.expectedMigrationLog,
					customMigrationLog,
				)

				// Create the migration executor to be able to
				// query the current version.
				driver, err := pgx_migrate.WithInstance(
					db.DB, &pgx_migrate.Config{},
				)
				require.NoError(t, err)

				require.Equal(
					t, test.expectedSchemaVersion,
					getSchemaVersion(t, driver, ""),
				)

				// Check the migraton version in the database.
				version, err := db.GetDatabaseVersion(ctxb)
				if test.expectedSchemaVersion != 0 {
					require.NoError(t, err)
				} else {
					require.Equal(t, sql.ErrNoRows, err)
				}

				require.Equal(
					t, test.expectedVersion, int(version),
				)
			}
		})
	}
}

// TestSchemaMigrationIdempotency tests that the our schema migrations are
// idempotent. This means that we can apply the migrations multiple times and
// the schema version will always be the same.
func TestSchemaMigrationIdempotency(t *testing.T) {
	dropMigrationTrackerEntries := func(t *testing.T, db *BaseDB) {
		_, err := db.Exec("DELETE FROM migration_tracker;")
		require.NoError(t, err)
	}

	lastMigration := migrationConfig[len(migrationConfig)-1]

	t.Run("SQLite", func(t *testing.T) {
		// First instantiate the database and run the migrations
		// including the custom migrations.
		t.Logf("Creating new SQLite DB for testing migrations")

		dbFileName := filepath.Join(t.TempDir(), "tmp.db")
		var (
			db  *SqliteStore
			err error
		)

		// Run the migration 3 times to test that the migrations
		// are idempotent.
		for i := 0; i < 3; i++ {
			db, err = NewSqliteStore(&SqliteConfig{
				SkipMigrations: false,
			}, dbFileName)
			require.NoError(t, err)

			dbToCleanup := db.DB
			t.Cleanup(func() {
				require.NoError(
					t, dbToCleanup.Close(),
				)
			})

			ctxb := t.Context()
			require.NoError(
				t, db.ApplyAllMigrations(ctxb, GetMigrations()),
			)

			version, dirty, err := db.GetSchemaVersion()
			require.NoError(t, err)

			// Now reset the schema version to 0 and make sure that
			// we can apply the migrations again.
			require.Equal(t, lastMigration.SchemaVersion, version)
			require.False(t, dirty)

			require.NoError(
				t, db.SetSchemaVersion(
					database.NilVersion, false,
				),
			)
			dropMigrationTrackerEntries(t, db.BaseDB)

			// Make sure that we reset the schema version.
			version, dirty, err = db.GetSchemaVersion()
			require.NoError(t, err)
			require.Equal(t, -1, version)
			require.False(t, dirty)
		}
	})

	t.Run("Postgres", func(t *testing.T) {
		// First create a temporary Postgres database to run
		// the migrations on.
		fixture := NewTestPgFixture(
			t, DefaultPostgresFixtureLifetime,
		)
		t.Cleanup(func() {
			fixture.TearDown(t)
		})

		dbName := randomDBName(t)

		// Next instantiate the database and run the migrations
		// including the custom migrations.
		t.Logf("Creating new Postgres DB '%s' for testing "+
			"migrations", dbName)

		_, err := fixture.db.ExecContext(
			t.Context(), "CREATE DATABASE "+dbName,
		)
		require.NoError(t, err)

		cfg := fixture.GetConfig(dbName)
		var db *PostgresStore

		// Run the migration 3 times to test that the migrations
		// are idempotent.
		for i := 0; i < 3; i++ {
			cfg.SkipMigrations = false
			db, err = NewPostgresStore(cfg)
			require.NoError(t, err)

			ctxb := t.Context()
			require.NoError(
				t, db.ApplyAllMigrations(ctxb, GetMigrations()),
			)

			version, dirty, err := db.GetSchemaVersion()
			require.NoError(t, err)

			// Now reset the schema version to 0 and make sure that
			// we can apply the migrations again.
			require.Equal(t, lastMigration.SchemaVersion, version)
			require.False(t, dirty)

			require.NoError(
				t, db.SetSchemaVersion(
					database.NilVersion, false,
				),
			)
			dropMigrationTrackerEntries(t, db.BaseDB)

			// Make sure that we reset the schema version.
			version, dirty, err = db.GetSchemaVersion()
			require.NoError(t, err)
			require.Equal(t, -1, version)
			require.False(t, dirty)
		}
	})
}

// TestMigrationBug19RC1 tests a bug that was present in the migration code
// at the v0.19.0-rc1 release.
// The bug was fixed in: https://github.com/lightningnetwork/lnd/pull/9647
// NOTE: This test may be removed once the final version of 0.19.0 is released.
func TestMigrationSucceedsAfterDirtyStateMigrationFailure19RC1(t *testing.T) {
	// setMigrationTrackerVersion is a helper function that
	// updates the migration tracker table to a specific version that
	// simulates the conditions of the bug.
	setMigrationTrackerVersion := func(t *testing.T, db *BaseDB) {
		_, err := db.Exec(`
	        DELETE FROM migration_tracker;
		INSERT INTO migration_tracker (version, migration_time)
			VALUES (2, CURRENT_TIMESTAMP);
	        `)
		require.NoError(t, err)
	}

	const (
		maxSchemaVersionBefore19RC1 = 4
		failingSchemaVersion        = 3
	)

	ctxb := t.Context()
	migrations := GetMigrations()
	migrations = migrations[:maxSchemaVersionBefore19RC1]
	lastMigration := migrations[len(migrations)-1]

	// Make sure that the last migration is the one we expect.
	require.Equal(
		t, maxSchemaVersionBefore19RC1, lastMigration.SchemaVersion,
	)

	t.Run("SQLite", func(t *testing.T) {
		// First instantiate the database and run the migrations
		// including the custom migrations.
		t.Logf("Creating new SQLite DB for testing migrations")

		dbFileName := filepath.Join(t.TempDir(), "tmp.db")
		var (
			db  *SqliteStore
			err error
		)

		db, err = NewSqliteStore(&SqliteConfig{
			SkipMigrations: false,
		}, dbFileName)
		require.NoError(t, err)

		dbToCleanup := db.DB
		t.Cleanup(func() {
			require.NoError(t, dbToCleanup.Close())
		})

		require.NoError(t, db.ApplyAllMigrations(ctxb, migrations))

		version, dirty, err := db.GetSchemaVersion()
		require.NoError(t, err)

		// Now reset the schema version to 0 and make sure that
		// we can apply the migrations again.
		require.Equal(t, lastMigration.SchemaVersion, version)
		require.False(t, dirty)

		// Set the schema version to the failing version and
		// make make the version dirty which essentially tells
		// golang-migrate that the migration failed.
		require.NoError(
			t, db.SetSchemaVersion(failingSchemaVersion, true),
		)

		// Set the migration tracker to the failing version.
		setMigrationTrackerVersion(t, db.BaseDB)

		// Close the DB so we can reopen it and apply the
		// migrations again.
		db.DB.Close()

		db, err = NewSqliteStore(&SqliteConfig{
			SkipMigrations: false,
		}, dbFileName)
		require.NoError(t, err)

		dbToCleanup2 := db.DB
		t.Cleanup(func() {
			require.NoError(t, dbToCleanup2.Close())
		})

		require.NoError(t, db.ApplyAllMigrations(ctxb, migrations))

		version, dirty, err = db.GetSchemaVersion()
		require.NoError(t, err)
		require.Equal(t, lastMigration.SchemaVersion, version)
		require.False(t, dirty)
	})

	t.Run("Postgres", func(t *testing.T) {
		// First create a temporary Postgres database to run
		// the migrations on.
		fixture := NewTestPgFixture(
			t, DefaultPostgresFixtureLifetime,
		)
		t.Cleanup(func() {
			fixture.TearDown(t)
		})

		dbName := randomDBName(t)

		// Next instantiate the database and run the migrations
		// including the custom migrations.
		t.Logf("Creating new Postgres DB '%s' for testing "+
			"migrations", dbName)

		_, err := fixture.db.ExecContext(
			t.Context(), "CREATE DATABASE "+dbName,
		)
		require.NoError(t, err)

		cfg := fixture.GetConfig(dbName)
		var db *PostgresStore

		cfg.SkipMigrations = false
		db, err = NewPostgresStore(cfg)
		require.NoError(t, err)

		require.NoError(t, db.ApplyAllMigrations(ctxb, migrations))

		version, dirty, err := db.GetSchemaVersion()
		require.NoError(t, err)

		// Now reset the schema version to 0 and make sure that
		// we can apply the migrations again.
		require.Equal(t, lastMigration.SchemaVersion, version)
		require.False(t, dirty)

		// Set the schema version to the failing version and
		// make make the version dirty which essentially tells
		// golang-migrate that the migration failed.
		require.NoError(
			t, db.SetSchemaVersion(failingSchemaVersion, true),
		)

		// Set the migration tracker to the failing version.
		setMigrationTrackerVersion(t, db.BaseDB)

		// Close the DB so we can reopen it and apply the
		// migrations again.
		db.DB.Close()

		db, err = NewPostgresStore(cfg)
		require.NoError(t, err)

		dbToCleanup2 := db.DB
		t.Cleanup(func() {
			require.NoError(t, dbToCleanup2.Close())
		})

		require.NoError(t, db.ApplyAllMigrations(ctxb, migrations))

		version, dirty, err = db.GetSchemaVersion()
		require.NoError(t, err)
		require.Equal(t, lastMigration.SchemaVersion, version)
		require.False(t, dirty)
	})
}
