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
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
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
	// Initialize the test context with 4 connected nodes, our test will
	// make use of 3 of them.
	ts := newInterceptorTestScenario(ht)

	alice, bob, carol := ts.alice, ts.bob, ts.carol

	// Open and wait for channels.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: p},
		{Local: bob, Remote: carol, Param: p},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)
	cpAB, cpBC := resp[0], resp[1]

	// Make sure Alice is aware of channel Bob=>Carol.
	ht.AssertTopologyChannelOpen(alice, cpBC)

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

	// We initiate a payment from Alice to Carol.
	done := make(chan struct{})
	go func() {
		// Signal that all the payments have been sent.
		defer close(done)

		ts.sendPaymentAndAssertAction(
			tc, []*node.HarnessNode{ts.bob, ts.carol},
		)
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
	ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_IN_FLIGHT)

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

	// Finally, close channels.
	ht.CloseChannel(alice, cpAB)
	ht.CloseChannel(bob, cpBC)
}

// testForwardInterceptorBasic tests the forward interceptor RPC layer.
// The test creates a cluster of 4 connected nodes: Alice -> Bob -> Carol ->
// Dave.
//
// Alice sends 4 different payments to Dave while the interceptor handles
// differently the htlcs.
// The test ensures that:
//  1. Intercepted failed htlcs result in no payment (invoice is not settled).
//  2. Intercepted resumed htlcs result in a payment (invoice is settled).
//  3. Intercepted held htlcs result in no payment (invoice is not settled).
//  4. When Interceptor disconnects it resumes all held htlcs, which result in
//     valid payment (invoice is settled).
func testForwardInterceptorBasic(ht *lntest.HarnessTest) {
	ts := newInterceptorTestScenario(ht)

	alice, bob, carol, dave := ts.alice, ts.bob, ts.carol, ts.dave

	// Open and wait for channels.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: p},
		{Local: bob, Remote: carol, Param: p},
		{Local: carol, Remote: dave, Param: p},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)
	cpAB, cpBC, cpCD := resp[0], resp[1], resp[2]

	// Make sure Alice is aware of channel Bob=>Carol and Carol=>Dave.
	ht.AssertTopologyChannelOpen(alice, cpBC)
	ht.AssertTopologyChannelOpen(alice, cpCD)

	// Connect the interceptor.
	interceptor, cancelInterceptor := bob.RPC.HtlcInterceptor()

	// Connect Carol's interceptor.
	carolInterceptor, cancelCarol := carol.RPC.HtlcInterceptor()

	// Prepare the test cases.
	testCases := ts.prepareTestCases()

	// For each test case make sure we initiate a payment from Alice to
	// Dave routed through Bob and Carol. For each payment we also test
	// its final status according to the interceptorAction specified
	// the test case.
	done := make(chan struct{})
	go func() {
		// Signal that all the payments have been sent.
		defer close(done)

		for _, tc := range testCases {
			attempt := ts.sendPaymentAndAssertAction(
				tc, []*node.HarnessNode{
					ts.bob, ts.carol, ts.dave,
				},
			)
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
		// We do not set any explicit endorsement signal on the
		// interceptor response.
		err := interceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
			IncomingCircuitKey: request.IncomingCircuitKey,
			Action:             tc.interceptorAction,
			Preimage:           tc.invoice.RPreimage,
		})
		require.NoError(ht, err, "failed to send request")

		// If the HTLC was resumed by bob, assert that Carol receives
		// the HTLC with the appropriate endorsement signal.
		if tc.interceptorAction !=
			routerrpc.ResolveHoldForwardAction_RESUME {

			continue
		}

		// Assert that the positive endorsement signal is propagated
		// passively along the route.
		carolHTLC := ht.ReceiveHtlcInterceptor(carolInterceptor)
		require.Equal(
			ht, routerrpc.HTLCEndorsement_ENDORSEMENT_TRUE,
			carolHTLC.Endorsed,
		)

		// Once we've checked this value, just resume so that the HTLC
		// will proceed as before.
		err = carolInterceptor.Send(
			//nolint:lll
			&routerrpc.ForwardHtlcInterceptResponse{
				IncomingCircuitKey: carolHTLC.IncomingCircuitKey,
				Action:             routerrpc.ResolveHoldForwardAction_RESUME,
			},
		)
		require.NoError(ht, err, "carol interceptor send")
	}

	// Once we've checked the HTLC carol received, we can cancel her
	// interceptor.
	cancelCarol()

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
			alice, preimage, lnrpc.Payment_IN_FLIGHT,
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

	// Finally, close channels.
	ht.CloseChannel(alice, cpAB)
	ht.CloseChannel(bob, cpBC)
}

// interceptorTestScenario is a helper struct to hold the test context and
// provide the needed functionality.
type interceptorTestScenario struct {
	ht                      *lntest.HarnessTest
	alice, bob, carol, dave *node.HarnessNode
}

// newInterceptorTestScenario initializes a new test scenario with three nodes
// and connects them to have the following topology,
//
//	Alice --> Bob --> Carol --> Dave
//
// Among them, Alice and Bob are standby nodes and Carol and Dave are a new
// nodes.
func newInterceptorTestScenario(
	ht *lntest.HarnessTest) *interceptorTestScenario {

	alice, bob := ht.Alice, ht.Bob
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.EnsureConnected(carol, dave)

	return &interceptorTestScenario{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
		dave:  dave,
	}
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
		addResponse := c.dave.RPC.AddInvoice(inv)
		invoice := c.dave.RPC.LookupInvoice(addResponse.RHash)

		// We'll need to also decode the returned invoice so we can
		// grab the payment address which is now required for ALL
		// payments.
		payReq := c.dave.RPC.DecodePayReq(invoice.PaymentRequest)

		t.invoice = invoice
		t.payAddr = payReq.PaymentAddr
	}

	return cases
}

// sendPaymentAndAssertAction sends a payment from alice to dave and asserts
// that the specified interceptor action is taken.
func (c *interceptorTestScenario) sendPaymentAndAssertAction(
	tc *interceptorTestCase,
	hops []*node.HarnessNode) *lnrpc.HTLCAttempt {

	// Build a route with the hops provided.
	route := c.buildRoute(tc.amountMsat, hops, tc.payAddr)

	// Send a custom record to the forwarding node.
	route.Hops[0].CustomRecords = map[uint64][]byte{
		customTestKey: customTestValue,
	}

	// Send the payment, endorsing the outgoing htlc.
	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: tc.invoice.RHash,
		Route:       route,
		Endorsed:    routerrpc.HTLCEndorsement_ENDORSEMENT_TRUE,
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

// testHTLCEndorsement tests the interceptor's ability to modify endorsement
// signals on HTLCs, testing that intercepted alterations are correctly
// propagated to the receiving node.
func testHTLCEndorsement(ht *lntest.HarnessTest) {
	ts := newInterceptorTestScenario(ht)
	alice, bob, carol, dave := ts.alice, ts.bob, ts.carol, ts.dave

	// Open and wait for channels.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: p},
		{Local: bob, Remote: carol, Param: p},
		{Local: carol, Remote: dave, Param: p},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)
	cpAB, cpBC, cpCD := resp[0], resp[1], resp[2]

	// Make sure Alice is aware of all channels.
	ht.AssertTopologyChannelOpen(alice, cpAB)
	ht.AssertTopologyChannelOpen(alice, cpBC)
	ht.AssertTopologyChannelOpen(alice, cpCD)

	// Connect the bobInterceptor.
	bobInterceptor, cancelBob := bob.RPC.HtlcInterceptor()
	defer cancelBob()

	// Subscribe to the interceptor for Carol so that we can see the
	// HTLC that is forwarded on by Bob with correct values.
	carolInterceptor, cancelCarol := carol.RPC.HtlcInterceptor()
	defer cancelCarol()

	invReq := &lnrpc.Invoice{ValueMsat: 1000}
	addResponse := dave.RPC.AddInvoice(invReq)

	// Send an endorsed payment from Alice to Dave.
	paymentClient := alice.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		PaymentRequest:    addResponse.PaymentRequest,
		NoInflightUpdates: true,
		TimeoutSeconds:    120,
		Endorsed:          routerrpc.HTLCEndorsement_ENDORSEMENT_TRUE,
		FeeLimitMsat:      noFeeLimitMsat,
	})

	// Assert that Bob receives an endorsed HTLC, and drop the endorsement
	// signal on the outgoing link.
	interceptAndCheckEndorsed(
		ht,
		bobInterceptor,
		routerrpc.HTLCEndorsement_ENDORSEMENT_TRUE,
		routerrpc.HTLCEndorsement_ENDORSEMENT_FALSE,
	)

	// Assert that Carol receives the HTLC with the endorsement signal
	// dropped, and take no action to set her outgoing value.
	interceptAndCheckEndorsed(
		ht,
		carolInterceptor,
		routerrpc.HTLCEndorsement_ENDORSEMENT_FALSE,
		routerrpc.HTLCEndorsement_ENDORSEMENT_UNKNOWN,
	)

	pmt, err := paymentClient.Recv()
	require.NoError(ht, err)

	require.Equal(ht, lnrpc.Payment_SUCCEEDED, pmt.Status)

	// Finally, close channels.
	ht.CloseChannel(alice, cpAB)
	ht.CloseChannel(bob, cpBC)
	ht.CloseChannel(carol, cpCD)
}

func interceptAndCheckEndorsed(ht *lntest.HarnessTest,
	interceptor rpc.InterceptorClient, incomingEndorsed,
	outgoingEndorsed routerrpc.HTLCEndorsement) {

	respChan := make(chan *routerrpc.ForwardHtlcInterceptRequest, 1)
	go func() {
		defer close(respChan)

		request, err := interceptor.Recv()
		require.NoError(ht, err)

		respChan <- request
	}()

	var request *routerrpc.ForwardHtlcInterceptRequest
	select {
	case request = <-respChan:
		require.NotNil(ht, request)
		require.Equal(ht, incomingEndorsed, request.Endorsed)

	case <-time.After(defaultTimeout):
		ht.Fatal("timeout waiting for interceptor")
	}

	err := interceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
		IncomingCircuitKey: request.IncomingCircuitKey,
		Action:             routerrpc.ResolveHoldForwardAction_RESUME,
		Endorsed:           outgoingEndorsed,
	})
	require.NoError(ht, err, "interceptor resume request")
}
