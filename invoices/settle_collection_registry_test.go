package invoices_test

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// testPendingSettleMixedRecovery checks that a released failure survives a
// registry restart without prematurely canceling or crediting the payment.
func testPendingSettleMixedRecovery(t *testing.T,
	makeDB func(t *testing.T) (invpkg.InvoiceDB, *clock.TestClock)) {

	t.Parallel()

	// Arrange a released two-shard payment on the real selected backend,
	// keeping its database and clock across a later registry restart.
	db, testClock := makeDB(t)
	invoice := newInvoice(t, false, false)
	_, err := db.AddInvoice(t.Context(), invoice, testInvoicePaymentHash)
	require.NoError(t, err)
	ref := invpkg.InvoiceRefByHash(testInvoicePaymentHash)
	failedKey, successKey := getCircuitKey(0), getCircuitKey(1)
	htlcs := make(map[invpkg.CircuitKey]*invpkg.HtlcAcceptDesc)
	for _, key := range []invpkg.CircuitKey{failedKey, successKey} {
		htlcs[key] = &invpkg.HtlcAcceptDesc{
			Amt:           testInvoiceAmount / 2,
			CustomRecords: make(record.CustomSet),
		}
	}
	_, err = db.UpdateInvoice(t.Context(), ref, nil,
		func(inv *invpkg.Invoice) (*invpkg.InvoiceUpdateDesc, error) {
			return &invpkg.InvoiceUpdateDesc{
				UpdateType: invpkg.AddHTLCsUpdate,
				AddHtlcs:   htlcs,
				State: &invpkg.InvoiceStateUpdateDesc{
					NewState: invpkg.ContractPendingSettle,
					Preimage: inv.Terms.PaymentPreimage,
				},
			}, nil
		},
	)
	require.NoError(t, err)

	// makeRegistryDB reuses persistent invoice state after the registry
	// restarts, so recovery reads the actual stored failure.
	makeRegistryDB := func(*testing.T) (invpkg.InvoiceDB,
		*clock.TestClock) {

		return db, testClock
	}
	// Act by persisting the first released failure before starting a new
	// registry; its sibling still needs a final collection outcome.
	_, err = db.UpdateInvoice(
		t.Context(), ref, nil, getFinalizeHtlcs(false, failedKey),
	)
	require.NoError(t, err)

	// Assert that one failure cannot finish a payment while another shard
	// can still be claimed, and that failure evidence survives in storage.
	stored, err := db.LookupInvoice(t.Context(), ref)
	require.NoError(t, err)
	require.Equal(t, invpkg.ContractPendingSettle, stored.State)
	require.Equal(t, invpkg.HtlcStateFailed, stored.Htlcs[failedKey].State)
	require.Equal(t, testInvoiceAmount/2, stored.AmtPaid)
	require.Zero(t, stored.SettleIndex)

	// Arrange fresh subscriptions after restart so the recovered pending
	// invoice and any later paid event are observed independently.
	ctx := newTestContext(t, nil, makeRegistryDB)
	all, err := ctx.registry.SubscribeNotifications(
		t.Context(), stored.AddIndex, 0,
	)
	require.NoError(t, err)
	defer all.Cancel()
	single, err := ctx.registry.SubscribeSingleInvoice(
		t.Context(), testInvoicePaymentHash,
	)
	require.NoError(t, err)
	defer single.Cancel()
	var update *invpkg.Invoice
	select {
	case update = <-single.Updates:
	case <-time.After(testTimeout):
		t.Fatal("missing initial invoice update")
	}
	require.Equal(t, invpkg.ContractPendingSettle, update.State)

	// Act by delivering the remaining success after restart; the stored
	// failure must still prevent successful completion of the payment.
	ctx.finalizeHtlc(ref, successKey, true)
	for update.State == invpkg.ContractPendingSettle {
		select {
		case update = <-single.Updates:
		case <-time.After(testTimeout):
			t.Fatal("missing mixed-outcome invoice update")
		}
	}
	// Assert that cancellation preserves the collected funds and both final
	// outcomes without allocating settlement metadata or issuing paid
	// events.
	require.Equal(t, invpkg.ContractCanceled, update.State)
	require.Equal(t, testInvoiceAmount/2, update.AmtPaid)
	require.Equal(
		t, invpkg.HtlcStateSettled, update.Htlcs[successKey].State,
	)
	require.Equal(t, invpkg.HtlcStateFailed, update.Htlcs[failedKey].State)
	require.Zero(t, update.SettleIndex)
	require.True(t, update.SettleDate.IsZero())
	select {
	case inv := <-all.SettledInvoices:
		t.Fatalf("partial collection reported as paid: %v", inv)
	case <-time.After(100 * time.Millisecond):
	}

	// Act by replaying the failed circuit through off-chain admission;
	// the final failure must remain a normal invoice rejection.
	resolution, err := ctx.registry.NotifyExitHopHtlc(
		testInvoicePaymentHash, testInvoiceAmount/2, testHtlcExpiry,
		testCurrentHeight, failedKey, nil, nil, testPayload,
	)
	require.NoError(t, err)
	// Assert the stored final failure returns a normal HTLC rejection,
	// preserving the canceled payment rather than reviving its preimage.
	checkFailResolution(t, resolution, invpkg.ResultReplayToCanceled)
}
