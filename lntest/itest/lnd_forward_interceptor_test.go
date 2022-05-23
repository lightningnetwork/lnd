package itest

import (
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	customTestKey   uint64 = 394829
	customTestValue        = []byte{1, 3, 5}
)

type interceptorTestCase struct {
	amountMsat        int64
	payAddr           []byte
	invoice           *lnrpc.Invoice
	shouldHold        bool
	interceptorAction routerrpc.ResolveHoldForwardAction
}

// testForwardInterceptorDedupHtlc tests that upon reconnection, duplicate
// HTLCs aren't re-notified using the HTLC interceptor API.
func testForwardInterceptorDedupHtlc(ht *lntest.HarnessTest) {
	// Initialize the test context with 3 connected nodes.
	ctx := newInterceptorTestContext(ht)
	const chanAmt = btcutil.Amount(300000)

	// Open and wait for channels.
	chanPoint := ctx.openChannel(ctx.alice, ctx.bob, chanAmt)
	ctx.openChannel(ctx.bob, ctx.carol, chanAmt)
	defer ctx.closeChannels()

	// Connect the interceptor.
	interceptor, cancelInterceptor := ht.HtlcInterceptor(ctx.bob)

	// Prepare the test cases.
	req := &lnrpc.Invoice{ValueMsat: 1000}
	addResponse := ht.AddInvoice(req, ctx.carol)
	invoice := ht.LookupInvoice(ctx.carol, addResponse.RHash)
	tc := &interceptorTestCase{
		amountMsat: 1000,
		invoice:    invoice,
		payAddr:    invoice.PaymentAddr,
	}

	// We initiate a payment from Alice.
	done := make(chan struct{})
	go func() {
		// Signal that all the payments have been sent.
		defer close(done)

		ctx.sendPaymentAndAssertAction(tc)
	}()

	// We start the htlc interceptor with a simple implementation that
	// saves all intercepted packets. These packets are held to simulate a
	// pending payment.
	packet := ht.ReceiveHtlcInterceptor(interceptor)

	// Here we should wait for the channel to contain a pending htlc, and
	// also be shown as being active.
	err := wait.NoError(func() error {
		channel := ht.QueryChannelByChanPoint(ctx.bob, chanPoint)

		if len(channel.PendingHtlcs) == 0 {
			return fmt.Errorf("expect alice <> bob channel to " +
				"have pending htlcs")
		}
		if channel.Active {
			return nil
		}

		return fmt.Errorf("channel not active")
	}, defaultTimeout)
	require.NoError(
		ctx.ht, err, "alice <> bob channel pending htlc never arrived",
	)

	// At this point we want to make bob's link send all pending htlcs to
	// the switch again. We force this behavior by disconnecting and
	// connecting to the peer.
	ctx.ht.DisconnectNodes(ctx.bob, ctx.alice)
	ctx.ht.EnsureConnected(ctx.bob, ctx.alice)

	// Here we wait for the channel to be active again.
	ht.AssertChannelExists(ctx.bob, chanPoint)

	// Now that the channel is active we make sure the test passes as
	// expected.

	// We expect one in flight payment since we held the htlcs.
	var preimage lntypes.Preimage
	copy(preimage[:], invoice.RPreimage)
	ht.AssertPaymentStatus(ctx.alice, preimage, lnrpc.Payment_IN_FLIGHT)

	// At this point if we have more than one held htlcs then we should
	// fail. This means we hold the same htlc twice which is a risk we want
	// to eliminate. If we don't have the same htlc twice in theory we can
	// cancel one and settle the other by mistake.
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)

		_, err := interceptor.Recv()
		require.Error(ht, err, "expected an error from interceptor")

		status, ok := status.FromError(err)
		switch {
		// If it is just the error result of the context cancellation
		// the we exit silently.
		case ok && status.Code() == codes.Canceled:
			fallthrough

		// When the test ends, during the node's shutdown it will close
		// the connection.
		case strings.Contains(err.Error(), "closed network connection"):
			fallthrough

		case strings.Contains(err.Error(), "EOF"):
			return
		}

		// Otherwise we receive an unexpected error.
		require.Failf(ht, "interceptor", "unexpected err: %v", err)
	}()

	// We now fail all htlcs to cancel the payment.
	err = interceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
		IncomingCircuitKey: packet.IncomingCircuitKey,
		Action:             routerrpc.ResolveHoldForwardAction_FAIL,
	})
	require.NoError(ht, err, "failed to send request")

	// Cancel the context, which will disconnect the above interceptor.
	cancelInterceptor()

	// Make sure all goroutines are finished.
	select {
	case <-done:
	case <-time.After(defaultTimeout):
		require.Fail(ht, "timeout waiting for sending payment")
	}

	select {
	case <-errDone:
	case <-time.After(defaultTimeout):
		require.Fail(ht, "timeout waiting for interceptor error")
	}
}

// testForwardInterceptorBasic tests the forward interceptor RPC layer.
// The test creates a cluster of 3 connected nodes: Alice -> Bob -> Carol
// Alice sends 4 different payments to Carol while the interceptor handles
// differently the htlcs.
// The test ensures that:
// 1. Intercepted failed htlcs result in no payment (invoice is not settled).
// 2. Intercepted resumed htlcs result in a payment (invoice is settled).
// 3. Intercepted held htlcs result in no payment (invoice is not settled).
// 4. When Interceptor disconnects it resumes all held htlcs, which result in
//    valid payment (invoice is settled).
func testForwardInterceptorBasic(ht *lntest.HarnessTest) {
	ctx := newInterceptorTestContext(ht)
	const chanAmt = btcutil.Amount(300000)

	// Open and wait for channels.
	chanPointAliceBob := ctx.openChannel(ctx.alice, ctx.bob, chanAmt)
	ctx.openChannel(ctx.bob, ctx.carol, chanAmt)
	defer ctx.closeChannels()

	// Connect the interceptor.
	interceptor, cancelInterceptor := ht.HtlcInterceptor(ctx.bob)

	// Prepare the test cases.
	testCases := ctx.prepareTestCases()

	// For each test case make sure we initiate a payment from Alice to
	// Carol routed through Bob. For each payment we also test its final
	// status according to the interceptorAction specified in the test
	// case.
	done := make(chan struct{})
	go func() {
		// Signal that all the payments have been sent.
		defer close(done)

		for _, tc := range testCases {
			attempt := ctx.sendPaymentAndAssertAction(tc)
			ctx.assertAction(tc, attempt)
		}
	}()

	// We make sure here the interceptor has processed all packets before
	// we check the payment statuses.
	for _, tc := range testCases {
		request := ht.ReceiveHtlcInterceptor(interceptor)

		// Assert sanity of informational packet data.
		require.NotZero(ht, request.OutgoingRequestedChanId)
		require.NotZero(ht, request.IncomingExpiry)
		require.NotZero(ht, request.IncomingAmountMsat)

		require.Less(ht, request.OutgoingExpiry,
			request.IncomingExpiry)
		require.Less(ht, request.OutgoingAmountMsat,
			request.IncomingAmountMsat)

		value, ok := request.CustomRecords[customTestKey]
		require.True(ht, ok, "expected custom record")
		require.Equal(ht, customTestValue, value)

		// For held packets we ignore, keeping them in hold status.
		if tc.shouldHold {
			continue
		}

		// For all other packets we resolve according to the test case.
		err := interceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
			IncomingCircuitKey: request.IncomingCircuitKey,
			Action:             tc.interceptorAction,
			Preimage:           tc.invoice.RPreimage,
		})
		require.NoError(ht, err, "failed to send request")
	}

	// At this point we are left with the held packets, we want to make
	// sure each one of them has a corresponding 'in-flight' payment at
	// Alice's node.
	for _, testCase := range testCases {
		if !testCase.shouldHold {
			continue
		}

		var preimage lntypes.Preimage
		copy(preimage[:], testCase.invoice.RPreimage)

		payment := ht.AssertPaymentStatus(
			ctx.alice, preimage, lnrpc.Payment_IN_FLIGHT,
		)
		expectedAmt := testCase.invoice.ValueMsat
		require.Equal(ht, expectedAmt, payment.ValueMsat,
			"incorrect in flight amount")
	}

	// Cancel the context, which will disconnect the above interceptor.
	cancelInterceptor()

	// Disconnect interceptor should cause resume held packets. After that
	// we wait for all go routines to finish, including the one that tests
	// the payment final status for the held payment.
	select {
	case <-done:
	case <-time.After(defaultTimeout):
		require.Fail(ht, "timeout waiting for sending payment")
	}

	// Verify that we don't get notified about already completed HTLCs
	// We do that by restarting alice, the sender the HTLCs. Under
	// https://github.com/lightningnetwork/lnd/issues/5115
	// this should cause all HTLCs settled or failed by the interceptor to
	// renotify.
	restartAlice := ht.SuspendNode(ctx.alice)
	require.NoError(ht, restartAlice(), "failed to restart alice")

	// Make sure the channel is active from Bob's PoV.
	ht.AssertChannelExists(ctx.bob, chanPointAliceBob)

	// Create a new interceptor as the old one has quit.
	interceptor, cancelInterceptor = ht.HtlcInterceptor(ctx.bob)

	done = make(chan struct{})
	go func() {
		defer close(done)

		_, err := interceptor.Recv()
		require.Error(ht, err, "expected an error from interceptor")

		status, ok := status.FromError(err)
		switch {
		// If it is just the error result of the context cancellation
		// the we exit silently.
		case ok && status.Code() == codes.Canceled:
			fallthrough

		// When the test ends, during the node's shutdown it will close
		// the connection.
		case strings.Contains(err.Error(), "closed network connection"):
			fallthrough

		case strings.Contains(err.Error(), "EOF"):
			return
		}

		// Otherwise we receive an unexpected error.
		require.Failf(ht, "iinterceptor", "unexpected err: %v", err)
	}()

	// Cancel the context, which will disconnect the above interceptor.
	cancelInterceptor()
	select {
	case <-done:
	case <-time.After(defaultTimeout):
		require.Fail(ht, "timeout waiting for interceptor error")
	}
}

// interceptorTestContext is a helper struct to hold the test context and
// provide the needed functionality.
type interceptorTestContext struct {
	mppTestContext
}

func newInterceptorTestContext(ht *lntest.HarnessTest) *interceptorTestContext {
	ctx := newMppTestContext(ht)
	return &interceptorTestContext{*ctx}
}

// prepareTestCases prepares 4 tests:
// 1. failed htlc.
// 2. resumed htlc.
// 3. settling htlc externally.
// 4. held htlc that is resumed later.
func (c *interceptorTestContext) prepareTestCases() []*interceptorTestCase {
	cases := []*interceptorTestCase{
		{
			amountMsat: 1000, shouldHold: false,
			interceptorAction: routerrpc.ResolveHoldForwardAction_FAIL,
		},
		{
			amountMsat: 1000, shouldHold: false,
			interceptorAction: routerrpc.ResolveHoldForwardAction_RESUME,
		},
		{
			amountMsat: 1000, shouldHold: false,
			interceptorAction: routerrpc.ResolveHoldForwardAction_SETTLE,
		},
		{
			amountMsat: 1000, shouldHold: true,
			interceptorAction: routerrpc.ResolveHoldForwardAction_RESUME,
		},
	}

	for _, t := range cases {
		inv := &lnrpc.Invoice{ValueMsat: t.amountMsat}
		addResponse := c.ht.AddInvoice(inv, c.carol)
		invoice := c.ht.LookupInvoice(c.carol, addResponse.RHash)

		// We'll need to also decode the returned invoice so we can
		// grab the payment address which is now required for ALL
		// payments.
		payReq := c.ht.DecodePayReq(c.carol, invoice.PaymentRequest)

		t.invoice = invoice
		t.payAddr = payReq.PaymentAddr
	}
	return cases
}

// sendPaymentAndAssertAction sends a payment from alice to carol and asserts
// that the specified interceptor action is taken.
func (c *interceptorTestContext) sendPaymentAndAssertAction(
	tc *interceptorTestCase) *lnrpc.HTLCAttempt {

	// Build a route from alice to carol.
	route := c.buildRoute(
		tc.amountMsat, []*lntest.HarnessNode{c.bob, c.carol},
		tc.payAddr,
	)

	// Send a custom record to the forwarding node.
	route.Hops[0].CustomRecords = map[uint64][]byte{
		customTestKey: customTestValue,
	}

	// Send the payment.
	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: tc.invoice.RHash,
		Route:       route,
	}
	return c.ht.SendToRouteV2(c.alice, sendReq)
}

func (c *interceptorTestContext) assertAction(tc *interceptorTestCase,
	attempt *lnrpc.HTLCAttempt) {

	// Now check the expected action has been taken.
	switch tc.interceptorAction {
	// For 'fail' interceptor action we make sure the payment failed.
	case routerrpc.ResolveHoldForwardAction_FAIL:
		require.Equal(c.ht, lnrpc.HTLCAttempt_FAILED, attempt.Status,
			"expected payment to fail")

		// Assert that we get a temporary channel failure which has a
		// channel update.
		require.NotNil(c.ht, attempt.Failure)
		require.NotNil(c.ht, attempt.Failure.ChannelUpdate)

		require.Equal(c.ht, lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE,
			attempt.Failure.Code)

	// For settle and resume we make sure the payment is successful.
	case routerrpc.ResolveHoldForwardAction_SETTLE:
		fallthrough

	case routerrpc.ResolveHoldForwardAction_RESUME:
		require.Equal(c.ht, lnrpc.HTLCAttempt_SUCCEEDED,
			attempt.Status, "expected payment to succeed")
	}
}

// buildRoute is a helper function to build a route with given hops.
func (c *interceptorTestContext) buildRoute(amtMsat int64,
	hops []*lntest.HarnessNode, payAddr []byte) *lnrpc.Route {

	rpcHops := make([][]byte, 0, len(hops))
	for _, hop := range hops {
		k := hop.PubKeyStr
		pubkey, err := route.NewVertexFromStr(k)
		require.NoErrorf(c.ht, err, "error parsing %v: %v", k, err)
		rpcHops = append(rpcHops, pubkey[:])
	}

	req := &routerrpc.BuildRouteRequest{
		AmtMsat:        amtMsat,
		FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
		HopPubkeys:     rpcHops,
		PaymentAddr:    payAddr,
	}

	routeResp := c.ht.BuildRoute(c.alice, req)
	return routeResp.Route
}
