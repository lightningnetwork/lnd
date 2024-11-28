package invoices_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestMigrationWithChannelDB tests the migration of invoices from a bolt backed
// channel.db to a SQL database. Note that this test does not attempt to be a
// complete migration test for all invoice types but rather is added as a tool
// for developers and users to debug invoice migration issues with an actual
// channel.db file.
func TestMigrationWithChannelDB(t *testing.T) {
	// First create a shared Postgres instance so we don't spawn a new
	// docker container for each test.
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	makeSQLDB := func(t *testing.T, sqlite bool) *invpkg.SQLStore {
		var db *sqldb.BaseDB
		if sqlite {
			db = sqldb.NewTestSqliteDB(t).BaseDB
		} else {
			db = sqldb.NewTestPostgresDB(t, pgFixture).BaseDB
		}

		executor := sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) invpkg.SQLInvoiceQueries {
				return db.WithTx(tx)
			},
		)

		testClock := clock.NewTestClock(time.Unix(1, 0))

		return invpkg.NewSQLStore(executor, testClock)
	}

	migrationTest := func(t *testing.T, kvStore *channeldb.DB,
		sqlite bool) {

		sqlStore := makeSQLDB(t, sqlite)
		ctxb := context.Background()

		const batchSize = 11
		err := invpkg.MigrateInvoicesToSQL(
			ctxb, kvStore.Backend, kvStore, sqlStore, batchSize,
		)
		require.NoError(t, err)

		// MigrateInvoices will check if the inserted invoice equals to
		// the migrated one, but as a sanity check, we'll also fetch the
		// invoices from the store and compare them to the original
		// invoices.
		query := invpkg.InvoiceQuery{
			IndexOffset: 0,
			// As a sanity check, fetch more invoices than we have
			// to ensure that we did not add any extra invoices.
			// Note that we don't really have a way to know the
			// exact number of invoices in the bolt db without first
			// iterating over all of them, but for test purposes
			// constant should be enough.
			NumMaxInvoices: 9999,
		}
		result1, err := kvStore.QueryInvoices(ctxb, query)
		require.NoError(t, err)
		numInvoices := len(result1.Invoices)

		result2, err := sqlStore.QueryInvoices(ctxb, query)
		require.NoError(t, err)
		require.Equal(t, numInvoices, len(result2.Invoices))

		// Simply zero out the add index so we don't fail on that when
		// comparing.
		for i := 0; i < numInvoices; i++ {
			result1.Invoices[i].AddIndex = 0
			result2.Invoices[i].AddIndex = 0

			// We need to override the timezone of the invoices as
			// the provided DB vs the test runners local time zone
			// might be different.
			invpkg.OverrideInvoiceTimeZone(&result1.Invoices[i])

			require.Equal(
				t, result1.Invoices[i], result2.Invoices[i],
			)
		}
	}

	tests := []struct {
		name   string
		dbPath string
	}{
		{
			"empty",
			t.TempDir(),
		},
		{
			"testdata",
			"testdata",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			store := channeldb.OpenForTesting(t, test.dbPath)

			t.Run("Postgres", func(t *testing.T) {
				migrationTest(t, store, false)
			})

			t.Run("SQLite", func(t *testing.T) {
				migrationTest(t, store, true)
			})
		})
	}
}
