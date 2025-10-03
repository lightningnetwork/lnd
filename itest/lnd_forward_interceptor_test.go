package itest

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
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

	actionResumeModify = routerrpc.ResolveHoldForwardAction_RESUME_MODIFIED
	actionResume       = routerrpc.ResolveHoldForwardAction_RESUME
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
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	// Initialize the test context with 3 connected nodes.
	cfgs := [][]string{nil, nil, nil}

	// Open and wait for channels.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	cpAB := chanPoints[0]

	// Init the scenario.
	ts := &interceptorTestScenario{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
	}

	// Connect the interceptor.
	interceptor, cancelInterceptor := bob.RPC.HtlcInterceptor()

	// Prepare the test cases.
	req := &lnrpc.Invoice{ValueMsat: 1000}
	addResponse := carol.RPC.AddInvoice(req)
	invoice := carol.RPC.LookupInvoice(addResponse.RHash)
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

		ts.sendPaymentAndAssertAction(tc)
	}()

	// We start the htlc interceptor with a simple implementation that
	// saves all intercepted packets. These packets are held to simulate a
	// pending payment.
	packet := ht.ReceiveHtlcInterceptor(interceptor)

	// Here we should wait for the channel to contain a pending htlc, and
	// also be shown as being active.
	err := wait.NoError(func() error {
		channel := ht.QueryChannelByChanPoint(bob, cpAB)

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
		ht, err, "alice <> bob channel pending htlc never arrived",
	)

	// At this point we want to make bob's link send all pending htlcs to
	// the switch again. We force this behavior by disconnecting and
	// connecting to the peer.
	ht.DisconnectNodes(bob, alice)
	ht.EnsureConnected(bob, alice)

	// Here we wait for the channel to be active again.
	ht.AssertChannelExists(alice, cpAB)
	ht.AssertChannelExists(bob, cpAB)

	// Now that the channel is active we make sure the test passes as
	// expected.

	// We expect one in flight payment since we held the htlcs.
	var preimage lntypes.Preimage
	copy(preimage[:], invoice.RPreimage)
	ht.AssertPaymentStatus(alice, preimage.Hash(), lnrpc.Payment_IN_FLIGHT)

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
//  1. Intercepted failed htlcs result in no payment (invoice is not settled).
//  2. Intercepted resumed htlcs result in a payment (invoice is settled).
//  3. Intercepted held htlcs result in no payment (invoice is not settled).
//  4. When Interceptor disconnects it resumes all held htlcs, which result in
//     valid payment (invoice is settled).
func testForwardInterceptorBasic(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	// Initialize the test context with 3 connected nodes.
	cfgs := [][]string{nil, nil, nil}

	// Open and wait for channels.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	cpAB := chanPoints[0]

	// Init the scenario.
	ts := &interceptorTestScenario{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
	}

	// Connect the interceptor.
	interceptor, cancelInterceptor := bob.RPC.HtlcInterceptor()

	// Prepare the test cases.
	testCases := ts.prepareTestCases()

	// For each test case make sure we initiate a payment from Alice to
	// Carol routed through Bob. For each payment we also test its final
	// status according to the interceptorAction specified in the test
	// case.
	done := make(chan struct{})
	go func() {
		// Signal that all the payments have been sent.
		defer close(done)

		for _, tc := range testCases {
			attempt := ts.sendPaymentAndAssertAction(tc)
			ts.assertAction(tc, attempt)
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
			alice, preimage.Hash(), lnrpc.Payment_IN_FLIGHT,
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
	restartAlice := ht.SuspendNode(alice)
	require.NoError(ht, restartAlice(), "failed to restart alice")

	// Make sure the channel is active from both Alice and Bob's PoV.
	ht.AssertChannelExists(alice, cpAB)
	ht.AssertChannelExists(bob, cpAB)

	// Create a new interceptor as the old one has quit.
	interceptor, cancelInterceptor = bob.RPC.HtlcInterceptor()

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

// testForwardInterceptorRestart tests that the interceptor can read any wire
// custom records provided by the sender of a payment as part of the
// update_add_htlc message and that those records are persisted correctly and
// re-sent on node restart.
func testForwardInterceptorRestart(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	// Initialize the test context with 4 connected nodes.
	cfgs := [][]string{nil, nil, nil, nil}

	// Open and wait for channels.
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol, dave := nodes[0], nodes[1], nodes[2], nodes[3]

	// Connect an interceptor to Bob's node.
	bobInterceptor, cancelBobInterceptor := bob.RPC.HtlcInterceptor()

	// Also connect an interceptor on Carol's node to check whether we're
	// relaying the TLVs send in update_add_htlc over Alice -> Bob on the
	// Bob -> Carol link.
	carolInterceptor, cancelCarolInterceptor := carol.RPC.HtlcInterceptor()
	defer cancelCarolInterceptor()

	req := &lnrpc.Invoice{ValueMsat: 50_000_000}
	addResponse := dave.RPC.AddInvoice(req)
	invoice := dave.RPC.LookupInvoice(addResponse.RHash)

	customRecords := map[uint64][]byte{
		65537: []byte("test"),
	}

	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest:        invoice.PaymentRequest,
		TimeoutSeconds:        int32(wait.PaymentTimeout.Seconds()),
		FeeLimitMsat:          noFeeLimitMsat,
		FirstHopCustomRecords: customRecords,
	}
	ht.SendPaymentAssertInflight(alice, sendReq)

	// We start the htlc interceptor with a simple implementation that saves
	// all intercepted packets. These packets are held to simulate a
	// pending payment.
	packet := ht.ReceiveHtlcInterceptor(bobInterceptor)
	require.Equal(ht, lntest.CustomRecordsWithUnendorsed(
		customRecords,
	), packet.InWireCustomRecords)

	// We accept the payment at Bob and resume it, so it gets to Carol.
	// This means the HTLC should now be fully locked in on Alice's side and
	// any restart of the node should cause the payment to be resumed and
	// the data to be persisted across restarts.
	err := bobInterceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
		IncomingCircuitKey: packet.IncomingCircuitKey,
		Action:             actionResume,
	})
	require.NoError(ht, err, "failed to send request")

	// We don't resume the payment on Carol, so it should be held there.

	// The payment should now be in flight.
	var preimage lntypes.Preimage
	copy(preimage[:], invoice.RPreimage)
	ht.AssertPaymentStatus(alice, preimage.Hash(), lnrpc.Payment_IN_FLIGHT)

	// We don't resume the payment on Carol, so it should be held there.
	// We now restart first Bob, then Alice, so we can make sure we've
	// started the interceptor again on Bob before Alice resumes the
	// payment.
	cancelBobInterceptor()
	restartBob := ht.SuspendNode(bob)
	restartAlice := ht.SuspendNode(alice)

	require.NoError(ht, restartBob(), "failed to restart bob")
	bobInterceptor, cancelBobInterceptor = bob.RPC.HtlcInterceptor()
	defer cancelBobInterceptor()

	require.NoError(ht, restartAlice(), "failed to restart alice")

	// Once restarted, we will wait until the reestabilishment of the links,
	// Alice=>Bob and Bob=>Carol, finish before calling the interceptors. We
	// check this by asserting that Carol is now aware of the two channels
	// being active again.
	ht.AssertChannelInGraph(carol, chanPoints[0])
	ht.AssertChannelInGraph(carol, chanPoints[1])

	// We should get another notification about the held HTLC.
	packet = ht.ReceiveHtlcInterceptor(bobInterceptor)

	require.Len(ht, packet.InWireCustomRecords, 2)
	require.Equal(ht, lntest.CustomRecordsWithUnendorsed(customRecords),
		packet.InWireCustomRecords)

	// And now we forward the payment at Carol, expecting only an
	// endorsement signal in our incoming custom records.
	packet = ht.ReceiveHtlcInterceptor(carolInterceptor)
	require.Len(ht, packet.InWireCustomRecords, 1)
	err = carolInterceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
		IncomingCircuitKey: packet.IncomingCircuitKey,
		Action:             actionResume,
	})
	require.NoError(ht, err, "failed to send request")

	// Assert that the payment was successful.
	ht.AssertPaymentStatus(
		alice, preimage.Hash(), lnrpc.Payment_SUCCEEDED,
		func(p *lnrpc.Payment) error {
			recordsEqual := reflect.DeepEqual(
				lntest.CustomRecordsWithUnendorsed(
					sendReq.FirstHopCustomRecords,
				), p.FirstHopCustomRecords,
			)
			if !recordsEqual {
				return fmt.Errorf("expected custom records to "+
					"be equal, got %v expected %v",
					p.FirstHopCustomRecords,
					sendReq.FirstHopCustomRecords)
			}

			if len(p.Htlcs) != 1 {
				return fmt.Errorf("expected 1 htlc, got %d",
					len(p.Htlcs))
			}

			htlc := p.Htlcs[0]
			rt := htlc.Route
			if rt.FirstHopAmountMsat != rt.TotalAmtMsat {
				return fmt.Errorf("expected first hop amount "+
					"to be %d, got %d", rt.TotalAmtMsat,
					rt.FirstHopAmountMsat)
			}

			// Make sure the custom channel data is nil because
			// this is not a custom channel payment.
			require.Nil(ht, rt.CustomChannelData)

			return nil
		},
	)
}

// interceptorTestScenario is a helper struct to hold the test context and
// provide the needed functionality.
type interceptorTestScenario struct {
	ht                *lntest.HarnessTest
	alice, bob, carol *node.HarnessNode
}

// prepareTestCases prepares 4 tests:
// 1. failed htlc.
// 2. resumed htlc.
// 3. settling htlc externally.
// 4. held htlc that is resumed later.
func (c *interceptorTestScenario) prepareTestCases() []*interceptorTestCase {
	var (
		actionFail   = routerrpc.ResolveHoldForwardAction_FAIL
		actionResume = routerrpc.ResolveHoldForwardAction_RESUME
		actionSettle = routerrpc.ResolveHoldForwardAction_SETTLE
	)

	cases := []*interceptorTestCase{
		{
			amountMsat: 1000, shouldHold: false,
			interceptorAction: actionFail,
		},
		{
			amountMsat: 1000, shouldHold: false,
			interceptorAction: actionResume,
		},
		{
			amountMsat: 1000, shouldHold: false,
			interceptorAction: actionSettle,
		},
		{
			amountMsat: 1000, shouldHold: true,
			interceptorAction: actionResume,
		},
	}

	for _, t := range cases {
		inv := &lnrpc.Invoice{ValueMsat: t.amountMsat}
		addResponse := c.carol.RPC.AddInvoice(inv)
		invoice := c.carol.RPC.LookupInvoice(addResponse.RHash)

		// We'll need to also decode the returned invoice so we can
		// grab the payment address which is now required for ALL
		// payments.
		payReq := c.carol.RPC.DecodePayReq(invoice.PaymentRequest)

		t.invoice = invoice
		t.payAddr = payReq.PaymentAddr
	}

	return cases
}

// sendPaymentAndAssertAction sends a payment from alice to carol and asserts
// that the specified interceptor action is taken.
func (c *interceptorTestScenario) sendPaymentAndAssertAction(
	tc *interceptorTestCase) *lnrpc.HTLCAttempt {

	// Build a route from alice to carol.
	route := c.buildRoute(
		tc.amountMsat, []*node.HarnessNode{c.bob, c.carol}, tc.payAddr,
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

	return c.alice.RPC.SendToRouteV2(sendReq)
}

func (c *interceptorTestScenario) assertAction(tc *interceptorTestCase,
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
func (c *interceptorTestScenario) buildRoute(amtMsat int64,
	hops []*node.HarnessNode, payAddr []byte) *lnrpc.Route {

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

	routeResp := c.alice.RPC.BuildRoute(req)

	return routeResp.Route
}
