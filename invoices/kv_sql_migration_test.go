package invoices_test

import (
	"database/sql"
	"os"
	"path"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
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

	makeSQLDB := func(t *testing.T, sqlite bool) (*invpkg.SQLStore,
		*sqldb.TransactionExecutor[*sqlc.Queries]) {

		var db *sqldb.BaseDB
		if sqlite {
			db = sqldb.NewTestSqliteDB(t).BaseDB
		} else {
			db = sqldb.NewTestPostgresDB(t, pgFixture).BaseDB
		}

		invoiceExecutor := sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) invpkg.SQLInvoiceQueries {
				return db.WithTx(tx)
			},
		)

		genericExecutor := sqldb.NewTransactionExecutor(
			db, func(tx *sql.Tx) *sqlc.Queries {
				return db.WithTx(tx)
			},
		)

		testClock := clock.NewTestClock(time.Unix(1, 0))

		return invpkg.NewSQLStore(invoiceExecutor, testClock),
			genericExecutor
	}

	migrationTest := func(t *testing.T, kvStore *channeldb.DB,
		sqlite bool) {

		sqlInvoiceStore, sqlStore := makeSQLDB(t, sqlite)
		ctxb := t.Context()

		const batchSize = 11
		err := sqlStore.ExecTx(
			ctxb, sqldb.WriteTxOpt(), func(tx *sqlc.Queries) error {
				return invpkg.MigrateInvoicesToSQL(
					ctxb, kvStore.Backend, kvStore, tx,
					batchSize,
				)
			}, sqldb.NoOpReset,
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

		result2, err := sqlInvoiceStore.QueryInvoices(ctxb, query)
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
			invpkg.OverrideInvoiceTimeZone(&result2.Invoices[i])

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
			var kvStore *channeldb.DB

			// First check if we have a channel.sqlite file in the
			// testdata directory. If we do, we'll use that as the
			// channel db for the migration test.
			chanDBPath := path.Join(
				test.dbPath, lncfg.SqliteChannelDBName,
			)

			// Just some sane defaults for the sqlite config.
			const (
				timeout  = 5 * time.Second
				maxConns = 50
			)

			sqliteConfig := &sqlite.Config{
				Timeout:        timeout,
				BusyTimeout:    timeout,
				MaxConnections: maxConns,
			}

			if fileExists(chanDBPath) {
				sqlbase.Init(maxConns)

				sqliteBackend, err := kvdb.Open(
					kvdb.SqliteBackendName,
					t.Context(),
					sqliteConfig, test.dbPath,
					lncfg.SqliteChannelDBName,
					lncfg.NSChannelDB,
				)

				require.NoError(t, err)
				kvStore, err = channeldb.CreateWithBackend(
					sqliteBackend,
				)

				require.NoError(t, err)
			} else {
				kvStore = channeldb.OpenForTesting(
					t, test.dbPath,
				)
			}

			t.Run("Postgres", func(t *testing.T) {
				migrationTest(t, kvStore, false)
			})

			t.Run("SQLite", func(t *testing.T) {
				migrationTest(t, kvStore, true)
			})
		})
	}
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}

	return !info.IsDir()
}
