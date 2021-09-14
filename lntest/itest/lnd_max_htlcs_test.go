package itest

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMaxHtlcPathfind tests the case where we try to send a payment over a
// channel where we have already reached the limit of the number of htlcs that
// we may add to the remote party's commitment. This test asserts that we do
// not attempt to use the full channel at all in our pathfinding.
func testMaxHtlcPathfind(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Setup a channel between Alice and Bob where Alice will only allow
	// Bob to add a maximum of 5 htlcs to her commitment.
	maxHtlcs := 5

	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:            1000000,
			PushAmt:        800000,
			RemoteMaxHtlcs: uint16(maxHtlcs),
		},
	)

	// Wait for Alice and Bob to receive the channel edge from the
	// funding manager.
	err := net.Alice.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err, "alice does not have open channel")

	err = net.Bob.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err, "bob does not have open channel")

	// Alice and bob should have one channel open with each other now.
	assertNodeNumChannels(t, net.Alice, 1)
	assertNodeNumChannels(t, net.Bob, 1)

	// Send our maximum number of htlcs from Bob -> Alice so that we get
	// to a point where Alice won't accept any more htlcs on the channel.
	subscriptions := make([]*holdSubscription, maxHtlcs)
	cancelCtxs := make([]func(), maxHtlcs)

	for i := 0; i < maxHtlcs; i++ {
		subCtx, cancel := context.WithTimeout(ctxb, defaultTimeout)
		cancelCtxs[i] = cancel

		subscriptions[i] = acceptHoldInvoice(
			subCtx, t.t, i, net.Bob, net.Alice,
		)
	}

	// Cancel all of our subscriptions on exit.
	defer func() {
		for _, cancel := range cancelCtxs {
			cancel()
		}
	}()

	err = assertNumActiveHtlcs([]*lntest.HarnessNode{
		net.Alice, net.Bob,
	}, maxHtlcs)
	require.NoError(t.t, err, "htlcs not active")

	// Now we send a payment from Alice -> Bob to sanity check that our
	// commitment limit is not applied in the opposite direction.
	subCtx, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	aliceBobSub := acceptHoldInvoice(
		subCtx, t.t, maxHtlcs, net.Alice, net.Bob,
	)
	err = assertNumActiveHtlcs([]*lntest.HarnessNode{
		net.Alice, net.Bob,
	}, maxHtlcs+1)
	require.NoError(t.t, err, "htlcs not active")

	// Now, we're going to try to send another payment from Bob -> Alice.
	// We've hit our max remote htlcs, so we expect this payment to spin
	// out dramatically with pathfinding.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	payment, err := net.Bob.RouterClient.SendPaymentV2(
		ctxt, &routerrpc.SendPaymentRequest{
			Amt:            1000,
			Dest:           net.Alice.PubKey[:],
			TimeoutSeconds: 60,
			FeeLimitSat:    1000000,
			MaxParts:       10,
			Amp:            true,
		},
	)
	require.NoError(t.t, err, "send payment failed")

	update, err := payment.Recv()
	require.NoError(t.t, err, "no payment in flight update")
	require.Equal(t.t, lnrpc.Payment_IN_FLIGHT, update.Status,
		"payment not inflight")

	update, err = payment.Recv()
	require.NoError(t.t, err, "no payment failed update")
	require.Equal(t.t, lnrpc.Payment_FAILED, update.Status)
	require.Len(t.t, update.Htlcs, 0, "expected no htlcs dispatched")

	// Now that we're done, we cancel all our pending htlcs so that we
	// can cleanup the channel with a coop close.
	for _, sub := range subscriptions {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		sub.cancel(ctxt, t.t)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	aliceBobSub.cancel(ctxt, t.t)

	err = assertNumActiveHtlcs([]*lntest.HarnessNode{
		net.Alice, net.Bob,
	}, 0)
	require.NoError(t.t, err, "expected all htlcs canceled")

	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

type holdSubscription struct {
	recipient           invoicesrpc.InvoicesClient
	hash                lntypes.Hash
	invSubscription     invoicesrpc.Invoices_SubscribeSingleInvoiceClient
	paymentSubscription routerrpc.Router_SendPaymentV2Client
}

// cancel updates a hold invoice to cancel from the recipient and consumes
// updates from the payer until it has reached a final, failed state.
func (h *holdSubscription) cancel(ctx context.Context, t *testing.T) {
	_, err := h.recipient.CancelInvoice(ctx, &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: h.hash[:],
	})
	require.NoError(t, err, "invoice cancel failed")

	invUpdate, err := h.invSubscription.Recv()
	require.NoError(t, err, "cancel invoice subscribe failed")
	require.Equal(t, lnrpc.Invoice_CANCELED, invUpdate.State,
		"expected invoice canceled")

	// We expect one in flight update when our htlc is canceled back, and
	// another when we fail the payment as a whole.
	payUpdate, err := h.paymentSubscription.Recv()
	require.NoError(t, err, "cancel payment subscribe failed")
	require.Len(t, payUpdate.Htlcs, 1)
	require.Equal(t, lnrpc.Payment_IN_FLIGHT, payUpdate.Status)

	payUpdate, err = h.paymentSubscription.Recv()
	require.NoError(t, err, "cancel payment subscribe failed")
	require.Equal(t, lnrpc.Payment_FAILED, payUpdate.Status,
		"expected payment failed")
	require.Equal(t, lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS,
		payUpdate.FailureReason, "expected unknown details")
}

// acceptHoldInvoice adds a hold invoice to the recipient node, pays it from
// the sender and asserts that we have reached the accepted state where htlcs
// are locked in for the payment.
func acceptHoldInvoice(ctx context.Context, t *testing.T, idx int, sender,
	receiver *lntest.HarnessNode) *holdSubscription {

	hash := [lntypes.HashSize]byte{byte(idx + 1)}

	invoice, err := receiver.AddHoldInvoice(
		ctx, &invoicesrpc.AddHoldInvoiceRequest{
			ValueMsat: 10000,
			Hash:      hash[:],
		},
	)
	require.NoError(t, err, "couldn't add invoice")

	invStream, err := receiver.InvoicesClient.SubscribeSingleInvoice(
		ctx, &invoicesrpc.SubscribeSingleInvoiceRequest{
			RHash: hash[:],
		},
	)
	require.NoError(t, err, "could not subscribe to invoice")

	inv, err := invStream.Recv()
	require.NoError(t, err, "invoice open stream failed")
	require.Equal(t, lnrpc.Invoice_OPEN, inv.State,
		"expected open")

	payStream, err := sender.RouterClient.SendPaymentV2(
		ctx, &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitSat:    1000000,
		},
	)
	require.NoError(t, err, "send payment failed")

	// Finally, assert that we progress to an accepted state. We expect
	// the payer to get one update for the creation of the payment, and
	// another when a htlc is dispatched.
	payment, err := payStream.Recv()
	require.NoError(t, err, "payment in flight stream failed")
	require.Equal(t, lnrpc.Payment_IN_FLIGHT, payment.Status)
	require.Len(t, payment.Htlcs, 0)

	payment, err = payStream.Recv()
	require.NoError(t, err, "payment in flight stream failed")
	require.Equal(t, lnrpc.Payment_IN_FLIGHT, payment.Status)
	require.Len(t, payment.Htlcs, 1)

	inv, err = invStream.Recv()
	require.NoError(t, err, "invoice accepted stream failed")
	require.Equal(t, lnrpc.Invoice_ACCEPTED, inv.State,
		"expected accepted invoice")

	return &holdSubscription{
		recipient:           receiver.InvoicesClient,
		hash:                hash,
		invSubscription:     invStream,
		paymentSubscription: payStream,
	}
}
