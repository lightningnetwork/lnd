package sqldb

import (
	"context"
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
	ctxb := context.Background()

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

// getLastMigrationVersion return the version of the last migration in the
// embedded filesystem.
func getLastMigrationVersion(t *testing.T) int {
	t.Helper()

	// Walk through the embedded filesystem
	entries, err := sqlSchemas.ReadDir("sqlc/migrations")
	require.NoError(t, err)

	return len(entries)
}

// TestCustomMigration tests that a custom in-code migrations are executed
// during the migration process.
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
			Name:    "3",
			Version: 3,
			MigrationFn: func(db *BaseDB) error {
				logMigration("3")

				return nil
			},
		},
		{
			Name:    "1",
			Version: 1,
			MigrationFn: func(db *BaseDB) error {
				logMigration("1")

				return nil
			},
		},
		{
			// We use this special case to test that a migration
			// will never be aplied in case the current version is
			// higher than the recommended version.
			Name:    "-5",
			Version: -5,
			MigrationFn: func(db *BaseDB) error {
				logMigration("-5")

				return nil
			},
		},
		{
			Name:    "5",
			Version: 5,
			MigrationFn: func(db *BaseDB) error {
				logMigration("5")

				return nil
			},
		},
	}

	// Get the version of the last migration in the embedded filesystem.
	lastMigrationVersion := getLastMigrationVersion(t)

	tests := []struct {
		name                 string
		migrations           []MigrationConfig
		expectedSuccess      bool
		expectedMigrationLog []string
		expectedVersion      int
	}{
		{
			name:                 "success",
			migrations:           migrations,
			expectedSuccess:      true,
			expectedMigrationLog: []string{"1", "3", "5"},
			expectedVersion:      lastMigrationVersion,
		},
		{
			name: "failure",
			migrations: append(migrations, MigrationConfig{
				Name:    "4",
				Version: 4,
				MigrationFn: func(db *BaseDB) error {
					return fmt.Errorf("migration 4 failed")
				},
			}),
			expectedSuccess:      false,
			expectedMigrationLog: []string{"1", "3"},
		},
	}

	for _, test := range tests {
		// checkVersion checks the database schema version against
		// the expected version.
		checkVersion := func(t *testing.T,
			driver database.Driver, dbName string) {

			sqlMigrate, err := migrate.NewWithInstance(
				"migrations", nil, dbName, driver,
			)
			require.NoError(t, err)

			version, _, err := sqlMigrate.Version()
			require.NoError(t, err)
			require.Equal(t, test.expectedVersion, int(version))

		}

		t.Run("SQLite "+test.name, func(t *testing.T) {
			customMigrationLog = nil

			// First instantiate the database and run the migrations
			// including the custom migrations.
			t.Logf("Creating new SQLite DB for testing migrations")

			dbFileName := filepath.Join(t.TempDir(), "tmp.db")
			db, err := NewSqliteStore(&SqliteConfig{
				SkipMigrations: false,
			}, dbFileName, test.migrations...)

			if db != nil {
				t.Cleanup(func() {
					require.NoError(t, db.DB.Close())
				})
			}

			if test.expectedSuccess {
				// Create the migration executor to be able to
				// query the current version.
				driver, err := sqlite_migrate.WithInstance(
					db.DB, &sqlite_migrate.Config{},
				)
				require.NoError(t, err)

				checkVersion(t, driver, "")

			} else {
				require.Error(t, err)
			}

			require.Equal(t,
				test.expectedMigrationLog, customMigrationLog,
			)
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
				context.Background(), "CREATE DATABASE "+dbName,
			)
			require.NoError(t, err)

			cfg := fixture.GetConfig(dbName)
			db, err := NewPostgresStore(cfg, test.migrations...)

			if test.expectedSuccess {
				require.NoError(t, err)
				// Create the migration executor to be able to
				// query the current version.
				driver, err := pgx_migrate.WithInstance(
					db.DB, &pgx_migrate.Config{},
				)
				require.NoError(t, err)

				checkVersion(t, driver, dbName)
			} else {
				require.Error(t, err)
			}

			require.Equal(t,
				test.expectedMigrationLog, customMigrationLog,
			)
		})
	}
}
