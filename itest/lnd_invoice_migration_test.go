package itest

import (
	"database/sql"
	"path"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

func openKVBackend(ht *lntest.HarnessTest, hn *node.HarnessNode) kvdb.Backend {
	sqlbase.Init(0)

	var (
		backend kvdb.Backend
		err     error
	)

	switch hn.Cfg.DBBackend {
	case node.BackendSqlite:
		backend, err = kvdb.Open(
			kvdb.SqliteBackendName,
			ht.Context(),
			&sqlite.Config{
				Timeout:     defaultTimeout,
				BusyTimeout: defaultTimeout,
			},
			hn.Cfg.DBDir(), lncfg.SqliteChannelDBName,
			lncfg.NSChannelDB,
		)
		require.NoError(ht, err)

	case node.BackendPostgres:
		backend, err = kvdb.Open(
			kvdb.PostgresBackendName, ht.Context(),
			&postgres.Config{
				Dsn:     hn.Cfg.PostgresDsn,
				Timeout: defaultTimeout,
			}, lncfg.NSChannelDB,
		)
		require.NoError(ht, err)
	}

	return backend
}

func openNativeSQLInvoiceDB(ht *lntest.HarnessTest,
	hn *node.HarnessNode) invoices.InvoiceDB {

	db := openNativeSQLDB(ht, hn)

	executor := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) invoices.SQLInvoiceQueries {
			return db.WithTx(tx)
		},
	)

	return invoices.NewSQLStore(
		executor, clock.NewDefaultClock(),
	)
}

func openNativeSQLDB(ht *lntest.HarnessTest,
	hn *node.HarnessNode) *sqldb.BaseDB {

	var db *sqldb.BaseDB

	switch hn.Cfg.DBBackend {
	case node.BackendSqlite:
		sqliteStore, err := sqldb.NewSqliteStore(
			&sqldb.SqliteConfig{
				Timeout:     defaultTimeout,
				BusyTimeout: defaultTimeout,
			},
			path.Join(
				hn.Cfg.DBDir(),
				lncfg.SqliteNativeDBName,
			),
		)
		require.NoError(ht, err)
		db = sqliteStore.BaseDB

	case node.BackendPostgres:
		postgresStore, err := sqldb.NewPostgresStore(
			&sqldb.PostgresConfig{
				Dsn:     hn.Cfg.PostgresDsn,
				Timeout: defaultTimeout,
			},
		)
		require.NoError(ht, err)
		db = postgresStore.BaseDB
	}

	return db
}

// clampTime truncates the time of the passed invoice to the microsecond level.
func clampTime(invoice *invoices.Invoice) {
	trunc := func(t time.Time) time.Time {
		return t.Truncate(time.Microsecond)
	}

	invoice.CreationDate = trunc(invoice.CreationDate)

	if !invoice.SettleDate.IsZero() {
		invoice.SettleDate = trunc(invoice.SettleDate)
	}

	if invoice.IsAMP() {
		for setID, ampState := range invoice.AMPState {
			if ampState.SettleDate.IsZero() {
				continue
			}

			ampState.SettleDate = trunc(ampState.SettleDate)
			invoice.AMPState[setID] = ampState
		}
	}

	for _, htlc := range invoice.Htlcs {
		if !htlc.AcceptTime.IsZero() {
			htlc.AcceptTime = trunc(htlc.AcceptTime)
		}

		if !htlc.ResolveTime.IsZero() {
			htlc.ResolveTime = trunc(htlc.ResolveTime)
		}
	}
}

// testInvoiceMigration tests that the invoice migration from the old KV store
// to the new native SQL store works as expected.
func testInvoiceMigration(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", []string{"--accept-amp"})

	// Make sure we run the test with SQLite or Postgres.
	if bob.Cfg.DBBackend != node.BackendSqlite &&
		bob.Cfg.DBBackend != node.BackendPostgres {

		ht.Skip("node not running with SQLite or Postgres")
	}

	// Skip the test if the node is already running with native SQL.
	if bob.Cfg.NativeSQL {
		ht.Skip("node already running with native SQL")
	}

	ht.EnsureConnected(alice, bob)
	cp := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     1000000,
			PushAmt: 500000,
		},
	)

	// Alice and bob should have one channel open with each other now.
	ht.AssertNodeNumChannels(alice, 1)
	ht.AssertNodeNumChannels(bob, 1)

	// Step 1: Add 10 normal invoices and pay 5 of them.
	normalInvoices := make([]*lnrpc.AddInvoiceResponse, 10)
	for i := 0; i < 10; i++ {
		invoice := &lnrpc.Invoice{
			Value: int64(1000 + i*100), // Varying amounts
			IsAmp: false,
		}

		resp := bob.RPC.AddInvoice(invoice)
		normalInvoices[i] = resp
	}

	for _, inv := range normalInvoices {
		sendReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: inv.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}

		ht.SendPaymentAssertSettled(alice, sendReq)
	}

	// Step 2: Add 10 AMP invoices and send multiple payments to 5 of them.
	ampInvoices := make([]*lnrpc.AddInvoiceResponse, 10)
	for i := 0; i < 10; i++ {
		invoice := &lnrpc.Invoice{
			Value: int64(2000 + i*200), // Varying amounts
			IsAmp: true,
		}

		resp := bob.RPC.AddInvoice(invoice)
		ampInvoices[i] = resp
	}

	// Select the first 5 invoices to send multiple AMP payments.
	for i := 0; i < 5; i++ {
		inv := ampInvoices[i]

		// Send 3 payments to each.
		for j := 0; j < 3; j++ {
			payReq := &routerrpc.SendPaymentRequest{
				PaymentRequest: inv.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
				Amp:            true,
			}

			// Send a normal AMP payment first, then a spontaneous
			// AMP payment.
			ht.SendPaymentAssertSettled(alice, payReq)

			// Generate an external payment address when attempting
			// to pseudo-reuse an AMP invoice. When using an
			// external payment address, we'll also expect an extra
			// invoice to appear in the ListInvoices response, since
			// a new invoice will be JIT inserted under a different
			// payment address than the one in the invoice.
			//
			// NOTE: This will only work when the peer has
			// spontaneous AMP payments enabled otherwise no invoice
			// under a different payment_addr will be found.
			payReq.PaymentAddr = ht.Random32Bytes()
			ht.SendPaymentAssertSettled(alice, payReq)
		}
	}

	// We can close the channel now.
	ht.CloseChannel(alice, cp)

	// Now stop Bob so we can open the DB for examination.
	require.NoError(ht, bob.Stop())

	// Open the KV channel DB.
	db, err := channeldb.CreateWithBackend(openKVBackend(ht, bob))
	require.NoError(ht, err)

	query := invoices.InvoiceQuery{
		IndexOffset: 0,
		// As a sanity check, fetch more invoices than we have
		// to ensure that we did not add any extra invoices.
		NumMaxInvoices: 9999,
	}

	// Fetch all invoices and make sure we have 35 in total.
	result1, err := db.QueryInvoices(ht.Context(), query)
	require.NoError(ht, err)
	require.Equal(ht, 35, len(result1.Invoices))
	numInvoices := len(result1.Invoices)

	bob.SetExtraArgs([]string{"--db.use-native-sql"})

	// Now run the migration flow three times to ensure that each run is
	// idempotent.
	for i := 0; i < 3; i++ {
		// Start bob with the native SQL flag set. This will trigger the
		// migration to run.
		require.NoError(ht, bob.Start(ht.Context()))

		// At this point the migration should have completed and the
		// node should be running with native SQL. Now we'll stop Bob
		// again so we can safely examine the database.
		require.NoError(ht, bob.Stop())

		// Now we'll open the database with the native SQL backend and
		// fetch the invoices again to ensure that they were migrated
		// correctly.
		sqlInvoiceDB := openNativeSQLInvoiceDB(ht, bob)
		result2, err := sqlInvoiceDB.QueryInvoices(ht.Context(), query)
		require.NoError(ht, err)

		require.Equal(ht, numInvoices, len(result2.Invoices))

		// Simply zero out the add index so we don't fail on that when
		// comparing.
		for i := 0; i < numInvoices; i++ {
			result1.Invoices[i].AddIndex = 0
			result2.Invoices[i].AddIndex = 0

			// Clamp the precision to microseconds. Note that we
			// need to override both invoices as the original
			// invoice is coming from KV database, it was stored as
			// a binary serialized Go time.Time value which has
			// nanosecond precision. The migrated invoice is stored
			// in SQL in PostgreSQL has microsecond precision while
			// in SQLite it has nanosecond precision if using TEXT
			// storage class.
			clampTime(&result1.Invoices[i])
			clampTime(&result2.Invoices[i])
			require.Equal(
				ht, result1.Invoices[i], result2.Invoices[i],
			)
		}
	}

	// Now restart Bob without the --db.use-native-sql flag so we can check
	// that the KV tombstone was set and that Bob will fail to start.
	require.NoError(ht, bob.Stop())
	bob.SetExtraArgs(nil)

	// Bob should now fail to start due to the tombstone being set.
	require.NoError(ht, bob.StartLndCmd(ht.Context()))
	require.Error(ht, bob.WaitForProcessExit())

	// Start Bob again so the test can complete.
	bob.SetExtraArgs([]string{"--db.use-native-sql"})
	require.NoError(ht, bob.Start(ht.Context()))
}
