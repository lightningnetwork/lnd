package itest

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testLocalPaymentDispatchGuard verifies that a node refusing local payment
// dispatch rejects the local payment-sending RPCs, so an external router
// dispatching over the switchrpc interface cannot collide with the embedded
// router in the shared attempt-ID space, while the read-only routing surface
// and the HTLC interception stream that an external router depends on keep
// working.
func testLocalPaymentDispatchGuard(ht *lntest.HarnessTest) {
	const (
		chanAmt    = btcutil.Amount(100000)
		paymentAmt = 10000
	)

	// Alice keeps the switchrpc build's default of refusing local dispatch;
	// every other harness node opts in, so Bob is ordinary.
	alice := ht.NewNodeWithOpts(
		"Alice", nil, node.WithRefuseLocalPaymentDispatch(),
	)
	ht.FundNumCoins(alice, 1)
	bob := ht.NewNode("Bob", nil)

	ht.EnsureConnected(alice, bob)
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPoint)

	ht.AssertChannelInGraph(alice, chanPoint)

	// Bob issues an invoice for the send attempts.
	inv := bob.RPC.AddInvoice(&lnrpc.Invoice{Value: paymentAmt})

	ctx := ht.Context()

	// SendPaymentV2 is server-streaming; a handler that errors before
	// sending surfaces the error on the first Recv. It must be refused with
	// codes.FailedPrecondition.
	sendStream, err := alice.RPC.Router.SendPaymentV2(
		ctx, &routerrpc.SendPaymentRequest{
			PaymentRequest: inv.PaymentRequest,
			TimeoutSeconds: 60,
		},
	)
	require.NoError(ht, err)
	_, err = sendStream.Recv()
	require.Equal(ht, codes.FailedPrecondition, status.Code(err),
		"SendPaymentV2 must be refused in external lifecycle mode")

	// QueryRoutes is read-only and must still answer. It also gives us a
	// route for the SendToRouteV2 attempt below.
	routes := alice.RPC.QueryRoutes(&lnrpc.QueryRoutesRequest{
		PubKey: bob.PubKeyStr,
		Amt:    paymentAmt,
	})
	require.NotEmpty(ht, routes.Routes, "QueryRoutes must still answer")

	// SendToRouteV2 is unary; the guard error is returned directly. It must
	// be refused with codes.FailedPrecondition.
	_, err = alice.RPC.Router.SendToRouteV2(
		ctx, &routerrpc.SendToRouteRequest{
			PaymentHash: inv.RHash,
			Route:       routes.Routes[0],
		},
	)
	require.Equal(ht, codes.FailedPrecondition, status.Code(err),
		"SendToRouteV2 must be refused in external lifecycle mode")

	// The graph-based EstimateRouteFee sends no HTLC and must still answer.
	_, err = alice.RPC.Router.EstimateRouteFee(
		ctx, &routerrpc.RouteFeeRequest{
			Dest:   bob.PubKey[:],
			AmtSat: paymentAmt,
		},
	)
	require.NoError(ht, err, "graph EstimateRouteFee must still answer")

	// HtlcInterceptor must still work: PS holds this stream on every
	// gateway, and it is the reason routerrpc is not compiled out.
	_, cancelInterceptor := alice.RPC.HtlcInterceptor()
	cancelInterceptor()

	// SubscribeHtlcEvents must still work.
	alice.RPC.SubscribeHtlcEvents()
}
