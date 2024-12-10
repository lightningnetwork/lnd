package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testQuiescence tests whether we can come to agreement on quiescence of a
// channel. We initiate quiescence via RPC and if it succeeds we verify that
// the expected initiator is the resulting initiator.
//
// NOTE FOR REVIEW: this could be improved by blasting the channel with HTLC
// traffic on both sides to increase the surface area of the change under test.
func testQuiescence(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	chanPoint := ht.OpenChannel(bob, alice, lntest.OpenChannelParams{
		Amt: btcutil.Amount(1000000),
	})

	res := alice.RPC.Quiesce(&devrpc.QuiescenceRequest{
		ChanId: chanPoint,
	})

	require.True(ht, res.Initiator)

	req := &routerrpc.SendPaymentRequest{
		Dest:           alice.PubKey[:],
		Amt:            100,
		PaymentHash:    ht.Random32Bytes(),
		FinalCltvDelta: finalCltvDelta,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}

	ht.SendPaymentAssertFail(
		bob, req,
		// This fails with insufficient balance because the bandwidth
		// manager reports 0 bandwidth if a link is not eligible for
		// forwarding, which is the case during quiescence.
		lnrpc.PaymentFailureReason_FAILURE_REASON_INSUFFICIENT_BALANCE,
	)
}
