package itest

import (
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
}
