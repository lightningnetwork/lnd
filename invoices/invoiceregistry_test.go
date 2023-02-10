package invoices_test

import (
	"crypto/rand"
	"math"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// TestSettleInvoice tests settling of an invoice and related notifications.
func TestSettleInvoice(t *testing.T) {
	ctx := newTestContext(t, nil)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		testInvoicePaymentHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice.
	addIdx, err := ctx.registry.AddInvoice(
		testInvoice, testInvoicePaymentHash,
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
		testCurrentHeight, getCircuitKey(10), hodlChan, testPayload,
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
		testPayload,
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
		testCurrentHeight, getCircuitKey(0), hodlChan, testPayload,
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
		testCurrentHeight, getCircuitKey(1), hodlChan, testPayload,
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
		testCurrentHeight, getCircuitKey(2), hodlChan, testPayload,
	)
	require.NoError(t, err, "unexpected NotifyExitHopHtlc error")
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultAmountTooLow)

	// Check that settled amount is equal to the sum of values of the htlcs
	// 0 and 1.
	inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	if inv.AmtPaid != amtPaid+amtPaid+600 {
		t.Fatalf("amount incorrect: expected %v got %v",
			amtPaid+amtPaid+600, inv.AmtPaid)
	}

	// Try to cancel.
	err = ctx.registry.CancelInvoice(testInvoicePaymentHash)
	require.ErrorIs(t, err, invpkg.ErrInvoiceAlreadySettled)

	// As this is a direct settle, we expect nothing on the hodl chan.
	select {
	case <-hodlChan:
		t.Fatal("unexpected resolution")
	default:
	}
}

func testCancelInvoice(t *testing.T, gc bool) {
	cfg := defaultRegistryConfig()

	// If set to true, then also delete the invoice from the DB after
	// cancellation.
	cfg.GcCanceledInvoicesOnTheFly = gc
	ctx := newTestContext(t, &cfg)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Try to cancel the not yet existing invoice. This should fail.
	err = ctx.registry.CancelInvoice(testInvoicePaymentHash)
	require.ErrorIs(t, err, invpkg.ErrInvoiceNotFound)

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		testInvoicePaymentHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice.
	amt := lnwire.MilliSatoshi(100000)
	_, err = ctx.registry.AddInvoice(testInvoice, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

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
	err = ctx.registry.CancelInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

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
			invpkg.InvoiceRefByHash(testInvoicePaymentHash),
		)
		require.Error(t, err)
	}

	// We expect no cancel notification to be sent to all invoice
	// subscribers (backwards compatibility).

	// Try to cancel again. Expect that we report ErrInvoiceNotFound if the
	// invoice has been garbage collected (since the invoice has been
	// deleted when it was canceled), and no error otherwise.
	err = ctx.registry.CancelInvoice(testInvoicePaymentHash)

	if gc {
		require.Error(t, err, invpkg.ErrInvoiceNotFound)
	} else {
		require.NoError(t, err)
	}

	// Notify arrival of a new htlc paying to this invoice. This should
	// result in a cancel resolution.
	hodlChan := make(chan interface{})
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amt, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
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

// TestCancelInvoice tests cancellation of an invoice and related notifications.
func TestCancelInvoice(t *testing.T) {
	// Test cancellation both with garbage collection (meaning that canceled
	// invoice will be deleted) and without (meaning it'll be kept).
	t.Run("garbage collect", func(t *testing.T) {
		testCancelInvoice(t, true)
	})

	t.Run("no garbage collect", func(t *testing.T) {
		testCancelInvoice(t, false)
	})
}

// TestSettleHoldInvoice tests settling of a hold invoice and related
// notifications.
func TestSettleHoldInvoice(t *testing.T) {
	defer timeout()()

	idb, err := newTestChannelDB(t, clock.NewTestClock(time.Time{}))
	if err != nil {
		t.Fatal(err)
	}

	// Instantiate and start the invoice ctx.registry.
	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                clock.NewTestClock(testTime),
	}

	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		cfg.Clock, 0, uint32(testCurrentHeight), nil, newMockNotifier(),
	)
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	err = registry.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Stop()

	allSubscriptions, err := registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := registry.SubscribeSingleInvoice(
		testInvoicePaymentHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice.
	_, err = registry.AddInvoice(testHodlInvoice, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

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
		testCurrentHeight, getCircuitKey(0), hodlChan, testPayload,
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
		testCurrentHeight, getCircuitKey(0), hodlChan, testPayload,
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
		testCurrentHeight+10, getCircuitKey(0), hodlChan, testPayload,
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
		getCircuitKey(1), hodlChan, testPayload,
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
	err = registry.SettleHodlInvoice(testInvoicePreimage)
	if err != nil {
		t.Fatal("expected set preimage to succeed")
	}

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
	err = registry.SettleHodlInvoice(testInvoicePreimage)
	require.ErrorIs(t, err, invpkg.ErrInvoiceAlreadySettled)

	// Try to cancel.
	err = registry.CancelInvoice(testInvoicePaymentHash)
	if err == nil {
		t.Fatal("expected cancellation of a settled invoice to fail")
	}
}

// TestCancelHoldInvoice tests canceling of a hold invoice and related
// notifications.
func TestCancelHoldInvoice(t *testing.T) {
	defer timeout()()

	testClock := clock.NewTestClock(testTime)
	idb, err := newTestChannelDB(t, testClock)
	require.NoError(t, err)

	// Instantiate and start the invoice ctx.registry.
	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                testClock,
	}
	expiryWatcher := invpkg.NewInvoiceExpiryWatcher(
		cfg.Clock, 0, uint32(testCurrentHeight), nil, newMockNotifier(),
	)
	registry := invpkg.NewRegistry(idb, expiryWatcher, &cfg)

	err = registry.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, registry.Stop())
	})

	// Add the invoice.
	_, err = registry.AddInvoice(testHodlInvoice, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

	amtPaid := lnwire.MilliSatoshi(100000)
	hodlChan := make(chan interface{}, 1)

	// NotifyExitHopHtlc without a preimage present in the invoice registry
	// should be possible.
	resolution, err := registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight, getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Cancel invoice.
	err = registry.CancelInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal("cancel invoice failed")
	}

	htlcResolution, _ := (<-hodlChan).(invpkg.HtlcResolution)
	require.NotNil(t, htlcResolution)
	checkFailResolution(t, htlcResolution, invpkg.ResultCanceled)

	// Offering the same htlc again at a higher height should still result
	// in a rejection. The accept height is expected to be the original
	// accept height.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry,
		testCurrentHeight+1, getCircuitKey(0), hodlChan, testPayload,
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

// TestUnknownInvoice tests that invoice registry returns an error when the
// invoice is unknown. This is to guard against returning a cancel htlc
// resolution for forwarded htlcs. In the link, NotifyExitHopHtlc is only called
// if we are the exit hop, but in htlcIncomingContestResolver it is called with
// forwarded htlc hashes as well.
func TestUnknownInvoice(t *testing.T) {
	ctx := newTestContext(t, nil)

	// Notify arrival of a new htlc paying to this invoice. This should
	// succeed.
	hodlChan := make(chan interface{})
	amt := lnwire.MilliSatoshi(100000)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amt, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatal("unexpected error")
	}
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultInvoiceNotFound)
}

// TestKeySend tests receiving a spontaneous payment with and without keysend
// enabled.
func TestKeySend(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		testKeySend(t, true)
	})
	t.Run("disabled", func(t *testing.T) {
		testKeySend(t, false)
	})
}

// testKeySend is the inner test function that tests keysend for a particular
// enabled state on the receiver end.
func testKeySend(t *testing.T, keySendEnabled bool) {
	defer timeout()()

	cfg := defaultRegistryConfig()
	cfg.AcceptKeySend = keySendEnabled
	ctx := newTestContext(t, &cfg)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
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
		invalidKeySendPayload,
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
		hash, amt, expiry,
		testCurrentHeight, getCircuitKey(10), hodlChan, keySendPayload,
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
		hash, amt, expiry,
		testCurrentHeight, getCircuitKey(10), hodlChan, keySendPayload,
	)
	require.Nil(t, err)
	checkSettleResolution(t, resolution, preimage)

	select {
	case <-allSubscriptions.NewInvoices:
		t.Fatalf("replayed keysend should not generate event")
	case <-time.After(time.Second):
	}

	// Finally, test that we can properly fulfill a second keysend payment
	// with a unique preiamge.
	preimage2 := lntypes.Preimage{1, 2, 3, 4}
	hash2 := preimage2.Hash()

	keySendPayload2 := &mockPayload{
		customRecords: map[uint64][]byte{
			record.KeySendType: preimage2[:],
		},
	}

	resolution, err = ctx.registry.NotifyExitHopHtlc(
		hash2, amt, expiry,
		testCurrentHeight, getCircuitKey(20), hodlChan, keySendPayload2,
	)
	require.Nil(t, err)

	checkSettleResolution(t, resolution, preimage2)
	checkSubscription()
}

// TestHoldKeysend tests receiving a spontaneous payment that is held.
func TestHoldKeysend(t *testing.T) {
	t.Run("settle", func(t *testing.T) {
		testHoldKeysend(t, false)
	})
	t.Run("timeout", func(t *testing.T) {
		testHoldKeysend(t, true)
	})
}

// testHoldKeysend is the inner test function that tests hold-keysend.
func testHoldKeysend(t *testing.T, timeoutKeysend bool) {
	defer timeout()()

	const holdDuration = time.Minute

	cfg := defaultRegistryConfig()
	cfg.AcceptKeySend = true
	cfg.KeysendHoldTime = holdDuration
	ctx := newTestContext(t, &cfg)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
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
		hash, amt, expiry,
		testCurrentHeight, getCircuitKey(10), hodlChan, keysendPayload,
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
	require.Nil(t, ctx.registry.SettleHodlInvoice(
		*newInvoice.Terms.PaymentPreimage,
	))

	// We expect a settled notification to be sent out.
	settledInvoice := <-allSubscriptions.SettledInvoices
	require.Equal(t, settledInvoice.State, invpkg.ContractSettled)
}

// TestMppPayment tests settling of an invoice with multiple partial payments.
// It covers the case where there is a mpp timeout before the whole invoice is
// paid and the case where the invoice is settled in time.
func TestMppPayment(t *testing.T) {
	defer timeout()()

	ctx := newTestContext(t, nil)

	// Add the invoice.
	_, err := ctx.registry.AddInvoice(testInvoice, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

	mppPayload := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmt, [32]byte{}),
	}

	// Send htlc 1.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(10), hodlChan1, mppPayload,
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
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(11), hodlChan2, mppPayload,
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
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(12), hodlChan3, mppPayload,
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
	inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	if inv.State != invpkg.ContractSettled {
		t.Fatal("expected invoice to be settled")
	}
	if inv.AmtPaid != testInvoice.Terms.Value {
		t.Fatalf("amount incorrect, expected %v but got %v",
			testInvoice.Terms.Value, inv.AmtPaid)
	}
}

// Tests that invoices are canceled after expiration.
func TestInvoiceExpiryWithRegistry(t *testing.T) {
	t.Parallel()

	testClock := clock.NewTestClock(testTime)
	idb, err := newTestChannelDB(t, testClock)
	require.NoError(t, err)

	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                testClock,
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

	var expectedCancellations []lntypes.Hash
	expiredInvoices := existingInvoices.expiredInvoices
	for paymentHash, expiredInvoice := range expiredInvoices {
		_, err := idb.AddInvoice(expiredInvoice, paymentHash)
		require.NoError(t, err)
		expectedCancellations = append(
			expectedCancellations, paymentHash,
		)
	}

	pendingInvoices := existingInvoices.pendingInvoices
	for paymentHash, pendingInvoice := range pendingInvoices {
		_, err := idb.AddInvoice(pendingInvoice, paymentHash)
		require.NoError(t, err)
	}

	if err := registry.Start(); err != nil {
		t.Fatalf("cannot start registry: %v", err)
	}

	// Now generate pending and invoices and add them to the registry while
	// it is up and running. We'll manipulate the clock to let them expire.
	newInvoices := generateInvoiceExpiryTestData(
		t, testTime, numExpired+numPending, 0, numPending,
	)

	var invoicesThatWillCancel []lntypes.Hash
	for paymentHash, pendingInvoice := range newInvoices.pendingInvoices {
		_, err := registry.AddInvoice(pendingInvoice, paymentHash)
		invoicesThatWillCancel = append(
			invoicesThatWillCancel, paymentHash,
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Check that they are really not canceled until before the clock is
	// advanced.
	for i := range invoicesThatWillCancel {
		invoice, err := registry.LookupInvoice(
			invoicesThatWillCancel[i],
		)
		if err != nil {
			t.Fatalf("cannot find invoice: %v", err)
		}

		if invoice.State == invpkg.ContractCanceled {
			t.Fatalf("expected pending invoice, got canceled")
		}
	}

	// Fwd time 1 day.
	testClock.SetTime(testTime.Add(24 * time.Hour))

	// Create the expected cancellation set before the final check.
	expectedCancellations = append(
		expectedCancellations, invoicesThatWillCancel...,
	)

	// canceled returns a bool to indicate whether all the invoices are
	// canceled.
	canceled := func() bool {
		for i := range expectedCancellations {
			invoice, err := registry.LookupInvoice(
				expectedCancellations[i],
			)
			require.NoError(t, err)

			if invoice.State != invpkg.ContractCanceled {
				return false
			}
		}

		return true
	}

	// Retrospectively check that all invoices that were expected to be
	// canceled are indeed canceled.
	require.Eventually(t, canceled, testTimeout, 10*time.Millisecond)

	// Finally stop the registry.
	require.NoError(t, registry.Stop(), "failed to stop invoice registry")
}

// TestOldInvoiceRemovalOnStart tests that we'll attempt to remove old canceled
// invoices upon start while keeping all settled ones.
func TestOldInvoiceRemovalOnStart(t *testing.T) {
	t.Parallel()

	testClock := clock.NewTestClock(testTime)
	idb, err := newTestChannelDB(t, testClock)
	require.NoError(t, err)

	cfg := invpkg.RegistryConfig{
		FinalCltvRejectDelta:        testFinalCltvRejectDelta,
		Clock:                       testClock,
		GcCanceledInvoicesOnStartup: true,
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

	i := 0
	for paymentHash, invoice := range existingInvoices.expiredInvoices {
		// Mark half of the invoices as settled, the other half as
		// canceled.
		if i%2 == 0 {
			invoice.State = invpkg.ContractSettled
		} else {
			invoice.State = invpkg.ContractCanceled
		}

		_, err := idb.AddInvoice(invoice, paymentHash)
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

	response, err := idb.QueryInvoices(query)
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
	response, err = idb.QueryInvoices(query)
	require.NoError(t, err)

	// Check that we really only kept the settled invoices after the
	// registry start.
	require.Equal(t, expected, response.Invoices)
}

// TestHeightExpiryWithRegistry tests our height-based invoice expiry for
// invoices paid with single and multiple htlcs, testing the case where the
// invoice is settled before expiry (and thus not canceled), and the case
// where the invoice is expired.
func TestHeightExpiryWithRegistry(t *testing.T) {
	t.Run("single shot settled before expiry", func(t *testing.T) {
		testHeightExpiryWithRegistry(t, 1, true)
	})

	t.Run("single shot expires", func(t *testing.T) {
		testHeightExpiryWithRegistry(t, 1, false)
	})

	t.Run("mpp settled before expiry", func(t *testing.T) {
		testHeightExpiryWithRegistry(t, 2, true)
	})

	t.Run("mpp expires", func(t *testing.T) {
		testHeightExpiryWithRegistry(t, 2, false)
	})
}

func testHeightExpiryWithRegistry(t *testing.T, numParts int, settle bool) {
	t.Parallel()
	defer timeout()()

	ctx := newTestContext(t, nil)

	require.Greater(t, numParts, 0, "test requires at least one part")

	// Add a hold invoice, we set a non-nil payment request so that this
	// invoice is not considered a keysend by the expiry watcher.
	invoice := *testInvoice
	invoice.HodlInvoice = true
	invoice.PaymentRequest = []byte{1, 2, 3}

	_, err := ctx.registry.AddInvoice(&invoice, testInvoicePaymentHash)
	require.NoError(t, err)

	payLoad := testPayload
	if numParts > 1 {
		payLoad = &mockPayload{
			mpp: record.NewMPP(testInvoiceAmt, [32]byte{}),
		}
	}

	htlcAmt := invoice.Terms.Value / lnwire.MilliSatoshi(numParts)
	hodlChan := make(chan interface{}, numParts)
	for i := 0; i < numParts; i++ {
		// We bump our expiry height for each htlc so that we can test
		// that the lowest expiry height is used.
		expiry := testHtlcExpiry + uint32(i)

		resolution, err := ctx.registry.NotifyExitHopHtlc(
			testInvoicePaymentHash, htlcAmt, expiry,
			testCurrentHeight, getCircuitKey(uint64(i)), hodlChan,
			payLoad,
		)
		require.NoError(t, err)
		require.Nil(t, resolution, "did not expect direct resolution")
	}

	require.Eventually(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
		require.NoError(t, err)

		return inv.State == invpkg.ContractAccepted
	}, time.Second, time.Millisecond*100)

	// Now that we've added our htlc(s), we tick our test clock to our
	// invoice expiry time. We don't expect the invoice to be canceled
	// based on its expiry time now that we have active htlcs.
	ctx.clock.SetTime(invoice.CreationDate.Add(invoice.Terms.Expiry + 1))

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
		err = ctx.registry.SettleHodlInvoice(testInvoicePreimage)
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
	inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
	require.NoError(t, err)
	require.Equal(t, expectedState, inv.State, "expected "+
		"hold invoice: %v, got: %v", expectedState, inv.State)
}

// TestMultipleSetHeightExpiry pays a hold invoice with two mpp sets, testing
// that the invoice expiry watcher only uses the expiry height of the second,
// successful set to cancel the invoice, and does not cancel early using the
// expiry height of the first set that was canceled back due to mpp timeout.
func TestMultipleSetHeightExpiry(t *testing.T) {
	t.Parallel()
	defer timeout()()

	ctx := newTestContext(t, nil)

	// Add a hold invoice.
	invoice := *testInvoice
	invoice.HodlInvoice = true

	_, err := ctx.registry.AddInvoice(&invoice, testInvoicePaymentHash)
	require.NoError(t, err)

	mppPayload := &mockPayload{
		mpp: record.NewMPP(testInvoiceAmt, [32]byte{}),
	}

	// Send htlc 1.
	hodlChan1 := make(chan interface{}, 1)
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, invoice.Terms.Value/2,
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(10), hodlChan1, mppPayload,
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
		testInvoicePaymentHash, invoice.Terms.Value/2, expiry,
		testCurrentHeight, getCircuitKey(11), hodlChan2, mppPayload,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Send htlc 3.
	hodlChan3 := make(chan interface{}, 1)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, invoice.Terms.Value/2, expiry,
		testCurrentHeight, getCircuitKey(12), hodlChan3, mppPayload,
	)
	require.NoError(t, err)
	require.Nil(t, resolution, "did not expect direct resolution")

	// Assert that we've reached an accepted state because the invoice has
	// been paid with a complete set.
	inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractAccepted, inv.State, "expected "+
		"hold invoice accepted")

	// Now we will notify the expiry height for the new set of htlcs. We
	// expect the invoice to be canceled by the expiry watcher.
	ctx.notifier.blockChan <- &chainntnfs.BlockEpoch{
		Height: int32(expiry),
	}

	require.Eventuallyf(t, func() bool {
		inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
		require.NoError(t, err)

		return inv.State == invpkg.ContractCanceled
	}, testTimeout, time.Millisecond*100, "invoice not canceled")
}

// TestSettleInvoicePaymentAddrRequired tests that if an incoming payment has
// an invoice that requires the payment addr bit to be set, and the incoming
// payment doesn't include an mpp payload, then the payment is rejected.
func TestSettleInvoicePaymentAddrRequired(t *testing.T) {
	t.Parallel()

	ctx := newTestContext(t, nil)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice, which requires the MPP payload to always be
	// included due to its set of feature bits.
	addIdx, err := ctx.registry.AddInvoice(
		testPayAddrReqInvoice, testInvoicePaymentHash,
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
		testInvoicePaymentHash, testInvoice.Terms.Value,
		uint32(testCurrentHeight)+testInvoiceCltvDelta-1,
		testCurrentHeight, getCircuitKey(10), hodlChan, testPayload,
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

// TestSettleInvoicePaymentAddrRequiredOptionalGrace tests that if an invoice
// in the database has an optional payment addr required bit set, then we'll
// still allow it to be paid by an incoming HTLC that doesn't include the MPP
// payload. This ensures we don't break payment for any invoices in the wild.
func TestSettleInvoicePaymentAddrRequiredOptionalGrace(t *testing.T) {
	t.Parallel()

	ctx := newTestContext(t, nil)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
	require.Nil(t, err)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(
		testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer subscription.Cancel()

	require.Equal(t, subscription.PayHash(), &testInvoicePaymentHash)

	// Add the invoice, which requires the MPP payload to always be
	// included due to its set of feature bits.
	addIdx, err := ctx.registry.AddInvoice(
		testPayAddrOptionalInvoice, testInvoicePaymentHash,
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
		testInvoicePaymentHash, testInvoiceAmt,
		testHtlcExpiry, testCurrentHeight,
		getCircuitKey(10), hodlChan, testPayload,
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
		if update.AmtPaid != testInvoice.Terms.Value {
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

// TestAMPWithoutMPPPayload asserts that we correctly reject an AMP HTLC that
// does not include an MPP record.
func TestAMPWithoutMPPPayload(t *testing.T) {
	defer timeout()()

	cfg := defaultRegistryConfig()
	cfg.AcceptAMP = true
	ctx := newTestContext(t, &cfg)

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
		lntypes.Hash{}, shardAmt, expiry,
		testCurrentHeight, getCircuitKey(uint64(10)), hodlChan,
		payload,
	)
	require.NoError(t, err)

	// We should receive the ResultAmpError failure.
	require.NotNil(t, resolution)
	checkFailResolution(t, resolution, invpkg.ResultAmpError)
}

// TestSpontaneousAmpPayment tests receiving a spontaneous AMP payment with both
// valid and invalid reconstructions.
func TestSpontaneousAmpPayment(t *testing.T) {
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
			testSpontaneousAmpPayment(
				t, test.ampEnabled, test.failReconstruction,
				test.numShards,
			)
		})
	}
}

// testSpontaneousAmpPayment runs a specific spontaneous AMP test case.
func testSpontaneousAmpPayment(
	t *testing.T, ampEnabled, failReconstruction bool, numShards int) {

	defer timeout()()

	cfg := defaultRegistryConfig()
	cfg.AcceptAMP = ampEnabled
	ctx := newTestContext(t, &cfg)

	allSubscriptions, err := ctx.registry.SubscribeNotifications(0, 0)
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
	// received its resolution directly from NotifyExistHopHtlc.
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
			child.Hash, shardAmt, expiry,
			testCurrentHeight, getCircuitKey(uint64(i)), hodlChan,
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
