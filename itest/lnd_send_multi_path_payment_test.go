package itest

import (
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testSendMultiPathPayment tests that we are able to successfully route a
// payment using multiple shards across different paths.
func testSendMultiPathPayment(ht *lntest.HarnessTest) {
	mts := newMppTestScenario(ht)

	const paymentAmt = btcutil.Amount(300000)

	// Set up a network with three different paths Alice <-> Bob. Channel
	// capacities are set such that the payment can only succeed if (at
	// least) three paths are used.
	//
	//              _ Eve _
	//             /       \
	// Alice -- Carol ---- Bob
	//      \              /
	//       \__ Dave ____/
	//
	req := &mppOpenChannelRequest{
		amtAliceCarol: 235000,
		amtAliceDave:  135000,
		amtCarolBob:   135000,
		amtCarolEve:   135000,
		amtDaveBob:    135000,
		amtEveBob:     135000,
	}
	mts.openChannels(req)
	chanPointAliceDave := mts.channelPoints[1]

	// Increase Dave's fee to make the test deterministic. Otherwise it
	// would be unpredictable whether pathfinding would go through Charlie
	// or Dave for the first shard.
	expectedPolicy := mts.updateDaveGlobalPolicy()

	// Make sure Alice has heard it.
	ht.AssertChannelPolicyUpdate(
		mts.alice, mts.dave, expectedPolicy, chanPointAliceDave, false,
	)

	// Our first test will be Alice paying Bob using a SendPayment call.
	// Let Bob create an invoice for Alice to pay.
	payReqs, rHashes, invoices := ht.CreatePayReqs(mts.bob, paymentAmt, 1)

	rHash := rHashes[0]
	payReq := payReqs[0]

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: payReq,
		MaxParts:       10,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	payment := ht.SendPaymentAssertSettled(mts.alice, sendReq)

	// Make sure we got the preimage.
	require.Equal(ht, hex.EncodeToString(invoices[0].RPreimage),
		payment.PaymentPreimage, "preimage doesn't match")

	// Check that Alice split the payment in at least three shards. Because
	// the hand-off of the htlc to the link is asynchronous (via a mailbox),
	// there is some non-determinism in the process. Depending on whether
	// the new pathfinding round is started before or after the htlc is
	// locked into the channel, different sharding may occur. Therefore we
	// can only check if the number of shards isn't below the theoretical
	// minimum.
	succeeded := 0
	for _, htlc := range payment.Htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			succeeded++
		}
	}

	const minExpectedShards = 3
	require.GreaterOrEqual(ht, succeeded, minExpectedShards,
		"expected shards not reached")

	// Make sure Bob show the invoice as settled for the full amount.
	inv := mts.bob.RPC.LookupInvoice(rHash)

	require.EqualValues(ht, paymentAmt, inv.AmtPaidSat,
		"incorrect payment amt")

	require.Equal(ht, lnrpc.Invoice_SETTLED, inv.State,
		"Invoice not settled")

	settled := 0
	for _, htlc := range inv.Htlcs {
		if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
			settled++
		}
	}
	require.Equal(ht, succeeded, settled,
		"num of HTLCs wrong")

	// Finally, close all channels.
	mts.closeChannels()
}
