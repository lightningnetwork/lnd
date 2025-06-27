package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// testQuiescence tests whether we can come to agreement on quiescence of a
// channel. We initiate quiescence via RPC and if it succeeds we verify that
// the expected initiator is the resulting initiator.
func testQuiescence(ht *lntest.HarnessTest) {
	cfg := node.CfgAnchor
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{cfg, cfg}, lntest.OpenChannelParams{
			Amt: btcutil.Amount(1000000),
		})

	alice, bob := nodes[0], nodes[1]
	chanPoint := chanPoints[0]

	// Bob adds an invoice.
	payAmt := btcutil.Amount(100000)
	invReq := &lnrpc.Invoice{
		Value: int64(payAmt),
	}
	invoice1 := bob.RPC.AddInvoice(invReq)

	// Before quiescence, Alice should be able to send HTLCs.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice1.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertSettled(alice, req)

	// Alice now requires the channel to be quiescent. Once it's done, she
	// will not be able to send payments.
	res := alice.RPC.Quiesce(&devrpc.QuiescenceRequest{
		ChanId: chanPoint,
	})
	require.True(ht, res.Initiator)

	// Bob adds another invoice.
	invoice2 := bob.RPC.AddInvoice(invReq)

	// Alice now tries to pay the second invoice.
	//
	// This fails with insufficient balance because the bandwidth manager
	// reports 0 bandwidth if a link is not eligible for forwarding, which
	// is the case during quiescence.
	req = &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice2.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertFail(
		alice, req,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INSUFFICIENT_BALANCE,
	)

	// Alice now restarts with an extremely short quiescence timeout.
	ht.RestartNodeWithExtraArgs(
		alice, []string{"--htlcswitch.quiescencetimeout=1s"},
	)

	// Once restarted, the channel is no longer quiescent so Alice can
	// finish the payment for invoice2.
	ht.SendPaymentAssertSettled(alice, req)

	// Bob adds another invoice.
	invoice3 := bob.RPC.AddInvoice(invReq)

	// Alice now requires the channel to be quiescent again. Since we are
	// using a short timeout (1ms) for the quiescence, Alice should
	// disconnect from Bob immediately.
	res = alice.RPC.Quiesce(&devrpc.QuiescenceRequest{
		ChanId: chanPoint,
	})
	require.True(ht, res.Initiator)

	// Sleep 2 seconds - we expect the above quiescence to timeout after 1s,
	// then Alice and Bob will try to reconnect.
	time.Sleep(2 * time.Second)

	// The above quiescence timeout will cause Alice to disconnect with Bob.
	// However, since the connection has an open channel, Alice and Bob will
	// be reconnected shortly.
	//
	// TODO(yy): When refactoring the peer connection we need to have an RPC
	// that reports how many times the peer has been connected.
	ht.AssertConnected(alice, bob)

	// Assert that Alice can pay invoice3. This implicitly checks that the
	// above quiescence is terminated.
	req = &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice3.PaymentRequest,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAssertSettled(alice, req)
}
