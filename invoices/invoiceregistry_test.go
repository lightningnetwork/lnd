package invoices

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

// TestSettleInvoice tests settling of an invoice and related notifications.
func TestSettleInvoice(t *testing.T) {
	ctx := newTestContext(t)
	defer ctx.cleanup()

	allSubscriptions := ctx.registry.SubscribeNotifications(0, 0)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	if subscription.hash != testInvoicePaymentHash {
		t.Fatalf("expected subscription for provided hash")
	}

	// Add the invoice.
	addIdx, err := ctx.registry.AddInvoice(testInvoice, testInvoicePaymentHash)
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
		if update.State != channeldb.ContractOpen {
			t.Fatalf("expected state ContractOpen, but got %v",
				update.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect a new invoice notification to be sent out.
	select {
	case newInvoice := <-allSubscriptions.NewInvoices:
		if newInvoice.State != channeldb.ContractOpen {
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
	if resolution.Preimage != nil {
		t.Fatal("expected cancel resolution")
	}
	if resolution.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, resolution.AcceptHeight)
	}
	if resolution.Outcome != ResultExpiryTooSoon {
		t.Fatalf("expected expiry too soon, got: %v",
			resolution.Outcome)
	}

	// Settle invoice with a slightly higher amount.
	amtPaid := lnwire.MilliSatoshi(100500)
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Outcome != ResultSettled {
		t.Fatalf("expected settled, got: %v", resolution.Outcome)
	}

	// We expect the settled state to be sent to the single invoice
	// subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != channeldb.ContractSettled {
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
		if settledInvoice.State != channeldb.ContractSettled {
			t.Fatalf("expected state ContractOpen, but got %v",
				settledInvoice.State)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// Try to settle again with the same htlc id. We need this idempotent
	// behaviour after a restart.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("unexpected NotifyExitHopHtlc error: %v", err)
	}
	if resolution.Preimage == nil {
		t.Fatal("expected settle resolution")
	}
	if resolution.Outcome != ResultReplayToSettled {
		t.Fatalf("expected replay settled, got: %v",
			resolution.Outcome)
	}

	// Try to settle again with a new higher-valued htlc. This payment
	// should also be accepted, to prevent any change in behaviour for a
	// paid invoice that may open up a probe vector.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid+600, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(1), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("unexpected NotifyExitHopHtlc error: %v", err)
	}
	if resolution.Preimage == nil {
		t.Fatal("expected settle resolution")
	}
	if resolution.Outcome != ResultDuplicateToSettled {
		t.Fatalf("expected duplicate settled, got: %v",
			resolution.Outcome)
	}

	// Try to settle again with a lower amount. This should fail just as it
	// would have failed if it were the first payment.
	resolution, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid-600, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(2), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("unexpected NotifyExitHopHtlc error: %v", err)
	}
	if resolution.Preimage != nil {
		t.Fatal("expected cancel resolution")
	}
	if resolution.Outcome != ResultAmountTooLow {
		t.Fatalf("expected amount too low, got: %v",
			resolution.Outcome)
	}

	// Check that settled amount is equal to the sum of values of the htlcs
	// 0 and 1.
	inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	if inv.AmtPaid != amtPaid+amtPaid+600 {
		t.Fatal("amount incorrect")
	}

	// Try to cancel.
	err = ctx.registry.CancelInvoice(testInvoicePaymentHash)
	if err != channeldb.ErrInvoiceAlreadySettled {
		t.Fatal("expected cancelation of a settled invoice to fail")
	}

	// As this is a direct sette, we expect nothing on the hodl chan.
	select {
	case <-hodlChan:
		t.Fatal("unexpected resolution")
	default:
	}
}

// TestCancelInvoice tests cancelation of an invoice and related notifications.
func TestCancelInvoice(t *testing.T) {
	ctx := newTestContext(t)
	defer ctx.cleanup()

	allSubscriptions := ctx.registry.SubscribeNotifications(0, 0)
	defer allSubscriptions.Cancel()

	// Try to cancel the not yet existing invoice. This should fail.
	err := ctx.registry.CancelInvoice(testInvoicePaymentHash)
	if err != channeldb.ErrInvoiceNotFound {
		t.Fatalf("expected ErrInvoiceNotFound, but got %v", err)
	}

	// Subscribe to the not yet existing invoice.
	subscription, err := ctx.registry.SubscribeSingleInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	if subscription.hash != testInvoicePaymentHash {
		t.Fatalf("expected subscription for provided hash")
	}

	// Add the invoice.
	amt := lnwire.MilliSatoshi(100000)
	_, err = ctx.registry.AddInvoice(testInvoice, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

	// We expect the open state to be sent to the single invoice subscriber.
	select {
	case update := <-subscription.Updates:
		if update.State != channeldb.ContractOpen {
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
		if newInvoice.State != channeldb.ContractOpen {
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
		if update.State != channeldb.ContractCanceled {
			t.Fatalf(
				"expected state ContractCanceled, but got %v",
				update.State,
			)
		}
	case <-time.After(testTimeout):
		t.Fatal("no update received")
	}

	// We expect no cancel notification to be sent to all invoice
	// subscribers (backwards compatibility).

	// Try to cancel again.
	err = ctx.registry.CancelInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal("expected cancelation of a canceled invoice to succeed")
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

	if resolution.Preimage != nil {
		t.Fatal("expected cancel htlc resolution")
	}
	if resolution.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, resolution.AcceptHeight)
	}
	if resolution.Outcome != ResultInvoiceAlreadyCanceled {
		t.Fatalf("expected invoice already canceled, got: %v",
			resolution.Outcome)
	}
}

// TestSettleHoldInvoice tests settling of a hold invoice and related
// notifications.
func TestSettleHoldInvoice(t *testing.T) {
	defer timeout()()

	cdb, cleanup, err := newTestChannelDB()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Instantiate and start the invoice ctx.registry.
	cfg := RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                clock.NewTestClock(testTime),
	}
	registry := NewRegistry(cdb, NewInvoiceExpiryWatcher(cfg.Clock), &cfg)

	err = registry.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Stop()

	allSubscriptions := registry.SubscribeNotifications(0, 0)
	defer allSubscriptions.Cancel()

	// Subscribe to the not yet existing invoice.
	subscription, err := registry.SubscribeSingleInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()

	if subscription.hash != testInvoicePaymentHash {
		t.Fatalf("expected subscription for provided hash")
	}

	// Add the invoice.
	_, err = registry.AddInvoice(testHodlInvoice, testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}

	// We expect the open state to be sent to the single invoice subscriber.
	update := <-subscription.Updates
	if update.State != channeldb.ContractOpen {
		t.Fatalf("expected state ContractOpen, but got %v",
			update.State)
	}

	// We expect a new invoice notification to be sent out.
	newInvoice := <-allSubscriptions.NewInvoices
	if newInvoice.State != channeldb.ContractOpen {
		t.Fatalf("expected state ContractOpen, but got %v",
			newInvoice.State)
	}

	// Use slightly higher amount for accept/settle.
	amtPaid := lnwire.MilliSatoshi(100500)

	hodlChan := make(chan interface{}, 1)

	// NotifyExitHopHtlc without a preimage present in the invoice registry
	// should be possible.
	resolution, err := registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test idempotency.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
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
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight+10,
		getCircuitKey(0), hodlChan, testPayload,
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
	if resolution == nil || resolution.Preimage != nil {
		t.Fatalf("expected htlc to be canceled")
	}
	if resolution.Outcome != ResultExpiryTooSoon {
		t.Fatalf("expected expiry too soon, got: %v",
			resolution.Outcome)
	}

	// We expect the accepted state to be sent to the single invoice
	// subscriber. For all invoice subscribers, we don't expect an update.
	// Those only get notified on settle.
	update = <-subscription.Updates
	if update.State != channeldb.ContractAccepted {
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

	htlcResolution := (<-hodlChan).(HtlcResolution)
	if *htlcResolution.Preimage != testInvoicePreimage {
		t.Fatal("unexpected preimage in hodl resolution")
	}
	if htlcResolution.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, resolution.AcceptHeight)
	}
	if htlcResolution.Outcome != ResultSettled {
		t.Fatalf("expected result settled, got: %v",
			htlcResolution.Outcome)
	}

	// We expect a settled notification to be sent out for both all and
	// single invoice subscribers.
	settledInvoice := <-allSubscriptions.SettledInvoices
	if settledInvoice.State != channeldb.ContractSettled {
		t.Fatalf("expected state ContractSettled, but got %v",
			settledInvoice.State)
	}
	if settledInvoice.AmtPaid != amtPaid {
		t.Fatalf("expected amount to be %v, but got %v",
			amtPaid, settledInvoice.AmtPaid)
	}

	update = <-subscription.Updates
	if update.State != channeldb.ContractSettled {
		t.Fatalf("expected state ContractSettled, but got %v",
			update.State)
	}

	// Idempotency.
	err = registry.SettleHodlInvoice(testInvoicePreimage)
	if err != channeldb.ErrInvoiceAlreadySettled {
		t.Fatalf("expected ErrInvoiceAlreadySettled but got %v", err)
	}

	// Try to cancel.
	err = registry.CancelInvoice(testInvoicePaymentHash)
	if err == nil {
		t.Fatal("expected cancelation of a settled invoice to fail")
	}
}

// TestCancelHoldInvoice tests canceling of a hold invoice and related
// notifications.
func TestCancelHoldInvoice(t *testing.T) {
	defer timeout()()

	cdb, cleanup, err := newTestChannelDB()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Instantiate and start the invoice ctx.registry.
	cfg := RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                clock.NewTestClock(testTime),
	}
	registry := NewRegistry(cdb, NewInvoiceExpiryWatcher(cfg.Clock), &cfg)

	err = registry.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Stop()

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
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
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

	htlcResolution := (<-hodlChan).(HtlcResolution)
	if htlcResolution.Preimage != nil {
		t.Fatal("expected cancel htlc resolution")
	}

	// Offering the same htlc again at a higher height should still result
	// in a rejection. The accept height is expected to be the original
	// accept height.
	resolution, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight+1,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if resolution.Preimage != nil {
		t.Fatalf("expected htlc to be canceled")
	}
	if resolution.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, resolution.AcceptHeight)
	}
	if resolution.Outcome != ResultReplayToCanceled {
		t.Fatalf("expected replay to canceled, got %v",
			resolution.Outcome)
	}
}

// TestUnknownInvoice tests that invoice registry returns an error when the
// invoice is unknown. This is to guard against returning a cancel htlc
// resolution for forwarded htlcs. In the link, NotifyExitHopHtlc is only called
// if we are the exit hop, but in htlcIncomingContestResolver it is called with
// forwarded htlc hashes as well.
func TestUnknownInvoice(t *testing.T) {
	ctx := newTestContext(t)
	defer ctx.cleanup()

	// Notify arrival of a new htlc paying to this invoice. This should
	// succeed.
	hodlChan := make(chan interface{})
	amt := lnwire.MilliSatoshi(100000)
	result, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amt, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatal("unexpected error")
	}
	if result.Outcome != ResultInvoiceNotFound {
		t.Fatalf("expected ResultInvoiceNotFound, got: %v",
			result.Outcome)
	}
}

// TestKeySend tests receiving a spontaneous payment with and without key send
// enabled.
func TestKeySend(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		testKeySend(t, true)
	})
	t.Run("disabled", func(t *testing.T) {
		testKeySend(t, false)
	})
}

// testKeySend is the inner test function that tests key send for a particular
// enabled state on the receiver end.
func testKeySend(t *testing.T, keySendEnabled bool) {
	defer timeout()()

	ctx := newTestContext(t)
	defer ctx.cleanup()

	ctx.registry.cfg.AcceptKeySend = keySendEnabled

	allSubscriptions := ctx.registry.SubscribeNotifications(0, 0)
	defer allSubscriptions.Cancel()

	hodlChan := make(chan interface{}, 1)

	amt := lnwire.MilliSatoshi(1000)
	expiry := uint32(testCurrentHeight + 20)

	// Create key for key send.
	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()

	// Try to settle invoice with an invalid key send htlc.
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

	// Expect a cancel resolution with the correct outcome.
	if resolution.Preimage != nil {
		t.Fatal("expected cancel resolution")
	}
	switch {
	case !keySendEnabled && resolution.Outcome != ResultInvoiceNotFound:
		t.Fatal("expected invoice not found outcome")

	case keySendEnabled && resolution.Outcome != ResultKeySendError:
		t.Fatal("expected key send error")
	}

	// Try to settle invoice with a valid key send htlc.
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

	// Expect a cancel resolution if key send is disabled.
	if !keySendEnabled {
		if resolution.Outcome != ResultInvoiceNotFound {
			t.Fatal("expected key send payment not to be accepted")
		}
		return
	}

	// Otherwise we expect no error and a settle resolution for the htlc.
	if resolution.Preimage == nil || *resolution.Preimage != preimage {
		t.Fatal("expected valid settle event")
	}

	// We expect a new invoice notification to be sent out.
	newInvoice := <-allSubscriptions.NewInvoices
	if newInvoice.State != channeldb.ContractOpen {
		t.Fatalf("expected state ContractOpen, but got %v",
			newInvoice.State)
	}

	// We expect a settled notification to be sent out.
	settledInvoice := <-allSubscriptions.SettledInvoices
	if settledInvoice.State != channeldb.ContractSettled {
		t.Fatalf("expected state ContractOpen, but got %v",
			settledInvoice.State)
	}
}

// TestMppPayment tests settling of an invoice with multiple partial payments.
// It covers the case where there is a mpp timeout before the whole invoice is
// paid and the case where the invoice is settled in time.
func TestMppPayment(t *testing.T) {
	defer timeout()()

	ctx := newTestContext(t)
	defer ctx.cleanup()

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

	htlcResolution := (<-hodlChan1).(HtlcResolution)
	if htlcResolution.Preimage != nil {
		t.Fatal("expected cancel resolution")
	}
	if htlcResolution.Outcome != ResultMppTimeout {
		t.Fatalf("expected mpp timeout, got: %v",
			htlcResolution.Outcome)
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
	if resolution == nil {
		t.Fatal("expected a settle resolution")
	}
	if resolution.Outcome != ResultSettled {
		t.Fatalf("expected result settled, got: %v",
			resolution.Outcome)
	}

	// Check that settled amount is equal to the sum of values of the htlcs
	// 2 and 3.
	inv, err := ctx.registry.LookupInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal(err)
	}
	if inv.State != channeldb.ContractSettled {
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

	cdb, cleanup, err := newTestChannelDB()
	defer cleanup()

	if err != nil {
		t.Fatal(err)
	}

	testClock := clock.NewTestClock(testTime)

	cfg := RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
		Clock:                testClock,
	}

	expiryWatcher := NewInvoiceExpiryWatcher(cfg.Clock)
	registry := NewRegistry(cdb, expiryWatcher, &cfg)

	// First prefill the Channel DB with some pre-existing invoices,
	// half of them still pending, half of them expired.
	const numExpired = 5
	const numPending = 5
	existingInvoices := generateInvoiceExpiryTestData(
		t, testTime, 0, numExpired, numPending,
	)

	var expectedCancellations []lntypes.Hash

	for paymentHash, expiredInvoice := range existingInvoices.expiredInvoices {
		if _, err := cdb.AddInvoice(expiredInvoice, paymentHash); err != nil {
			t.Fatalf("cannot add invoice to channel db: %v", err)
		}
		expectedCancellations = append(expectedCancellations, paymentHash)
	}

	for paymentHash, pendingInvoice := range existingInvoices.pendingInvoices {
		if _, err := cdb.AddInvoice(pendingInvoice, paymentHash); err != nil {
			t.Fatalf("cannot add invoice to channel db: %v", err)
		}
	}

	if err = registry.Start(); err != nil {
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
		invoicesThatWillCancel = append(invoicesThatWillCancel, paymentHash)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Check that they are really not canceled until before the clock is
	// advanced.
	for i := range invoicesThatWillCancel {
		invoice, err := registry.LookupInvoice(invoicesThatWillCancel[i])
		if err != nil {
			t.Fatalf("cannot find invoice: %v", err)
		}

		if invoice.State == channeldb.ContractCanceled {
			t.Fatalf("expected pending invoice, got canceled")
		}
	}

	// Fwd time 1 day.
	testClock.SetTime(testTime.Add(24 * time.Hour))

	// Give some time to the watcher to cancel everything.
	time.Sleep(500 * time.Millisecond)
	registry.Stop()

	// Create the expected cancellation set before the final check.
	expectedCancellations = append(
		expectedCancellations, invoicesThatWillCancel...,
	)

	// Retrospectively check that all invoices that were expected to be canceled
	// are indeed canceled.
	for i := range expectedCancellations {
		invoice, err := registry.LookupInvoice(expectedCancellations[i])
		if err != nil {
			t.Fatalf("cannot find invoice: %v", err)
		}

		if invoice.State != channeldb.ContractCanceled {
			t.Fatalf("expected canceled invoice, got: %v", invoice.State)
		}
	}
}
