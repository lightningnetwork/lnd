package itest

import (
	"database/sql"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// openNativeSQLPaymentsDB opens the native SQL payments store for the given
// node, using the already-migrated SQL database on disk.
func openNativeSQLPaymentsDB(ht *lntest.HarnessTest,
	hn *node.HarnessNode) paymentsdb.DB {

	db := openNativeSQLDB(ht, hn)

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) paymentsdb.SQLQueries {
			return db.WithTx(tx)
		},
	)

	queryCfg := sqldb.DefaultSQLiteConfig()
	if hn.Cfg.DBBackend != node.BackendSqlite {
		queryCfg = sqldb.DefaultPostgresConfig()
	}

	store, err := paymentsdb.NewSQLStore(
		&paymentsdb.SQLStoreConfig{
			QueryCfg: queryCfg,
		},
		executor,
	)
	require.NoError(ht, err)

	return store
}

// testPaymentMigration tests that the payment migration from the old KV store
// to the new native SQL store works correctly.
//
// Crucially, it also verifies that payments sent *after* the migration land in
// the SQL backend. This catches the regression where the schema migration was
// promoted to mainline but the store was still wired up to the KV backend —
// in that case pre-migration data is copied to SQL by the migration, but all
// new payments continue to be written to KV and are invisible to the SQL store.
func testPaymentMigration(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)

	// Make sure we run the test with SQLite or Postgres.
	if alice.Cfg.DBBackend != node.BackendSqlite &&
		alice.Cfg.DBBackend != node.BackendPostgres {

		ht.Skip("node not running with SQLite or Postgres")
	}

	// Skip the test if the node is already running with native SQL.
	if alice.Cfg.NativeSQL {
		ht.Skip("node already running with native SQL")
	}

	ht.EnsureConnected(alice, bob)
	cp := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     1_000_000,
			PushAmt: 500_000,
		},
	)

	const numPreMigrationPayments = 5

	// Step 1: Send payments from Alice to Bob before the migration. These
	// will be written to Alice's KV payment store.
	for i := range numPreMigrationPayments {
		invoice := bob.RPC.AddInvoice(&lnrpc.Invoice{
			Value: int64(1_000 + i*100),
		})

		ht.SendPaymentAssertSettled(
			alice, &routerrpc.SendPaymentRequest{
				PaymentRequest: invoice.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)
	}

	ht.CloseChannel(alice, cp)

	// Stop Alice so we can safely examine the database.
	require.NoError(ht, alice.Stop())

	// Open the KV payments store and confirm the pre-migration payment
	// count.
	kvPaymentsDB, err := paymentsdb.NewKVStore(openKVBackend(ht, alice))
	require.NoError(ht, err)

	allPaymentsQuery := paymentsdb.Query{
		MaxPayments:       9999,
		IncludeIncomplete: true,
	}

	kvResult, err := kvPaymentsDB.QueryPayments(
		ht.Context(), allPaymentsQuery,
	)
	require.NoError(ht, err)
	require.Len(ht, kvResult.Payments, numPreMigrationPayments)

	// Step 2: Start Alice with --db.use-native-sql to trigger the
	// migration. Run it three times to verify the migration is idempotent.
	alice.SetExtraArgs([]string{"--db.use-native-sql"})

	for range 3 {
		require.NoError(ht, alice.Start(ht.Context()))
		require.NoError(ht, alice.Stop())

		sqlPaymentsDB := openNativeSQLPaymentsDB(ht, alice)
		sqlResult, err := sqlPaymentsDB.QueryPayments(
			ht.Context(), allPaymentsQuery,
		)
		require.NoError(ht, err)
		require.Len(ht, sqlResult.Payments, numPreMigrationPayments)
	}

	// Step 3: Send payments after the migration. These must land in the
	// SQL backend. If the payments store was accidentally left pointing at
	// the KV backend, these payments would be missing from the SQL store
	// and the final assertion below would fail.
	require.NoError(ht, alice.Start(ht.Context()))

	ht.EnsureConnected(alice, bob)

	cp2 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     1_000_000,
			PushAmt: 500_000,
		},
	)

	const numPostMigrationPayments = 5

	for i := range numPostMigrationPayments {
		invoice := bob.RPC.AddInvoice(&lnrpc.Invoice{
			Value: int64(2_000 + i*100),
		})

		ht.SendPaymentAssertSettled(
			alice, &routerrpc.SendPaymentRequest{
				PaymentRequest: invoice.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)
	}

	ht.CloseChannel(alice, cp2)
	require.NoError(ht, alice.Stop())

	// Open the SQL store directly and verify it contains both the
	// pre-migration payments (migrated from KV) and the post-migration
	// payments (written directly to SQL).
	sqlPaymentsDB := openNativeSQLPaymentsDB(ht, alice)
	sqlResult, err := sqlPaymentsDB.QueryPayments(
		ht.Context(), allPaymentsQuery,
	)
	require.NoError(ht, err)

	totalExpected := numPreMigrationPayments + numPostMigrationPayments
	require.Len(
		ht, sqlResult.Payments, totalExpected,
		"expected %d payments in SQL store (pre + post migration), "+
			"got %d; if post-migration payments are missing the "+
			"payments store may be wired to the KV backend "+
			"instead of SQL in BuildDatabase", totalExpected,
		len(sqlResult.Payments),
	)

	// Verify that Alice can no longer start without --db.use-native-sql
	// now that the tombstone has been set.
	alice.SetExtraArgs(nil)
	require.NoError(ht, alice.StartLndCmd(ht.Context()))
	require.Error(ht, alice.WaitForProcessExit())

	// Start Alice again so the harness can clean up properly.
	alice.SetExtraArgs([]string{"--db.use-native-sql"})
	require.NoError(ht, alice.Start(ht.Context()))
}
