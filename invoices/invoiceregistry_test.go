package invoices_test

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"testing/quick"
	"time"

	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestInvoiceRegistry is a master test which encompasses all tests using an
// InvoiceDB instance. The purpose of this test is to be able to run all tests
// with a custom DB instance, so that we can test the same logic with different
// DB implementations.
func TestInvoiceRegistry(t *testing.T) {
	testList := []struct {
		name string
		test func(t *testing.T,
			makeDB func(t *testing.T) (
				invpkg.InvoiceDB, *clock.TestClock))
	}{
		{
			name: "SettleInvoice",
			test: testSettleInvoice,
		},
		{
			name: "CancelInvoice",
			test: testCancelInvoice,
		},
		{
			name: "SettleHoldInvoice",
			test: testSettleHoldInvoice,
		},
		{
			name: "CancelHoldInvoice",
			test: testCancelHoldInvoice,
		},
		{
			name: "UnknownInvoice",
			test: testUnknownInvoice,
		},
		{
			name: "KeySend",
			test: testKeySend,
		},
		{
			name: "HoldKeysend",
			test: testHoldKeysend,
		},
		{
			name: "MppPayment",
			test: testMppPayment,
		},
		{
			name: "MppPaymentWithOverpayment",
			test: testMppPaymentWithOverpayment,
		},
		{
			name: "InvoiceExpiryWithRegistry",
			test: testInvoiceExpiryWithRegistry,
		},
		{
			name: "OldInvoiceRemovalOnStart",
			test: testOldInvoiceRemovalOnStart,
		},
		{
			name: "HeightExpiryWithRegistry",
			test: testHeightExpiryWithRegistry,
		},
		{
			name: "MultipleSetHeightExpiry",
			test: testMultipleSetHeightExpiry,
		},
		{
			name: "SettleInvoicePaymentAddrRequired",
			test: testSettleInvoicePaymentAddrRequired,
		},
		{
			name: "SettleInvoicePaymentAddrRequiredOptionalGrace",
			test: testSettleInvoicePaymentAddrRequiredOptionalGrace,
		},
		{
			name: "AMPWithoutMPPPayload",
			test: testAMPWithoutMPPPayload,
		},
		{
			name: "SpontaneousAmpPayment",
			test: testSpontaneousAmpPayment,
		},
		{
			name: "FailPartialMPPPaymentExternal",
			test: testFailPartialMPPPaymentExternal,
		},
		{
			name: "FailPartialAMPPayment",
			test: testFailPartialAMPPayment,
		},
		{
			name: "CancelAMPInvoicePendingHTLCs",
			test: testCancelAMPInvoicePendingHTLCs,
		},
	}

	makeKeyValueDB := func(t *testing.T) (invpkg.InvoiceDB,
		*clock.TestClock) {

		testClock := clock.NewTestClock(testTime)
		db, err := channeldb.MakeTestInvoiceDB(
			t, channeldb.OptionClock(testClock),
		)
		require.NoError(t, err, "unable to make test db")

		return db, testClock
	}

	// First create a shared Postgres instance so we don't spawn a new
	// docker container for each test.
	pgFixture := sqldb.NewTestPgFixture(
		t, sqldb.DefaultPostgresFixtureLifetime,
	)
	t.Cleanup(func() {
		pgFixture.TearDown(t)
	})

	makeSQLDB := func(t *testing.T, sqlite bool) (invpkg.InvoiceDB,
		*clock.TestClock) {

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

		testClock := clock.NewTestClock(testTime)

		return invpkg.NewSQLStore(executor, testClock), testClock
	}

	for _, test := range testList {
		test := test

		t.Run(test.name+"_KV", func(t *testing.T) {
			test.test(t, makeKeyValueDB)
		})

		t.Run(test.name+"_SQLite", func(t *testing.T) {
			test.test(t,
				func(t *testing.T) (
					invpkg.InvoiceDB, *clock.TestClock) {

					return makeSQLDB(t, true)
				})
		})

		t.Run(test.name+"_Postgres", func(t *testing.T) {
			test.test(t,
				func(t *testing.T) (
					invpkg.InvoiceDB, *clock.TestClock) {

					return makeSQLDB(t, false)
				})
		})
	}
}

// testSettleInvoice tests settling of an invoice and related notifications.
func testSettleInvoice(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	ctx := newTestContext(t, nil, makeDB)

	ctxb := t.Context()
	allSubscriptions, err := ctx.registry.SubscribeNotifications(ctxb, 0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		ctxb, testInvoicePaymentHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice.
	testInvoice := newInvoice(t, false, false)
	addIdx, err := ctx.registry.AddInvoice(
		ctxb, testInvoice, testInvoicePaymentHash,
	)
	if err != nil {
		t.Fatal(err)
	}

	if addIdx != 1 {
		t.Fatalf("expected addIndex to start with 1, but got %v",
			addIdx)
	}

	// We expect the open state to be sent to the single invoice subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.State != invpkg.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				newInvoice.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	hodlChan := make(chan interface{}, 1)

	// Try to settle invoice with an htlc that expires too soon.
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value,
		uint32(testCurrentHeight)+testInvoiceCltvDelta-1,
		testCurrentHeight, getCircuitKey(10), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	require.NotNil(t, resolution)
	failResolution := checkFailResolution(
		t, resolution, invpkg.ResultExpiryTooSoon,
	)
	require.Equal(t, testCurrentHeight, failResolution.AcceptHeight)

	// Settle invoice with a slightly higher amount.
	amtPaid := lnwire.MilliSatoshi(100500)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	require.NotNil(t, resolution)
	settleResolution := checkSettleResolution(
		t, resolution, testInvoicePreimage,
	)
	require.Equal(t, invpkg.ResultSettled, settleResolution.Outcome)

	// We expect the settled state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.State)
		}
		if update.AmtPaid != amtPaid {
			t.Fatal("invoice AmtPaid incorrect")
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a settled notification to be sent out.
	select {
	case settledInvoice := <-allSubscriptions.SettledInvoices:
		if settledInvoice.State != invpkg.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				settledInvoice.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// Try to settle again with the same htlc id. We need this idempotent
	// behaviour after a restart.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	require.NoError(t, err, "unexpected NotifyExitHopHtlc error")
	require.NotNil(t, resolution)
	settleResolution = checkSettleResolution(
		t, resolution, testInvoicePreimage,
	)
	require.Equal(t, invpkg.ResultReplayToSettled, settleResolution.Outcome)

	// Try to settle again with a new higher-valued htlc. This payment
	// should also be accepted, to prevent any change in behaviour for a
	// paid invoice that may open up a probe vector.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid+600, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(1), hodlChan,
		nil, testPayload,
	)
	require.NoError(t, err, "unexpected NotifyExitHopHtlc error")
	require.NotNil(t, resolution)
	settleResolution = checkSettleResolution(
		t, resolution, testInvoicePreimage,
	)
	require.Equal(
		t, invpkg.ResultDuplicateToSettled, settleResolution.Outcome,
	)

	// Try to settle again with a lower amount. This should fail just as it
	// would have failed if it were the first payment.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid-600, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(2), hodlChan,
		nil, testPayload,
	)
	require.NoError(t, err, "unexpected NotifyExitHopHtlc error")
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultAmountTooLow)

	// Check that settled amount is equal to the sum of values of the htlcs
	// 0 and 1.
	inv, err := ctx.registry.LookupInvoice(ctxb, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	if inv.AmtPaid != amtPaid+amtPaid+600 {
		t.Fatalf("amount incorrect: expected %v got %v",
			amtPaid+amtPaid+600, inv.AmtPaid)
	}

	// Try to cancel.
	err = ctx.registry.CancelInvoice(ctxb, testInvoicePaymentHash)
	require.ErrorIs(t, err, invpkg.ErrInvoiceAlreadySettled)

	// As this is a direct settle, we expect nothing on the hodl chan.
	select {
	case <-hodlChan:
		t.Fatal("unexpected resolution")
	default:
	}
}

func testCancelInvoiceImpl(t *testing.T, gc bool,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	cfg := defaultRegistryConfig()

	// If set to true, then also delete the invoice from the DB after
	// cancellation.
	cfg.GcCanceledInvoicesOnTheFly = gc
	ctx := newTestContext(t, &cfg, makeDB)

	ctxb := t.Context()
	allSubscriptions, err := ctx.registry.SubscribeNotifications(ctxb, 0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Try to cancel the not yet existing invoice. This should fail.
	err = ctx.registry.CancelInvoice(ctxb, testInvoicePaymentHash)
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice.
	testInvoice := newInvoice(t, false, false)
	_, err = ctx.registry.AddInvoice(
		ctxb, testInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	// We expect the open state to be sent to the single invoice subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractOpen {
			t.Fatalf(
				"expected state ContractOpen, but got %v",
				update.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.State != invpkg.ContractOpen {
			t.Fatalf(
				"expected state ContractOpen, but got %v",
				newInvoice.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// Cancel invoice.
	err = ctx.registry.CancelInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err)

	// We expect the canceled state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractCanceled {
			t.Fatalf(
				"expected state ContractCanceled, but got %v",
				update.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	if gc {
		// Check that the invoice has been deleted from the db.
		_, err = ctx.idb.LookupInvoice(
			ctxb, invpkg.InvoiceRefByHash(testInvoicePaymentHash),
		)
		require.Error(t, err)
	}

	// We expect no cancel notification to be sent to all invoice
	// subscribers (backwards compatibility).

	// Try to cancel again. Expect that we report ErrInvoiceNotFound if the
	// invoice has been garbage collected (since the invoice has been
	// deleted when it was canceled), and no error otherwise.
	err = ctx.registry.CancelInvoice(ctxb, testInvoicePaymentHash)

	if gc {
		require.Error(t, err, invpkg.ErrInvoiceNotFound)
	} else {
		require.NoError(t, err)
	}

	// Notify arrival of a new htlc paying to this invoice. This should
	// result in a cancel resolution.
	hodlChan := make(chan interface{})
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoiceAmount, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatal("expected settlement of a canceled invoice to succeed")
	}
	require.NotNil(t, resolution)

	// If the invoice has been deleted (or not present) then we expect the
	// outcome to be ResultInvoiceNotFound instead of when the invoice is
	// in our database in which case we expect ResultInvoiceAlreadyCanceled.
	var failResolution *invpkg.HtlcFailResolution
	if gc {
		failResolution = checkFailResolution(
			t, resolution, invpkg.ResultInvoiceNotFound,
		)
	} else {
		failResolution = checkFailResolution(
			t, resolution, invpkg.ResultInvoiceAlreadyCanceled,
		)
	}

	require.Equal(t, testCurrentHeight, failResolution.AcceptHeight)
}

// testCancelInvoice tests cancellation of an invoice and related notifications.
func testCancelInvoice(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	// Test cancellation both with garbage collection (meaning that canceled
	// invoice will be deleted) and without (meaning it'll be kept).
	t.Run("garbage collect", func(t *testing.T) {
		testCancelInvoiceImpl(t, true, makeDB)
	})

	t.Run("no garbage collect", func(t *testing.T) {
		testCancelInvoiceImpl(t, false, makeDB)
	})
}

// testSettleHoldInvoice tests settling of a hold invoice and related
// notifications.
func testSettleHoldInvoice(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	idb, clock := makeDB(t)

	// Instantiate and start the invoice ctx.registry.
	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                clock,
		HtlcInterceptor:      &invpkg.MockHtlcModifier{},
	}

	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		cfg.Clock, 0, uint32(testCurrentHeight), nil, newMockNotifier(),
	)
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	err := registry.Start()
	require.NoError(t, err)
	defer registry.Stop()

	ctxb := t.Context()
	allSubscriptions, err := registry.SubscribeNotifications(ctxb, 0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := registry.SubscribeSingleInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice.
	invoice := newInvoice(t, true, false)
	_, err = registry.AddInvoice(ctxb, invoice, testInvoicePaymentHash)
	require.NoError(t, err)

	// We expect the open state to be sent to the single invoice subscriber.
	update := <-subscription.Updates
	if update.State != invpkg.ContractOpen {
		t.Fatalf("expected state ContractOpen, but got %v",
			update.State)
	}

	// We expect a new invoice notification to be sent out.
	newInvoice := <-allSubscriptions.NewInvoices
	if newInvoice.State != invpkg.ContractOpen {
		t.Fatalf("expected state ContractOpen, but got %v",
			newInvoice.State)
	}

	// Use slightly higher amount for accept/settle.
	amtPaid := lnwire.MilliSatoshi(100500)

	hodlChan := make(chan interface{}, 1)

	// NotifyExitHopHtlc without a preimage present in the invoice registry
	// should be possible.
	resolution, err := registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test idempotency.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test replay at a higher height. We expect the same result because it
	// is a replay.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight+10, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test a new htlc coming in that doesn't meet the final cltv delta
	// requirement. It should be rejected.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, 1, testCurrentHeight,
		getCircuitKey(1), hodlChan, nil, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultExpiryTooSoon)

	// We expect the accepted state to be sent to the single invoice
	// subscriber. For all invoice subscribers, we don't expect an update.
	// Those only get notified on settle.
	update = <-subscription.Updates
	if update.State != invpkg.ContractAccepted {
		t.Fatalf("expected state ContractAccepted, but got %v",
			update.State)
	}
	if update.AmtPaid != amtPaid {
		t.Fatal("invoice AmtPaid incorrect")
	}

	// Settling with preimage should succeed.
	err = registry.SettleHodlInvoice(ctxb, testInvoicePreimage)
	require.NoError(t, err, "expected set preimage to succeed")

	htlcResolution, _ := (<-hodlChan).(invpkg.HtlcResolution)
	require.NotNil(t, htlcResolution)
	settleResolution := checkSettleResolution(
		t, htlcResolution, testInvoicePreimage,
	)
	require.Equal(t, testCurrentHeight, settleResolution.AcceptHeight)
	require.Equal(t, invpkg.ResultSettled, settleResolution.Outcome)

	// We expect a settled notification to be sent out for both all and
	// single invoice subscribers.
	settledInvoice := <-allSubscriptions.SettledInvoices
	if settledInvoice.State != invpkg.ContractSettled {
		t.Fatalf("expected state ContractSettled, but got %v",
			settledInvoice.State)
	}
	if settledInvoice.AmtPaid != amtPaid {
		t.Fatalf("expected amount to be %v, but got %v",
			amtPaid, settledInvoice.AmtPaid)
	}

	update = <-subscription.Updates
	if update.State != invpkg.ContractSettled {
		t.Fatalf("expected state ContractSettled, but got %v",
			update.State)
	}

	// Idempotency.
	err = registry.SettleHodlInvoice(ctxb, testInvoicePreimage)
	require.ErrorIs(t, err, invpkg.ErrInvoiceAlreadySettled)

	// Try to cancel.
	err = registry.CancelInvoice(ctxb, testInvoicePaymentHash)
	require.Error(
		t, err, "expected cancellation of a settled invoice to fail",
	)
}

// testCancelHoldInvoice tests canceling of a hold invoice and related
// notifications.
func testCancelHoldInvoice(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	idb, testClock := makeDB(t)

	// Instantiate and start the invoice ctx.registry.
	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                testClock,
		HtlcInterceptor:      &invpkg.MockHtlcModifier{},
	}
	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		cfg.Clock, 0, uint32(testCurrentHeight), nil, newMockNotifier(),
	)
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	err := registry.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, registry.Stop())
	})

	ctxb := t.Context()

	// Add the invoice.
	invoice := newInvoice(t, true, false)
	_, err = registry.AddInvoice(ctxb, invoice, testInvoicePaymentHash)
	require.NoError(t, err)

	amtPaid := lnwire.MilliSatoshi(100000)
	hodlChan := make(chan interface{}, 1)

	// NotifyExitHopHtlc without a preimage present in the invoice registry
	// should be possible.
	resolution, err := registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Cancel invoice.
	err = registry.CancelInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err, "cancel invoice failed")

	htlcResolution, _ := (<-hodlChan).(invpkg.HtlcResolution)
	require.NotNil(t, htlcResolution)
	checkFailResolution(t, htlcResolution, invpkg.ResultCanceled)

	// Offering the same htlc again at a higher height should still result
	// in a rejection. The accept height is expected to be the original
	// accept height.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight+1, getCircuitKey(0), hodlChan,
		nil, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	require.NotNil(t, resolution)
	failResolution := checkFailResolution(
		t, resolution, invpkg.ResultReplayToCanceled,
	)
	require.Equal(t, testCurrentHeight, failResolution.AcceptHeight)
}

// testUnknownInvoice tests that invoice registry returns an error when the
// invoice is unknown. This is to guard against returning a cancel htlc
// resolution for forwarded htlcs. In the link, NotifyExitHopHtlc is only called
// if we are the exit hop, but in htlcIncomingContestResolver it is called with
// forwarded htlc hashes as well.
func testUnknownInvoice(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	ctx := newTestContext(t, nil, makeDB)

	// Notify arrival of a new htlc paying to this invoice. This should
	// succeed.
	hodlChan := make(chan interface{})
	amt := lnwire.MilliSatoshi(100000)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amt, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, nil, testPayload,
	)
	if err != nil {
		t.Fatal("unexpected error")
	}
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultInvoiceNotFound)
}

// testKeySend tests receiving a spontaneous payment with and without keysend
// enabled.
func testKeySend(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	t.Run("enabled", func(t *testing.T) {
		testKeySendImpl(t, true, makeDB)
	})
	t.Run("disabled", func(t *testing.T) {
		testKeySendImpl(t, false, makeDB)
	})
}

// testKeySendImpl is the inner test function that tests keysend for a
// particular enabled state on the receiver end.
func testKeySendImpl(t *testing.T, keySendEnabled bool,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	cfg := defaultRegistryConfig()
	cfg.AcceptKeySend = keySendEnabled
	ctx := newTestContext(t, &cfg, makeDB)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(
		t.Context(), 0, 0,
	)
	require.NoError(t, err)
	defer allSubscriptions.Cancel()

	hodlChan := make(chan interface{}, 1)

	amt := lnwire.MilliSatoshi(1000)
	expiry := uint32(testCurrentHeight + 20)

	// Create key for keysend.
	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()

	// Try to settle invoice with an invalid keysend htlc.
	invalidKeySendPayload := &mockPayload{
		customRecords: map[uint64][]byte{
			record.KeySendType: {1, 2, 3},
		},
	}

	resolution, err := ctx.registry.NotifyExitHopHtlc(
		hash, amt, expiry,
		testCurrentHeight, getCircuitKey(10), hodlChan,
		nil, invalidKeySendPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	require.NotNil(t, resolution)

	if !keySendEnabled {
		checkFailResolution(t, resolution, invpkg.ResultInvoiceNotFound)
	} else {
		checkFailResolution(t, resolution, invpkg.ResultKeySendError)
	}

	// Try to settle invoice with a valid keysend htlc.
	keySendPayload := &mockPayload{
		customRecords: map[uint64][]byte{
			record.KeySendType: preimage[:],
		},
	}

	resolution, err = ctx.registry.NotifyExitHopHtlc(
		hash, amt, expiry, testCurrentHeight, getCircuitKey(10),
		hodlChan, nil, keySendPayload,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Expect a cancel resolution if keysend is disabled.
	if !keySendEnabled {
		checkFailResolution(t, resolution, invpkg.ResultInvoiceNotFound)
		return
	}

	checkSubscription := func() {
		// We expect a new invoice notification to be sent out.
		newInvoice := <-allSubscriptions.NewInvoices
		require.Equal(t, newInvoice.State, invpkg.ContractOpen)

		// We expect a settled notification to be sent out.
		settledInvoice := <-allSubscriptions.SettledInvoices
		require.Equal(t, settledInvoice.State, invpkg.ContractSettled)
	}

	checkSettleResolution(t, resolution, preimage)
	checkSubscription()

	// Replay the same keysend payment. We expect an identical resolution,
	// but no event should be generated.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		hash, amt, expiry, testCurrentHeight, getCircuitKey(10),
		hodlChan, nil, keySendPayload,
	)
	require.Nil(t, err)
	checkSettleResolution(t, resolution, preimage)

	select {
	case <-allSubscriptions.NewInvoices:
		t.Fatalf("replayed keysend should not generate event")
	case <-time.After(time.Second):
	}

	// Finally, test that we can properly fulfill a second keysend payment
	// with a unique preimage.
	preimage2 := lntypes.Preimage{1, 2, 3, 4}
	hash2 := preimage2.Hash()

	keySendPayload2 := &mockPayload{
		customRecords: map[uint64][]byte{
			record.KeySendType: preimage2[:],
		},
	}

	resolution, err = ctx.registry.NotifyExitHopHtlc(
		hash2, amt, expiry, testCurrentHeight, getCircuitKey(20),
		hodlChan, nil, keySendPayload2,
	)
	require.Nil(t, err)

	checkSettleResolution(t, resolution, preimage2)
	checkSubscription()
}

// testHoldKeysend tests receiving a spontaneous payment that is held.
func testHoldKeysend(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	t.Run("settle", func(t *testing.T) {
		testHoldKeysendImpl(t, false, makeDB)
	})
	t.Run("timeout", func(t *testing.T) {
		testHoldKeysendImpl(t, true, makeDB)
	})
}

// testHoldKeysendImpl is the inner test function that tests hold-keysend.
func testHoldKeysendImpl(t *testing.T, timeoutKeysend bool,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	const holdDuration = time.Minute

	cfg := defaultRegistryConfig()
	cfg.AcceptKeySend = true
	cfg.KeysendHoldTime = holdDuration
	ctx := newTestContext(t, &cfg, makeDB)

	ctxb := t.Context()
	allSubscriptions, err := ctx.registry.SubscribeNotifications(ctxb, 0, 0)
	require.NoError(t, err)
	defer allSubscriptions.Cancel()

	hodlChan := make(chan interface{}, 1)

	amt := lnwire.MilliSatoshi(1000)
	expiry := uint32(testCurrentHeight + 20)

	// Create key for keysend.
	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()

	// Try to settle invoice with a valid keysend htlc.
	keysendPayload := &mockPayload{
		customRecords: map[uint64][]byte{
			record.KeySendType: preimage[:],
		},
	}

	resolution, err := ctx.registry.NotifyExitHopHtlc(
		hash, amt, expiry, testCurrentHeight, getCircuitKey(10),
		hodlChan, nil, keysendPayload,
	)
	if err != nil {
		t.Fatal(err)
	}

	// No immediate resolution is expected.
	require.Nil(t, resolution, "expected hold resolution")

	// We expect a new invoice notification to be sent out.
	newInvoice := <-allSubscriptions.NewInvoices
	if newInvoice.State != invpkg.ContractOpen {
		t.Fatalf("expected state ContractOpen, but got %v",
			newInvoice.State)
	}

	// We expect no further invoice notifications yet (on the all invoices
	// subscription).
	select {
	case <-allSubscriptions.NewInvoices:
		t.Fatalf("no invoice update expected")
	case <-time.After(100 * time.Millisecond):
	}

	if timeoutKeysend {
		// Advance the clock to just past the hold duration.
		ctx.clock.SetTime(ctx.clock.Now().Add(
			holdDuration + time.Millisecond),
		)

		// Expect the keysend payment to be failed.
		res := <-hodlChan
		failResolution, ok := res.(*invpkg.HtlcFailResolution)
		require.Truef(
			t, ok, "expected fail resolution, got: %T",
			resolution,
		)
		require.Equal(
			t, invpkg.ResultCanceled, failResolution.Outcome,
			"expected keysend payment to be failed",
		)

		return
	}

	// Settle keysend payment manually.
	require.NoError(t, ctx.registry.SettleHodlInvoice(
		ctxb, *newInvoice.Terms.PaymentPreimage,
	))

	// We expect a settled notification to be sent out.
	settledInvoice := <-allSubscriptions.SettledInvoices
	require.Equal(t, settledInvoice.State, invpkg.ContractSettled)
}

// testMppPayment tests settling of an invoice with multiple partial payments.
// It covers the case where there is a mpp timeout before the whole invoice is
// paid and the case where the invoice is settled in time.
func testMppPayment(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	ctx := newTestContext(t, nil, makeDB)
	ctxb := t.Context()

	// Add the invoice.
	testInvoice := newInvoice(t, false, false)
	_, err := ctx.registry.AddInvoice(
		ctxb, testInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	mppPayload := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, [32]byte{}),
	}

	// Send htlc 1.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry, testCurrentHeight, getCircuitKey(10),
		hodlChan1, nil, mppPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolution != nil {
		t.Fatal("expected no direct resolution")
	}

	// Simulate mpp timeout releasing htlc 1.
	ctx.clock.SetTime(testTime.Add(30 * time.Second))

	htlcResolution, _ := (<-hodlChan1).(invpkg.HtlcResolution)
	failResolution, ok := htlcResolution.(*invpkg.HtlcFailResolution)
	if !ok {
		t.Fatalf("expected fail resolution, got: %T",
			resolution)
	}
	if failResolution.Outcome != invpkg.ResultMppTimeout {
		t.Fatalf("expected mpp timeout, got: %v",
			failResolution.Outcome)
	}

	// Send htlc 2.
	hodlChan2 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry, testCurrentHeight, getCircuitKey(11),
		hodlChan2, nil, mppPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolution != nil {
		t.Fatal("expected no direct resolution")
	}

	// Send htlc 3.
	hodlChan3 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry, testCurrentHeight, getCircuitKey(12),
		hodlChan3, nil, mppPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	settleResolution, ok := resolution.(*invpkg.HtlcSettleResolution)
	if !ok {
		t.Fatalf("expected settle resolution, got: %T",
			htlcResolution)
	}
	if settleResolution.Outcome != invpkg.ResultSettled {
		t.Fatalf("expected result settled, got: %v",
			settleResolution.Outcome)
	}

	// Check that settled amount is equal to the sum of values of the htlcs
	// 2 and 3.
	inv, err := ctx.registry.LookupInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err)
	if inv.State != invpkg.ContractSettled {
		t.Fatal("expected invoice to be settled")
	}
	if inv.AmtPaid != testInvoice.Terms.Value {
		t.Fatalf("amount incorrect, expected %v but got %v",
			testInvoice.Terms.Value, inv.AmtPaid)
	}
}

// testMppPaymentWithOverpayment tests settling of an invoice with multiple
// partial payments. It covers the case where the mpp overpays what is in the
// invoice.
func testMppPaymentWithOverpayment(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	ctxb := t.Context()
	f := func(overpaymentRand uint64) bool {
		ctx := newTestContext(t, nil, makeDB)

		// Add the invoice.
		testInvoice := newInvoice(t, false, false)
		_, err := ctx.registry.AddInvoice(
			ctxb, testInvoice, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		mppPayload := &mockPayload{
			mpp: record.NewMPP(testInvoiceAmount, [32]byte{}),
		}

		// We constrain overpayment amount to be [1,1000].
		overpayment := lnwire.MilliSatoshi((overpaymentRand % 999) + 1)

		// Send htlc 1.
		hodlChan1 := make(chan interface{}, 1)
		resolution, err := ctx.registry.NotifyExitHopHtlc(
			testInvoicePaymentHash, testInvoice.Terms.Value/2,
			testHtlcExpiry, testCurrentHeight, getCircuitKey(11),
			hodlChan1, nil, mppPayload,
		)
		if err != nil {
			t.Fatal(err)
		}
		if resolution != nil {
			t.Fatal("expected no direct resolution")
		}

		// Send htlc 2.
		hodlChan2 := make(chan interface{}, 1)
		resolution, err = ctx.registry.NotifyExitHopHtlc(
			testInvoicePaymentHash,
			testInvoice.Terms.Value/2+overpayment, testHtlcExpiry,
			testCurrentHeight, getCircuitKey(12), hodlChan2,
			nil, mppPayload,
		)
		if err != nil {
			t.Fatal(err)
		}
		settleResolution, ok :=
			resolution.(*invpkg.HtlcSettleResolution)
		if !ok {
			t.Fatalf("expected settle resolution, got: %T",
				resolution)
		}
		if settleResolution.Outcome != invpkg.ResultSettled {
			t.Fatalf("expected result settled, got: %v",
				settleResolution.Outcome)
		}

		// Check that settled amount is equal to the sum of values of
		// the htlcs 1 and 2.
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)
		if inv.State != invpkg.ContractSettled {
			t.Fatal("expected invoice to be settled")
		}

		return inv.AmtPaid == testInvoice.Terms.Value+overpayment
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatalf("amount incorrect: %v", err)
	}
}

// testInvoiceExpiryWithRegistry tests that invoices are canceled after
// expiration.
func testInvoiceExpiryWithRegistry(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	idb, testClock := makeDB(t)

	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                testClock,
		HtlcInterceptor:      &invpkg.MockHtlcModifier{},
	}

	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		cfg.Clock, 0, uint32(testCurrentHeight), nil, newMockNotifier(),
	)
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	// First prefill the Channel DB with some pre-existing invoices,
	// half of them still pending, half of them expired.
	const numExpired = 5
	const numPending = 5
	existingInvoices := generateInvoiceExpiryTestData(
		t, testTime, 0, numExpired, numPending,
	)

	ctxb := t.Context()

	var expectedCancellations []lntypes.Hash
	expiredInvoices := existingInvoices.expiredInvoices
	for paymentHash, expiredInvoice := range expiredInvoices {
		_, err := idb.AddInvoice(ctxb, expiredInvoice, paymentHash)
		require.NoError(t, err)
		expectedCancellations = append(
			expectedCancellations, paymentHash,
		)
	}

	pendingInvoices := existingInvoices.pendingInvoices
	for paymentHash, pendingInvoice := range pendingInvoices {
		_, err := idb.AddInvoice(ctxb, pendingInvoice, paymentHash)
		require.NoError(t, err)
	}

	require.NoError(t, registry.Start(), "cannot start registry")

	// Now generate pending and invoices and add them to the registry while
	// it is up and running. We'll manipulate the clock to let them expire.
	newInvoices := generateInvoiceExpiryTestData(
		t, testTime, numExpired+numPending, 0, numPending,
	)

	var invoicesThatWillCancel []lntypes.Hash
	for paymentHash, pendingInvoice := range newInvoices.pendingInvoices {
		_, err := registry.AddInvoice(ctxb, pendingInvoice, paymentHash)
		require.NoError(t, err)
		invoicesThatWillCancel = append(
			invoicesThatWillCancel, paymentHash,
		)
	}

	// Check that they are really not canceled until before the clock is
	// advanced.
	for i := range invoicesThatWillCancel {
		invoice, err := registry.LookupInvoice(
			ctxb, invoicesThatWillCancel[i],
		)
		require.NoError(t, err, "cannot find invoice")
		require.NotEqual(t, invpkg.ContractCanceled, invoice.State,
			"expected pending invoice, got canceled")
	}

	// Fwd time 1 day.
	testClock.SetTime(testTime.Add(24 * time.Hour))

	// Create the expected cancellation set before the final check.
	expectedCancellations = append(
		expectedCancellations, invoicesThatWillCancel...,
	)

	// canceled returns a bool to indicate whether all the invoices are
	// canceled.
	canceled := func() error {
		for i := range expectedCancellations {
			invoice, err := registry.LookupInvoice(
				ctxb, expectedCancellations[i],
			)
			if err != nil {
				return err
			}

			if invoice.State != invpkg.ContractCanceled {
				return fmt.Errorf("expected state %v, got %v",
					invpkg.ContractCanceled, invoice.State)
			}
		}

		return nil
	}

	// Retrospectively check that all invoices that were expected to be
	// canceled are indeed canceled.
	err := wait.NoError(canceled, testTimeout)
	require.NoError(t, err, "timeout checking invoice state")

	// Finally stop the registry.
	require.NoError(t, registry.Stop(), "failed to stop invoice registry")
}

// testOldInvoiceRemovalOnStart tests that we'll attempt to remove old canceled
// invoices upon start while keeping all settled ones.
func testOldInvoiceRemovalOnStart(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	idb, testClock := makeDB(t)

	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta:        testFinalCltvRejectDelta,
		Clock:                       testClock,
		GcCanceledInvoicesOnStartup: true,
		HtlcInterceptor:             &invpkg.MockHtlcModifier{},
	}

	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		cfg.Clock, 0, uint32(testCurrentHeight), nil, newMockNotifier(),
	)
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	// First prefill the Channel DB with some pre-existing expired invoices.
	const numExpired = 5
	const numPending = 0
	existingInvoices := generateInvoiceExpiryTestData(
		t, testTime, 0, numExpired, numPending,
	)

	ctxb := t.Context()

	i := 0
	for paymentHash, invoice := range existingInvoices.expiredInvoices {
		// Mark half of the invoices as settled, the other half as
		// canceled.
		if i%2 == 0 {
			invoice.State = invpkg.ContractSettled
		} else {
			invoice.State = invpkg.ContractCanceled
		}

		_, err := idb.AddInvoice(ctxb, invoice, paymentHash)
		require.NoError(t, err)
		i++
	}

	// Collect all settled invoices for our expectation set.
	var expected []invpkg.Invoice

	// Perform a scan query to collect all invoices.
	query := invpkg.InvoiceQuery{
		IndexOffset:    0,
		NumMaxInvoices: math.MaxUint64,
	}

	response, err := idb.QueryInvoices(ctxb, query)
	require.NoError(t, err)

	// Save all settled invoices for our expectation set.
	for _, invoice := range response.Invoices {
		if invoice.State == invpkg.ContractSettled {
			expected = append(expected, invoice)
		}
	}

	// Start the registry which should collect and delete all canceled
	// invoices upon start.
	err = registry.Start()
	require.NoError(t, err, "cannot start the registry")

	// Perform a scan query to collect all invoices.
	response, err = idb.QueryInvoices(ctxb, query)
	require.NoError(t, err)

	// Check that we really only kept the settled invoices after the
	// registry start.
	require.Equal(t, expected, response.Invoices)
}

// testHeightExpiryWithRegistry tests our height-based invoice expiry for
// invoices paid with single and multiple htlcs, testing the case where the
// invoice is settled before expiry (and thus not canceled), and the case
// where the invoice is expired.
func testHeightExpiryWithRegistry(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	t.Run("single shot settled before expiry", func(t *testing.T) {
		testHeightExpiryWithRegistryImpl(t, 1, true, makeDB)
	})

	t.Run("single shot expires", func(t *testing.T) {
		testHeightExpiryWithRegistryImpl(t, 1, false, makeDB)
	})

	t.Run("mpp settled before expiry", func(t *testing.T) {
		testHeightExpiryWithRegistryImpl(t, 2, true, makeDB)
	})

	t.Run("mpp expires", func(t *testing.T) {
		testHeightExpiryWithRegistryImpl(t, 2, false, makeDB)
	})
}

func testHeightExpiryWithRegistryImpl(t *testing.T, numParts int, settle bool,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	ctx := newTestContext(t, nil, makeDB)

	require.Greater(t, numParts, 0, "test requires at least one part")

	// Add a hold invoice, we set a non-nil payment request so that this
	// invoice is not considered a keysend by the expiry watcher.
	testInvoice := newInvoice(t, false, false)
	testInvoice.HodlInvoice = true
	testInvoice.PaymentRequest = []byte{1, 2, 3}

	ctxb := t.Context()
	_, err := ctx.registry.AddInvoice(
		ctxb, testInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	payLoad := testPayload
	if numParts > 1 {
		payLoad = &mockPayload{
			mpp: record.NewMPP(testInvoiceAmount, [32]byte{}),
		}
	}

	htlcAmt := testInvoice.Terms.Value / lnwire.MilliSatoshi(numParts)
	hodlChan := make(chan interface{}, numParts)
	for i := 0; i < numParts; i++ {
		// We bump our expiry height for each htlc so that we can test
		// that the lowest expiry height is used.
		expiry := testHtlcExpiry + uint32(i)

		resolution, err := ctx.registry.NotifyExitHopHtlc(
			testInvoicePaymentHash, htlcAmt, expiry,
			testCurrentHeight, getCircuitKey(uint64(i)), hodlChan,
			nil, payLoad,
		)
		require.NoError(t, err)
		require.Nil(t, resolution, "did not expect direct resolution")
	}

	require.Eventually(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return inv.State == invpkg.ContractAccepted
	}, time.Second, time.Millisecond*100)

	// Now that we've added our htlc(s), we tick our test clock to our
	// invoice expiry time. We don't expect the invoice to be canceled
	// based on its expiry time now that we have active htlcs.
	ctx.clock.SetTime(
		testInvoice.CreationDate.Add(testInvoice.Terms.Expiry + 1),
	)

	// The expiry watcher loop takes some time to process the new clock
	// time. We mine the block before our expiry height, our mock will block
	// until the expiry watcher consumes this height, so we can be sure
	// that the expiry loop has run at least once after this block is
	// consumed.
	ctx.notifier.blockChan <- &chainntnfs.BlockEpoch{
		Height: int32(testHtlcExpiry - 1),
	}

	// If we want to settle our invoice in this test, we do so now.
	if settle {
		err = ctx.registry.SettleHodlInvoice(ctxb, testInvoicePreimage)
		require.NoError(t, err)

		for i := 0; i < numParts; i++ {
			resolution, _ := (<-hodlChan).(invpkg.HtlcResolution)
			require.NotNil(t, resolution)
			settleResolution := checkSettleResolution(
				t, resolution, testInvoicePreimage,
			)
			outcome := settleResolution.Outcome
			require.Equal(t, invpkg.ResultSettled, outcome)
		}
	}

	// Now we mine our htlc's expiry height.
	ctx.notifier.blockChan <- &chainntnfs.BlockEpoch{
		Height: int32(testHtlcExpiry),
	}

	// If we did not settle the invoice before its expiry, we now expect
	// a cancellation.
	expectedState := invpkg.ContractSettled
	if !settle {
		expectedState = invpkg.ContractCanceled

		htlcResolution, _ := (<-hodlChan).(invpkg.HtlcResolution)
		require.NotNil(t, htlcResolution)
		checkFailResolution(
			t, htlcResolution, invpkg.ResultCanceled,
		)
	}

	// Finally, lookup the invoice and assert that we have the state we
	// expect.
	inv, err := ctx.registry.LookupInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err)
	require.Equal(t, expectedState, inv.State, "expected "+
		"hold invoice: %v, got: %v", expectedState, inv.State)
}

// testMultipleSetHeightExpiry pays a hold invoice with two mpp sets, testing
// that the invoice expiry watcher only uses the expiry height of the second,
// successful set to cancel the invoice, and does not cancel early using the
// expiry height of the first set that was canceled back due to mpp timeout.
func testMultipleSetHeightExpiry(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	ctx := newTestContext(t, nil, makeDB)

	// Add a hold invoice.
	testInvoice := newInvoice(t, true, false)

	ctxb := t.Context()
	_, err := ctx.registry.AddInvoice(
		ctxb, testInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	mppPayload := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, [32]byte{}),
	}

	// Send htlc 1.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry, testCurrentHeight, getCircuitKey(10),
		hodlChan1, nil, mppPayload,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Simulate mpp timeout releasing htlc 1.
	ctx.clock.SetTime(testTime.Add(30 * time.Second))

	htlcResolution, _ := (<-hodlChan1).(invpkg.HtlcResolution)
	failResolution, ok := htlcResolution.(*invpkg.HtlcFailResolution)
	require.True(t, ok, "expected fail resolution, got: %T", resolution)
	require.Equal(t, invpkg.ResultMppTimeout, failResolution.Outcome,
		"expected MPP Timeout, got: %v", failResolution.Outcome)

	// Notify the expiry height for our first htlc. We don't expect the
	// invoice to be expired based on block height because the htlc set
	// was never completed.
	ctx.notifier.blockChan <- &chainntnfs.BlockEpoch{
		Height: int32(testHtlcExpiry),
	}

	// Now we will send a full set of htlcs for the invoice with a higher
	// expiry height. We expect the invoice to move into the accepted state.
	expiry := testHtlcExpiry + 5

	// Send htlc 2.
	hodlChan2 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2, expiry,
		testCurrentHeight, getCircuitKey(11), hodlChan2,
		nil, mppPayload,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Send htlc 3.
	hodlChan3 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2, expiry,
		testCurrentHeight, getCircuitKey(12), hodlChan3,
		nil, mppPayload,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Assert that we've reached an accepted state because the invoice has
	// been paid with a complete set.
	inv, err := ctx.registry.LookupInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractAccepted, inv.State, "expected "+
		"hold invoice accepted")

	// Now we will notify the expiry height for the new set of htlcs. We
	// expect the invoice to be canceled by the expiry watcher.
	ctx.notifier.blockChan <- &chainntnfs.BlockEpoch{
		Height: int32(expiry),
	}

	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return inv.State == invpkg.ContractCanceled
	}, testTimeout, time.Millisecond*100, "invoice not canceled")
}

// testSettleInvoicePaymentAddrRequired tests that if an incoming payment has
// an invoice that requires the payment addr bit to be set, and the incoming
// payment doesn't include an mpp payload, then the payment is rejected.
func testSettleInvoicePaymentAddrRequired(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	ctx := newTestContext(t, nil, makeDB)
	ctxb := t.Context()

	allSubscriptions, err := ctx.registry.SubscribeNotifications(ctxb, 0, 0)
	require.NoError(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	invoice := &invpkg.Invoice{
		Terms: invpkg.ContractTerm{
			PaymentPreimage: &testInvoicePreimage,
			Value:           testInvoiceAmount,
			Expiry:          time.Hour,
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadRequired,
					lnwire.PaymentAddrRequired,
				),
				lnwire.Features,
			),
		},
		CreationDate: testInvoiceCreationDate,
	}
	// Add the invoice, which requires the MPP payload to always be
	// included due to its set of feature bits.
	addIdx, err := ctx.registry.AddInvoice(
		ctxb, invoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, int(addIdx), 1)

	// We expect the open state to be sent to the single invoice subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.State != invpkg.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				newInvoice.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	hodlChan := make(chan interface{}, 1)

	// Now try to settle the invoice, the testPayload doesn't have any mpp
	// information, so it should be forced to the updateLegacy path then
	// fail as a required feature bit exists.
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, invoice.Terms.Value,
		uint32(testCurrentHeight)+testInvoiceCltvDelta-1,
		testCurrentHeight, getCircuitKey(10), hodlChan,
		nil, testPayload,
	)
	require.NoError(t, err)

	failResolution, ok := resolution.(*invpkg.HtlcFailResolution)
	if !ok {
		t.Fatalf("expected fail resolution, got: %T",
			resolution)
	}
	require.Equal(t, failResolution.AcceptHeight, testCurrentHeight)
	require.Equal(t, failResolution.Outcome, invpkg.ResultAddressMismatch)
}

// testSettleInvoicePaymentAddrRequiredOptionalGrace tests that if an invoice
// in the database has an optional payment addr required bit set, then we'll
// still allow it to be paid by an incoming HTLC that doesn't include the MPP
// payload. This ensures we don't break payment for any invoices in the wild.
func testSettleInvoicePaymentAddrRequiredOptionalGrace(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	ctx := newTestContext(t, nil, makeDB)
	ctxb := t.Context()

	allSubscriptions, err := ctx.registry.SubscribeNotifications(ctxb, 0, 0)
	require.NoError(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	invoice := &invpkg.Invoice{
		Terms: invpkg.ContractTerm{
			PaymentPreimage: &testInvoicePreimage,
			Value:           testInvoiceAmount,
			Expiry:          time.Hour,
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadRequired,
					lnwire.PaymentAddrOptional,
				),
				lnwire.Features,
			),
		},
		CreationDate: testInvoiceCreationDate,
	}

	// Add the invoice, which does not require the MPP payload to always be
	// included due to its set of feature bits.
	addIdx, err := ctx.registry.AddInvoice(
		ctxb, invoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)
	require.Equal(t, int(addIdx), 1)

	// We expect the open state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.State != invpkg.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				newInvoice.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We'll now attempt to settle the invoice as normal, this should work
	// no problem as we should allow these existing invoices to be settled.
	hodlChan := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoiceAmount, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(10), hodlChan,
		nil, testPayload,
	)
	require.NoError(t, err)

	settleResolution, ok := resolution.(*invpkg.HtlcSettleResolution)
	if !ok {
		t.Fatalf("expected settle resolution, got: %T",
			resolution)
	}
	require.Equal(t, settleResolution.Outcome, invpkg.ResultSettled)

	// We expect the settled state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != invpkg.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.State)
		}
		if update.AmtPaid != invoice.Terms.Value {
			t.Fatal("invoice AmtPaid incorrect")
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a settled notification to be sent out.
	select {
	case settledInvoice := <-allSubscriptions.SettledInvoices:
		if settledInvoice.State != invpkg.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				settledInvoice.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}
}

// testAMPWithoutMPPPayload asserts that we correctly reject an AMP HTLC that
// does not include an MPP record.
func testAMPWithoutMPPPayload(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	cfg := defaultRegistryConfig()
	cfg.AcceptAMP = true
	ctx := newTestContext(t, &cfg, makeDB)

	const (
		shardAmt = lnwire.MilliSatoshi(10)
		expiry   = uint32(testCurrentHeight + 20)
	)

	// Create payload with missing MPP field.
	payload := &mockPayload{
		amp: record.NewAMP([32]byte{}, [32]byte{}, 0),
	}

	hodlChan := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		lntypes.Hash{}, shardAmt, expiry, testCurrentHeight,
		getCircuitKey(uint64(10)), hodlChan, nil,
		payload,
	)
	require.NoError(t, err)

	// We should receive the ResultAmpError failure.
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultAmpError)
}

// testSpontaneousAmpPayment tests receiving a spontaneous AMP payment with both
// valid and invalid reconstructions.
func testSpontaneousAmpPayment(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	tests := []struct {
		name               string
		ampEnabled         bool
		failReconstruction bool
		numShards          int
	}{
		{
			name:               "enabled valid one shard",
			ampEnabled:         true,
			failReconstruction: false,
			numShards:          1,
		},
		{
			name:               "enabled valid multiple shards",
			ampEnabled:         true,
			failReconstruction: false,
			numShards:          3,
		},
		{
			name:               "enabled invalid one shard",
			ampEnabled:         true,
			failReconstruction: true,
			numShards:          1,
		},
		{
			name:               "enabled invalid multiple shards",
			ampEnabled:         true,
			failReconstruction: true,
			numShards:          3,
		},
		{
			name:               "disabled valid multiple shards",
			ampEnabled:         false,
			failReconstruction: false,
			numShards:          3,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testSpontaneousAmpPaymentImpl(
				t, test.ampEnabled, test.failReconstruction,
				test.numShards, makeDB,
			)
		})
	}
}

// testSpontaneousAmpPayment runs a specific spontaneous AMP test case.
func testSpontaneousAmpPaymentImpl(
	t *testing.T, ampEnabled, failReconstruction bool, numShards int,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()
	defer timeout()()

	cfg := defaultRegistryConfig()
	cfg.AcceptAMP = ampEnabled
	ctx := newTestContext(t, &cfg, makeDB)
	ctxb := t.Context()

	allSubscriptions, err := ctx.registry.SubscribeNotifications(ctxb, 0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	const (
		totalAmt = lnwire.MilliSatoshi(360)
		expiry   = uint32(testCurrentHeight + 20)
	)

	var (
		shardAmt = totalAmt / lnwire.MilliSatoshi(numShards)
		payAddr  [32]byte
		setID    [32]byte
	)
	_, err = rand.Read(payAddr[:])
	require.NoError(t, err)
	_, err = rand.Read(setID[:])
	require.NoError(t, err)

	var sharer amp.Sharer
	sharer, err = amp.NewSeedSharer()
	require.NoError(t, err)

	// Asserts that a new invoice is published on the NewInvoices channel.
	checkOpenSubscription := func() {
		t.Helper()
		newInvoice := <-allSubscriptions.NewInvoices
		require.Equal(t, newInvoice.State, invpkg.ContractOpen)
	}

	// Asserts that a settled invoice is published on the SettledInvoices
	// channel.
	checkSettleSubscription := func() {
		t.Helper()
		settledInvoice := <-allSubscriptions.SettledInvoices

		// Since this is an AMP invoice, the invoice state never
		// changes, but the AMP state should show that the setID has
		// been settled.
		htlcState := settledInvoice.AMPState[setID].State
		require.Equal(t, htlcState, invpkg.HtlcStateSettled)
	}

	// Asserts that no invoice is published on the SettledInvoices channel
	// w/in two seconds.
	checkNoSettleSubscription := func() {
		t.Helper()
		select {
		case <-allSubscriptions.SettledInvoices:
			t.Fatal("no settle ntfn expected")
		case <-time.After(2 * time.Second):
		}
	}

	// Record the hodl channels of all HTLCs but the last one, which
	// received its resolution directly from NotifyExitHopHtlc.
	hodlChans := make(map[lntypes.Preimage]chan interface{})
	for i := 0; i < numShards; i++ {
		isFinalShard := i == numShards-1

		hodlChan := make(chan interface{}, 1)

		var child *amp.Child
		if !isFinalShard {
			var left amp.Sharer
			left, sharer, err = sharer.Split()
			require.NoError(t, err)

			child = left.Child(uint32(i))

			// Only store the first numShards-1 hodlChans.
			hodlChans[child.Preimage] = hodlChan
		} else {
			child = sharer.Child(uint32(i))
		}

		// Send a blank share when the set should fail reconstruction,
		// otherwise send the derived share.
		var share [32]byte
		if !failReconstruction {
			share = child.Share
		}

		payload := &mockPayload{
			mpp: record.NewMPP(totalAmt, payAddr),
			amp: record.NewAMP(share, setID, uint32(i)),
		}

		resolution, err := ctx.registry.NotifyExitHopHtlc(
			child.Hash, shardAmt, expiry, testCurrentHeight,
			getCircuitKey(uint64(i)), hodlChan, nil,
			payload,
		)
		require.NoError(t, err)

		// When keysend is disabled all HTLC should fail with invoice
		// not found, since one is not inserted before executing
		// UpdateInvoice.
		if !ampEnabled {
			require.NotNil(t, resolution)
			checkFailResolution(
				t, resolution, invpkg.ResultInvoiceNotFound,
			)
			continue
		}

		// Check that resolutions are properly formed.
		if !isFinalShard {
			// Non-final shares should always return a nil
			// resolution, theirs will be delivered via the
			// hodlChan.
			require.Nil(t, resolution)
		} else {
			// The final share should receive a non-nil resolution.
			// Also assert that it is the proper type based on the
			// test case.
			require.NotNil(t, resolution)
			if failReconstruction {
				checkFailResolution(
					t, resolution,
					invpkg.ResultAmpReconstruction,
				)
			} else {
				checkSettleResolution(
					t, resolution, child.Preimage,
				)
			}
		}

		// Assert the behavior of the Open and Settle notifications.
		// There should always be an open (keysend is enabled) followed
		// by settle for valid AMP payments.
		//
		// NOTE: The cases are split in separate if conditions, rather
		// than else-if, to properly handle the case when there is only
		// one shard.
		if i == 0 {
			checkOpenSubscription()
		}
		if isFinalShard {
			if failReconstruction {
				checkNoSettleSubscription()
			} else {
				checkSettleSubscription()
			}
		}
	}

	// No need to check the hodl chans when keysend is not enabled.
	if !ampEnabled {
		return
	}

	// For the non-final hodl chans, assert that they receive the expected
	// failure or preimage.
	for preimage, hodlChan := range hodlChans {
		resolution, ok := (<-hodlChan).(invpkg.HtlcResolution)
		require.True(t, ok)
		require.NotNil(t, resolution)
		if failReconstruction {
			checkFailResolution(
				t, resolution, invpkg.ResultAmpReconstruction,
			)
		} else {
			checkSettleResolution(t, resolution, preimage)
		}
	}
}

// testFailPartialMPPPaymentExternal tests that the HTLC set is cancelled back
// as soon as the HTLC interceptor denies one of the HTLCs.
func testFailPartialMPPPaymentExternal(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	mockHtlcInterceptor := &invpkg.MockHtlcModifier{}
	cfg := defaultRegistryConfig()
	cfg.HtlcInterceptor = mockHtlcInterceptor
	ctx := newTestContext(t, &cfg, makeDB)

	// Add an invoice which we are going to pay via a MPP set.
	testInvoice := newInvoice(t, false, false)

	ctxb := t.Context()
	_, err := ctx.registry.AddInvoice(
		ctxb, testInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	mppPayload := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, [32]byte{}),
	}

	// Send first HTLC which pays part of the invoice but keeps the invoice
	// in an open state because the amount is less than the invoice amount.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/3,
		testHtlcExpiry, testCurrentHeight, getCircuitKey(1),
		hodlChan1, nil, mppPayload,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Register the expected response from the interceptor so that the
	// whole HTLC set is cancelled.
	expectedResponse := invpkg.HtlcModifyResponse{
		CancelSet: true,
	}
	mockHtlcInterceptor.On("Intercept", mock.Anything, mock.Anything).
		Return(nil, expectedResponse)

	// Send htlc 2. We expect the HTLC to be cancelled because the
	// interceptor will deny it.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry, testCurrentHeight, getCircuitKey(2), nil,
		nil, mppPayload,
	)
	require.NoError(t, err)
	failResolution, ok := resolution.(*invpkg.HtlcFailResolution)
	require.True(t, ok, "expected fail resolution, got: %T", resolution)

	// Make sure the resolution includes the custom error msg.
	require.Equal(t, invpkg.ExternalValidationFailed,
		failResolution.Outcome, "expected ExternalValidationFailed, "+
			"got: %v", failResolution.Outcome)

	// Expect HLTC 1 also to be cancelled because it is part of the cancel
	// set and the interceptor cancelled the whole set after receiving the
	// second HTLC.
	select {
	case resolution := <-hodlChan1:
		htlcResolution, _ := resolution.(invpkg.HtlcResolution)
		failResolution, ok = htlcResolution.(*invpkg.HtlcFailResolution)
		require.True(
			t, ok, "expected fail resolution, got: %T",
			htlcResolution,
		)
		require.Equal(
			t, invpkg.ExternalValidationFailed,
			failResolution.Outcome, "expected "+
				"ExternalValidationFailed, got: %v",
			failResolution.Outcome,
		)

	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for HTLC resolution")
	}

	// Assert that the invoice is still open.
	inv, err := ctx.registry.LookupInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractOpen, inv.State, "expected "+
		"OPEN invoice")

	// Now let the invoice expire.
	currentTime := ctx.clock.Now()
	ctx.clock.SetTime(currentTime.Add(61 * time.Minute))

	// Make sure the invoices changes to the canceled state.
	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return inv.State == invpkg.ContractCanceled
	}, testTimeout, time.Millisecond*100, "invoice not canceled")

	// Fetch the invoice again and compare the number of cancelled HTLCs.
	inv, err = ctx.registry.LookupInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	// Make sure all HTLCs are in the canceled state which in our case is
	// only the first one because the second HTLC was never added to the
	// invoice registry in the first place.
	require.Len(t, inv.Htlcs, 1)
	require.Equal(
		t, invpkg.HtlcStateCanceled, inv.Htlcs[getCircuitKey(1)].State,
	)
}

// testFailPartialAMPPayment tests the MPP timeout logic for AMP invoices. It
// makes sure that all HTLCs are cancelled if the full invoice amount is not
// received. Moreover it points out some TODOs to make AMP invoices more robust.
func testFailPartialAMPPayment(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	ctx := newTestContext(t, nil, makeDB)
	ctxb := t.Context()

	const (
		expiry    = uint32(testCurrentHeight + 20)
		numShards = 4
	)

	var (
		shardAmt = testInvoiceAmount / lnwire.MilliSatoshi(numShards)
		setID    [32]byte
		payAddr  [32]byte
	)
	_, err := rand.Read(payAddr[:])
	require.NoError(t, err)

	// Create an AMP invoice we are going to pay via a multi-part payment.
	ampInvoice := newInvoice(t, false, true)

	// An AMP invoice is referenced by the payment address.
	ampInvoice.Terms.PaymentAddr = payAddr

	_, err = ctx.registry.AddInvoice(
		ctxb, ampInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	// Generate a random setID for the HTLCs.
	_, err = rand.Read(setID[:])
	require.NoError(t, err)

	htlcPayload1 := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, payAddr),
		// We are not interested in settling the AMP HTLC so we don't
		// use valid shares.
		amp: record.NewAMP([32]byte{1}, setID, 1),
	}

	// Send first HTLC which pays part of the invoice.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		lntypes.Hash{1}, shardAmt, expiry, testCurrentHeight,
		getCircuitKey(1), hodlChan1, nil, htlcPayload1,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	htlcPayload2 := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, payAddr),
		// We are not interested in settling the AMP HTLC so we don't
		// use valid shares.
		amp: record.NewAMP([32]byte{2}, setID, 2),
	}

	// Send htlc 2 which should be added to the invoice as expected.
	hodlChan2 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		lntypes.Hash{2}, shardAmt, expiry, testCurrentHeight,
		getCircuitKey(2), hodlChan2, nil, htlcPayload2,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Now time-out the HTLCs. The HoldDuration is 30 seconds after the
	// HTLC will be cancelled.
	currentTime := ctx.clock.Now()
	ctx.clock.SetTime(currentTime.Add(35 * time.Second))

	// Expect HLTC 1 to be canceled via the MPPTimeout fail resolution.
	select {
	case resolution := <-hodlChan1:
		htlcResolution, _ := resolution.(invpkg.HtlcResolution)
		failRes, ok := htlcResolution.(*invpkg.HtlcFailResolution)
		require.True(
			t, ok, "expected fail resolution, got: %T", resolution,
		)
		require.Equal(
			t, invpkg.ResultMppTimeout, failRes.Outcome,
			"expected MPPTimeout, got: %v", failRes.Outcome,
		)

	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for HTLC resolution")
	}

	// Expect HLTC 2 to be canceled via the MPPTimeout fail resolution.
	select {
	case resolution := <-hodlChan2:
		htlcResolution, _ := resolution.(invpkg.HtlcResolution)
		failRes, ok := htlcResolution.(*invpkg.HtlcFailResolution)
		require.True(
			t, ok, "expected fail resolution, got: %T", resolution,
		)
		require.Equal(
			t, invpkg.ResultMppTimeout, failRes.Outcome,
			"expected MPPTimeout, got: %v", failRes.Outcome,
		)

	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for HTLC resolution")
	}

	// The AMP invoice should still be open.
	inv, err := ctx.registry.LookupInvoice(ctxb, testInvoicePaymentHash)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractOpen, inv.State, "expected "+
		"OPEN invoice")

	// Because one HTLC of the set was cancelled we expect the AMPState to
	// be set to canceled.
	ampState, ok := inv.AMPState[setID]
	require.True(t, ok, "expected AMPState to be set")
	require.Equal(t, invpkg.HtlcStateCanceled, ampState.State, "expected "+
		"AMPState CANCELED")

	// The following is a bug and should not be allowed because the sub
	// AMP invoice is already marked as canceled. However LND will accept
	// other HTLCs to the AMP sub-invoice.
	//
	// TODO(ziggie): Fix this bug.
	htlcPayload3 := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, payAddr),
		// We are not interested in settling the AMP HTLC so we don't
		// use valid shares.
		amp: record.NewAMP([32]byte{3}, setID, 3),
	}

	// Send htlc 3 which should be added to the invoice as expected.
	hodlChan3 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		lntypes.Hash{3}, shardAmt, expiry, testCurrentHeight,
		getCircuitKey(3), hodlChan3, nil, htlcPayload3,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// TODO(ziggie): This is a race condition between the invoice being
	// cancelled and the htlc being added to the invoice. If we do not wait
	// here until the HTLC is added to the invoice, the test might fail
	// because the HTLC will not be resolved.
	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return len(inv.Htlcs) == 3
	}, testTimeout, time.Millisecond*100, "HTLC 3 not added to invoice")

	// Now also let the invoice expire the invoice expiry is 1 hour.
	currentTime = ctx.clock.Now()
	ctx.clock.SetTime(currentTime.Add(1 * time.Minute))

	// Expect HLTC 3 to be canceled either via the cancelation of the
	// invoice or because the MPP timeout kicks in.
	select {
	case resolution := <-hodlChan3:
		htlcResolution, _ := resolution.(invpkg.HtlcResolution)
		failRes, ok := htlcResolution.(*invpkg.HtlcFailResolution)
		require.True(
			t, ok, "expected fail resolution, got: %T", resolution,
		)
		require.Equal(
			t, invpkg.ResultMppTimeout, failRes.Outcome,
			"expected MPPTimeout, got: %v", failRes.Outcome,
		)

	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for HTLC resolution")
	}

	// expire the invoice here.
	currentTime = ctx.clock.Now()
	ctx.clock.SetTime(currentTime.Add(61 * time.Minute))

	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return inv.State == invpkg.ContractCanceled
	}, testTimeout, time.Millisecond*100, "invoice not canceled")

	// Fetch the invoice again and compare the number of cancelled HTLCs.
	inv, err = ctx.registry.LookupInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	// Make sure all HTLCs are in the cancelled state.
	require.Len(t, inv.Htlcs, 3)
	for _, htlc := range inv.Htlcs {
		require.Equal(t, invpkg.HtlcStateCanceled, htlc.State,
			"expected HTLC to be canceled")
	}
}

// testCancelAMPInvoicePendingHTLCs tests the case where an AMP invoice is
// canceled and the remaining HTLCs are also canceled so that no HTLCs are left
// in the accepted state.
func testCancelAMPInvoicePendingHTLCs(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	ctx := newTestContext(t, nil, makeDB)
	ctxb := t.Context()

	const (
		expiry    = uint32(testCurrentHeight + 20)
		numShards = 4
	)

	var (
		shardAmt = testInvoiceAmount / lnwire.MilliSatoshi(numShards)
		payAddr  [32]byte
	)
	_, err := rand.Read(payAddr[:])
	require.NoError(t, err)

	// Create an AMP invoice we are going to pay via a multi-part payment.
	ampInvoice := newInvoice(t, false, true)

	// An AMP invoice is referenced by the payment address.
	ampInvoice.Terms.PaymentAddr = payAddr

	_, err = ctx.registry.AddInvoice(
		ctxb, ampInvoice, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	htlcPayloadSet1 := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, payAddr),
		// We are not interested in settling the AMP HTLC so we don't
		// use valid shares.
		amp: record.NewAMP([32]byte{1}, [32]byte{1}, 1),
	}

	// Send first HTLC which pays part of the invoice.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		lntypes.Hash{1}, shardAmt, expiry, testCurrentHeight,
		getCircuitKey(1), hodlChan1, nil, htlcPayloadSet1,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	htlcPayloadSet2 := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmount, payAddr),
		// We are not interested in settling the AMP HTLC so we don't
		// use valid shares.
		amp: record.NewAMP([32]byte{2}, [32]byte{2}, 1),
	}

	// Send htlc 2 which should be added to the invoice as expected.
	hodlChan2 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		lntypes.Hash{2}, shardAmt, expiry, testCurrentHeight,
		getCircuitKey(2), hodlChan2, nil, htlcPayloadSet2,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return len(inv.Htlcs) == 2
	}, testTimeout, time.Millisecond*100, "HTLCs not added to invoice")

	// expire the invoice here.
	ctx.clock.SetTime(testTime.Add(65 * time.Minute))

	// Expect HLTC 1 to be canceled via the MPPTimeout fail resolution.
	select {
	case resolution := <-hodlChan1:
		htlcResolution, _ := resolution.(invpkg.HtlcResolution)
		_, ok := htlcResolution.(*invpkg.HtlcFailResolution)
		require.True(
			t, ok, "expected fail resolution, got: %T", resolution,
		)

	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for HTLC resolution")
	}

	// Expect HLTC 2 to be canceled via the MPPTimeout fail resolution.
	select {
	case resolution := <-hodlChan2:
		htlcResolution, _ := resolution.(invpkg.HtlcResolution)
		_, ok := htlcResolution.(*invpkg.HtlcFailResolution)
		require.True(
			t, ok, "expected fail resolution, got: %T", resolution,
		)

	case <-time.After(testTimeout):
		t.Fatal("timeout waiting for HTLC resolution")
	}

	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(
			ctxb, testInvoicePaymentHash,
		)
		require.NoError(t, err)

		return inv.State == invpkg.ContractCanceled
	}, testTimeout, time.Millisecond*100, "invoice not canceled")

	// Fetch the invoice again and compare the number of cancelled HTLCs.
	inv, err := ctx.registry.LookupInvoice(
		ctxb, testInvoicePaymentHash,
	)
	require.NoError(t, err)

	// Make sure all HTLCs are in the cancelled state.
	require.Len(t, inv.Htlcs, 2)
	for _, htlc := range inv.Htlcs {
		require.Equal(t, invpkg.HtlcStateCanceled, htlc.State,
			"expected HTLC to be canceled")
	}
}
