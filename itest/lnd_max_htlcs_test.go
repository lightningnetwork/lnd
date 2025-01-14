package itest

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testMaxHtlcPathfind tests the case where we try to send a payment over a
// channel where we have already reached the limit of the number of htlcs that
// we may add to the remote party's commitment. This test asserts that we do
// not attempt to use the full channel at all in our pathfinding.
func testMaxHtlcPathfind(ht *lntest.HarnessTest) {
	// Setup a channel between Alice and Bob where Alice will only allow
	// Bob to add a maximum of 5 htlcs to her commitment.
	maxHtlcs := 5

	// Create nodes with the new flag so they understand the new payment
	// status.
	cfg := []string{"--routerrpc.usestatusinitiated"}
	cfgs := [][]string{cfg, cfg}

	// Create a channel Alice->Bob.
	_, nodes := ht.CreateSimpleNetwork(
		cfgs, lntest.OpenChannelParams{
			Amt:            1000000,
			PushAmt:        800000,
			RemoteMaxHtlcs: uint16(maxHtlcs),
		},
	)
	alice, bob := nodes[0], nodes[1]

	// Alice and bob should have one channel open with each other now.
	ht.AssertNodeNumChannels(alice, 1)
	ht.AssertNodeNumChannels(bob, 1)

	// Send our maximum number of htlcs from Bob -> Alice so that we get
	// to a point where Alice won't accept any more htlcs on the channel.
	subscriptions := make([]*holdSubscription, maxHtlcs)

	for i := 0; i < maxHtlcs; i++ {
		subscriptions[i] = acceptHoldInvoice(ht, i, bob, alice)
	}

	ht.AssertNumActiveHtlcs(alice, maxHtlcs)
	ht.AssertNumActiveHtlcs(bob, maxHtlcs)

	// Now we send a payment from Alice -> Bob to sanity check that our
	// commitment limit is not applied in the opposite direction.
	aliceBobSub := acceptHoldInvoice(ht, maxHtlcs, alice, bob)
	ht.AssertNumActiveHtlcs(alice, maxHtlcs+1)
	ht.AssertNumActiveHtlcs(bob, maxHtlcs+1)

	// Now, we're going to try to send another payment from Bob -> Alice.
	// We've hit our max remote htlcs, so we expect this payment to spin
	// out dramatically with pathfinding.
	sendReq := &routerrpc.SendPaymentRequest{
		Amt:         1000,
		Dest:        alice.PubKey[:],
		FeeLimitSat: 1000000,
		MaxParts:    10,
		Amp:         true,
	}
	ht.SendPaymentAndAssertStatus(bob, sendReq, lnrpc.Payment_FAILED)

	// Now that we're done, we cancel all our pending htlcs so that we
	// can cleanup the channel with a coop close.
	for _, sub := range subscriptions {
		sub.cancel(ht)
	}
	aliceBobSub.cancel(ht)

	ht.AssertNumActiveHtlcs(alice, 0)
	ht.AssertNumActiveHtlcs(bob, 0)
}

type holdSubscription struct {
	recipient           *node.HarnessNode
	hash                lntypes.Hash
	invSubscription     invoicesrpc.Invoices_SubscribeSingleInvoiceClient
	paymentSubscription routerrpc.Router_SendPaymentV2Client
}

// cancel updates a hold invoice to cancel from the recipient and consumes
// updates from the payer until it has reached a final, failed state.
func (h *holdSubscription) cancel(ht *lntest.HarnessTest) {
	h.recipient.RPC.CancelInvoice(h.hash[:])

	invUpdate := ht.ReceiveSingleInvoice(h.invSubscription)
	require.Equal(ht, lnrpc.Invoice_CANCELED, invUpdate.State,
		"expected invoice canceled")

	// We expect one in flight update when our htlc is canceled back, and
	// another when we fail the payment as a whole.
	payUpdate := ht.AssertPaymentStatusFromStream(
		h.paymentSubscription, lnrpc.Payment_IN_FLIGHT,
	)
	require.Len(ht, payUpdate.Htlcs, 1)

	payUpdate = ht.AssertPaymentStatusFromStream(
		h.paymentSubscription, lnrpc.Payment_FAILED,
	)
	require.Equal(ht, lnrpc.Payment_FAILED, payUpdate.Status,
		"expected payment failed")
	require.Equal(ht, lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS, //nolint:ll
		payUpdate.FailureReason, "expected unknown details")
}

// acceptHoldInvoice adds a hold invoice to the recipient node, pays it from
// the sender and asserts that we have reached the accepted state where htlcs
// are locked in for the payment.
func acceptHoldInvoice(ht *lntest.HarnessTest, idx int, sender,
	receiver *node.HarnessNode) *holdSubscription {

	hash := [lntypes.HashSize]byte{byte(idx + 1)}

	req := &invoicesrpc.AddHoldInvoiceRequest{
		ValueMsat: 10000,
		Hash:      hash[:],
	}
	invoice := receiver.RPC.AddHoldInvoice(req)

	invStream := receiver.RPC.SubscribeSingleInvoice(hash[:])
	ht.AssertInvoiceState(invStream, lnrpc.Invoice_OPEN)

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice.PaymentRequest,
		FeeLimitSat:    1000000,
	}
	payStream := sender.RPC.SendPayment(sendReq)

	// Finally, assert that we progress to an accepted state. We expect
	// the payer to get one update for the creation of the payment, and
	// another when a htlc is dispatched.
	payment := ht.AssertPaymentStatusFromStream(
		payStream, lnrpc.Payment_INITIATED,
	)
	require.Empty(ht, payment.Htlcs)

	payment = ht.AssertPaymentStatusFromStream(
		payStream, lnrpc.Payment_IN_FLIGHT,
	)
	require.Len(ht, payment.Htlcs, 1)

	ht.AssertInvoiceState(invStream, lnrpc.Invoice_ACCEPTED)

	return &holdSubscription{
		recipient:           receiver,
		hash:                hash,
		invSubscription:     invStream,
		paymentSubscription: payStream,
	}
}
