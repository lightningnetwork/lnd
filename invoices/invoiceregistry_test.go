package invoices

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
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
	event, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value,
		uint32(testCurrentHeight)+testInvoiceCltvDelta-1,
		testCurrentHeight, getCircuitKey(10), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if event.Preimage != nil {
		t.Fatal("expected cancel event")
	}
	if event.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, event.AcceptHeight)
	}

	// Settle invoice with a slightly higher amount.
	amtPaid := lnwire.MilliSatoshi(100500)
	_, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatal(err)
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
	event, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("unexpected NotifyExitHopHtlc error: %v", err)
	}
	if event.Preimage == nil {
		t.Fatal("expected settle event")
	}

	// Try to settle again with a new higher-valued htlc. This payment
	// should also be accepted, to prevent any change in behaviour for a
	// paid invoice that may open up a probe vector.
	event, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid+600, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(1), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("unexpected NotifyExitHopHtlc error: %v", err)
	}
	if event.Preimage == nil {
		t.Fatal("expected settle event")
	}

	// Try to settle again with a lower amount. This should fail just as it
	// would have failed if it were the first payment.
	event, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid-600, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(2), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("unexpected NotifyExitHopHtlc error: %v", err)
	}
	if event.Preimage != nil {
		t.Fatal("expected cancel event")
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
		t.Fatal("unexpected event")
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
	// result in a cancel event.
	hodlChan := make(chan interface{})
	event, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amt, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatal("expected settlement of a canceled invoice to succeed")
	}

	if event.Preimage != nil {
		t.Fatal("expected cancel hodl event")
	}
	if event.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, event.AcceptHeight)
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
	}
	registry := NewRegistry(cdb, &cfg)

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
	event, err := registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if event != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test idempotency.
	event, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if event != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test replay at a higher height. We expect the same result because it
	// is a replay.
	event, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight+10,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if event != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Test a new htlc coming in that doesn't meet the final cltv delta
	// requirement. It should be rejected.
	event, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, 1, testCurrentHeight,
		getCircuitKey(1), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if event == nil || event.Preimage != nil {
		t.Fatalf("expected htlc to be canceled")
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

	hodlEvent := (<-hodlChan).(HodlEvent)
	if *hodlEvent.Preimage != testInvoicePreimage {
		t.Fatal("unexpected preimage in hodl event")
	}
	if hodlEvent.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, event.AcceptHeight)
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
	defer timeout()

	cdb, cleanup, err := newTestChannelDB()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Instantiate and start the invoice ctx.registry.
	cfg := RegistryConfig{
		FinalCltvRejectDelta: testFinalCltvRejectDelta,
	}
	registry := NewRegistry(cdb, &cfg)

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
	event, err := registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if event != nil {
		t.Fatalf("expected htlc to be held")
	}

	// Cancel invoice.
	err = registry.CancelInvoice(testInvoicePaymentHash)
	if err != nil {
		t.Fatal("cancel invoice failed")
	}

	hodlEvent := (<-hodlChan).(HodlEvent)
	if hodlEvent.Preimage != nil {
		t.Fatal("expected cancel hodl event")
	}

	// Offering the same htlc again at a higher height should still result
	// in a rejection. The accept height is expected to be the original
	// accept height.
	event, err = registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amtPaid, testHtlcExpiry, testCurrentHeight+1,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != nil {
		t.Fatalf("expected settle to succeed but got %v", err)
	}
	if event.Preimage != nil {
		t.Fatalf("expected htlc to be canceled")
	}
	if event.AcceptHeight != testCurrentHeight {
		t.Fatalf("expected acceptHeight %v, but got %v",
			testCurrentHeight, event.AcceptHeight)
	}
}

// TestUnknownInvoice tests that invoice registry returns an error when the
// invoice is unknown. This is to guard against returning a cancel hodl event
// for forwarded htlcs. In the link, NotifyExitHopHtlc is only called if we are
// the exit hop, but in htlcIncomingContestResolver it is called with forwarded
// htlc hashes as well.
func TestUnknownInvoice(t *testing.T) {
	ctx := newTestContext(t)
	defer ctx.cleanup()

	// Notify arrival of a new htlc paying to this invoice. This should
	// succeed.
	hodlChan := make(chan interface{})
	amt := lnwire.MilliSatoshi(100000)
	_, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, amt, testHtlcExpiry, testCurrentHeight,
		getCircuitKey(0), hodlChan, testPayload,
	)
	if err != channeldb.ErrInvoiceNotFound {
		t.Fatal("expected invoice not found error")
	}
}

// TestSettleMpp tests settling of an invoice with multiple partial payments.
func TestSettleMpp(t *testing.T) {
	defer timeout()

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
	event, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(10), hodlChan1, mppPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if event != nil {
		t.Fatal("expected no direct resolution")
	}

	// Simulate mpp timeout releasing htlc 1.
	ctx.clock.SetTime(testTime.Add(30 * time.Second))

	hodlEvent := (<-hodlChan1).(HodlEvent)
	if hodlEvent.Preimage != nil {
		t.Fatal("expected cancel event")
	}

	// Send htlc 2.
	hodlChan2 := make(chan interface{}, 1)
	event, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(11), hodlChan2, mppPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if event != nil {
		t.Fatal("expected no direct resolution")
	}

	// Send htlc 3.
	hodlChan3 := make(chan interface{}, 1)
	event, err = ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoice.Terms.Value/2,
		testHtlcExpiry,
		testCurrentHeight, getCircuitKey(12), hodlChan3, mppPayload,
	)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil {
		t.Fatal("expected a settle event")
	}

	// Check that settled amount is equal to the sum of values of the htlcs
	// 0 and 1.
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
