package itest

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
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
func testForwardInterceptorDedupHtlc(ht *lntemp.HarnessTest) {
	// Initialize the test context with 3 connected nodes.
	ts := newInterceptorTestScenario(ht)

	alice, bob, carol := ts.alice, ts.bob, ts.carol

	// Open and wait for channels.
	const chanAmt = btcutil.Amount(300000)
	p := lntemp.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntemp.OpenChannelRequest{
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
// The test creates a cluster of 3 connected nodes: Alice -> Bob -> Carol
// Alice sends 4 different payments to Carol while the interceptor handles
// differently the htlcs.
// The test ensures that:
//  1. Intercepted failed htlcs result in no payment (invoice is not settled).
//  2. Intercepted resumed htlcs result in a payment (invoice is settled).
//  3. Intercepted held htlcs result in no payment (invoice is not settled).
//  4. When Interceptor disconnects it resumes all held htlcs, which result in
//     valid payment (invoice is settled).
func testForwardInterceptorBasic(net *lntest.NetworkHarness, t *harnessTest) {
	// Initialize the test context with 3 connected nodes.
	alice := net.NewNode(t.t, "alice", nil)
	defer shutdownAndAssert(net, t, alice)

	bob := net.NewNode(t.t, "bob", nil)
	defer shutdownAndAssert(net, t, bob)

	carol := net.NewNode(t.t, "carol", nil)
	defer shutdownAndAssert(net, t, carol)

	testContext := newInterceptorTestContext(t, net, alice, bob, carol)

	const (
		chanAmt = btcutil.Amount(300000)
	)

	// Open and wait for channels.
	testContext.openChannel(testContext.alice, testContext.bob, chanAmt)
	testContext.openChannel(testContext.bob, testContext.carol, chanAmt)
	defer testContext.closeChannels()
	testContext.waitForChannels()

	// Connect the interceptor.
	ctxb := context.Background()
	ctxt, cancelInterceptor := context.WithTimeout(ctxb, defaultTimeout)
	interceptor, err := testContext.bob.RouterClient.HtlcInterceptor(ctxt)
	require.NoError(t.t, err, "failed to create HtlcInterceptor")

	// Prepare the test cases.
	testCases := testContext.prepareTestCases()

	// A channel for the interceptor go routine to send the requested packets.
	interceptedChan := make(chan *routerrpc.ForwardHtlcInterceptRequest,
		len(testCases))

	// Run the interceptor loop in its own go routine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			request, err := interceptor.Recv()
			if err != nil {
				// If it is  just the error result of the context cancellation
				// the we exit silently.
				status, ok := status.FromError(err)
				if ok && status.Code() == codes.Canceled {
					return
				}
				// Otherwise it an unexpected error, we fail the test.
				require.NoError(t.t, err, "unexpected error in interceptor.Recv()")
				return
			}
			interceptedChan <- request
		}
	}()

	// For each test case make sure we initiate a payment from Alice to Carol
	// routed through Bob. For each payment we also test its final status
	// according to the interceptorAction specified in the test case.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, tc := range testCases {
			attempt, err := testContext.sendAliceToCarolPayment(
				context.Background(), tc.invoice.ValueMsat,
				tc.invoice.RHash, tc.payAddr,
			)

			if t.t.Failed() {
				return
			}
			if err != nil {
				require.NoError(t.t, err, "failed to send payment")
			}

			switch tc.interceptorAction {
			// For 'fail' interceptor action we make sure the payment failed.
			case routerrpc.ResolveHoldForwardAction_FAIL:
				require.Equal(t.t, lnrpc.HTLCAttempt_FAILED,
					attempt.Status, "expected payment to fail")

				// Assert that we get a temporary channel
				// failure which has a channel update.
				require.NotNil(t.t, attempt.Failure)
				require.NotNil(t.t, attempt.Failure.ChannelUpdate)

				require.Equal(t.t,
					lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE,
					attempt.Failure.Code)

			// For settle and resume we make sure the payment is successful.
			case routerrpc.ResolveHoldForwardAction_SETTLE:
				fallthrough

			case routerrpc.ResolveHoldForwardAction_RESUME:
				require.Equal(t.t, lnrpc.HTLCAttempt_SUCCEEDED,
					attempt.Status, "expected payment to succeed")
			}
		}
	}()

	// We make sure here the interceptor has processed all packets before we
	// check the payment statuses.
	for i := 0; i < len(testCases); i++ {
		select {
		case request := <-interceptedChan:
			// Assert sanity of informational packet data.
			require.NotZero(t.t, request.OutgoingRequestedChanId)
			require.NotZero(t.t, request.IncomingExpiry)
			require.NotZero(t.t, request.IncomingAmountMsat)

			require.Less(
				t.t,
				request.OutgoingExpiry, request.IncomingExpiry,
			)
			require.Less(
				t.t,
				request.OutgoingAmountMsat,
				request.IncomingAmountMsat,
			)

			value, ok := request.CustomRecords[customTestKey]
			require.True(t.t, ok, "expected custom record")
			require.Equal(t.t, customTestValue, value)

			testCase := testCases[i]

			// For held packets we ignore, keeping them in hold status.
			if testCase.shouldHold {
				continue
			}

			// For all other packets we resolve according to the test case.
			_ = interceptor.Send(&routerrpc.ForwardHtlcInterceptResponse{
				IncomingCircuitKey: request.IncomingCircuitKey,
				Action:             testCase.interceptorAction,
				Preimage:           testCase.invoice.RPreimage,
			})
		case <-time.After(defaultTimeout):
			t.Fatalf("response from interceptor was not received %v", i)
		}
	}

	// At this point we are left with the held packets, we want to make sure
	// each one of them has a corresponding 'in-flight' payment at
	// Alice's node.
	payments, err := testContext.alice.ListPayments(context.Background(),
		&lnrpc.ListPaymentsRequest{IncludeIncomplete: true})
	require.NoError(t.t, err, "failed to fetch payment")

	for _, testCase := range testCases {
		if testCase.shouldHold {
			hashStr := hex.EncodeToString(testCase.invoice.RHash)
			var foundPayment *lnrpc.Payment
			expectedAmt := testCase.invoice.ValueMsat
			for _, p := range payments.Payments {
				if p.PaymentHash == hashStr {
					foundPayment = p
					break
				}
			}
			require.NotNil(t.t, foundPayment, fmt.Sprintf("expected "+
				"to find pending payment for held htlc %v",
				hashStr))
			require.Equal(t.t, lnrpc.Payment_IN_FLIGHT,
				foundPayment.Status, "expected payment to be "+
					"in flight")
			require.Equal(t.t, expectedAmt, foundPayment.ValueMsat,
				"incorrect in flight amount")
		}
	}

	// Disconnect interceptor should cause resume held packets.
	// After that we wait for all go routines to finish, including the one
	// that tests the payment final status for the held payment.
	cancelInterceptor()
	wg.Wait()

	// Verify that we don't get notified about already completed HTLCs
	// We do that by restarting alice, the sender the HTLCs. Under
	// https://github.com/lightningnetwork/lnd/issues/5115
	// this should cause all HTLCs settled or failed by the interceptor to renotify.
	restartAlice, err := net.SuspendNode(alice)
	require.NoError(t.t, err, "failed to suspend alice")

	ctxt, cancelInterceptor = context.WithTimeout(ctxb, defaultTimeout)
	defer cancelInterceptor()
	interceptor, err = testContext.bob.RouterClient.HtlcInterceptor(ctxt)
	require.NoError(t.t, err, "failed to create HtlcInterceptor")

	err = restartAlice()
	require.NoError(t.t, err, "failed to restart alice")

	go func() {
		request, err := interceptor.Recv()
		if err != nil {
			// If it is  just the error result of the context cancellation
			// the we exit silently.
			status, ok := status.FromError(err)
			if ok && status.Code() == codes.Canceled {
				return
			}
			// Otherwise it an unexpected error, we fail the test.
			require.NoError(
				t.t, err, "unexpected error in interceptor.Recv()",
			)
			return
		}

		require.Nil(t.t, request, "no more intercepts should arrive")
	}()

	err = wait.Predicate(func() bool {
		channels, err := bob.ListChannels(ctxt, &lnrpc.ListChannelsRequest{
			ActiveOnly: true, Peer: alice.PubKey[:],
		})
		return err == nil && len(channels.Channels) > 0
	}, defaultTimeout)
	require.NoError(t.t, err, "alice <> bob channel didn't re-activate")
}

// interceptorTestContext is a helper struct to hold the test context and
// provide the needed functionality.
type interceptorTestContext struct {
	t   *harnessTest
	net *lntest.NetworkHarness

	// Keep a list of all our active channels.
	networkChans      []*lnrpc.ChannelPoint
	closeChannelFuncs []func()

	alice, bob, carol *lntest.HarnessNode
	nodes             []*lntest.HarnessNode
}

func newInterceptorTestContext(t *harnessTest,
	net *lntest.NetworkHarness,
	alice, bob, carol *lntest.HarnessNode) *interceptorTestContext {

	// Connect nodes
	nodes := []*lntest.HarnessNode{alice, bob, carol}
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			net.EnsureConnected(t.t, nodes[i], nodes[j])
		}
	}

	ctx := interceptorTestContext{
		t:     t,
		net:   net,
		alice: alice,
		bob:   bob,
		carol: carol,
		nodes: nodes,
	}

	return &ctx
}

// prepareTestCases prepares 4 tests:
// 1. failed htlc.
// 2. resumed htlc.
// 3. settling htlc externally.
// 4. held htlc that is resumed later.
func (c *interceptorTestContext) prepareTestCases() []*interceptorTestCase {
	cases := []*interceptorTestCase{
		{amountMsat: 1000, shouldHold: false,
			interceptorAction: routerrpc.ResolveHoldForwardAction_FAIL},
		{amountMsat: 1000, shouldHold: false,
			interceptorAction: routerrpc.ResolveHoldForwardAction_RESUME},
		{amountMsat: 1000, shouldHold: false,
			interceptorAction: routerrpc.ResolveHoldForwardAction_SETTLE},
		{amountMsat: 1000, shouldHold: true,
			interceptorAction: routerrpc.ResolveHoldForwardAction_RESUME},
	}

	for _, t := range cases {
		addResponse, err := c.carol.AddInvoice(context.Background(), &lnrpc.Invoice{
			ValueMsat: t.amountMsat,
		})
		require.NoError(c.t.t, err, "unable to add invoice")

		invoice, err := c.carol.LookupInvoice(context.Background(), &lnrpc.PaymentHash{
			RHashStr: hex.EncodeToString(addResponse.RHash),
		})
		require.NoError(c.t.t, err, "unable to find invoice")

		// We'll need to also decode the returned invoice so we can
		// grab the payment address which is now required for ALL
		// payments.
		payReq, err := c.carol.DecodePayReq(context.Background(), &lnrpc.PayReqString{
			PayReq: invoice.PaymentRequest,
		})
		require.NoError(c.t.t, err, "unable to decode invoice")

		t.invoice = invoice
		t.payAddr = payReq.PaymentAddr
	}
	return cases
}

func (c *interceptorTestContext) openChannel(from, to *lntest.HarnessNode,
	chanSize btcutil.Amount) {

	c.net.SendCoins(c.t.t, btcutil.SatoshiPerBitcoin, from)

	chanPoint := openChannelAndAssert(
		c.t, c.net, from, to,
		lntest.OpenChannelParams{
			Amt: chanSize,
		},
	)

	c.closeChannelFuncs = append(c.closeChannelFuncs, func() {
		closeChannelAndAssert(c.t, c.net, from, chanPoint, false)
	})

	c.networkChans = append(c.networkChans, chanPoint)
}

func (c *interceptorTestContext) closeChannels() {
	for _, f := range c.closeChannelFuncs {
		f()
	}
}

func (c *interceptorTestContext) waitForChannels() {
	// Wait for all nodes to have seen all channels.
	for _, chanPoint := range c.networkChans {
		for _, node := range c.nodes {
			txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			require.NoError(c.t.t, err, "unable to get txid")

			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			err = node.WaitForNetworkChannelOpen(chanPoint)
			require.NoError(c.t.t, err, fmt.Sprintf("(%d): timeout "+
				"waiting for channel(%s) open", node.NodeID,
				point))
		}
	}
}

// sendAliceToCarolPayment sends a payment from alice to carol and make an
// attempt to pay. The lnrpc.HTLCAttempt is returned.
func (c *interceptorTestContext) sendAliceToCarolPayment(ctx context.Context,
	amtMsat int64,
	paymentHash, paymentAddr []byte) (*lnrpc.HTLCAttempt, error) {

	// Build a route from alice to carol.
	route, err := c.buildRoute(
		ctx, amtMsat, []*lntest.HarnessNode{c.bob, c.carol},
		paymentAddr,
	)
	if err != nil {
		return nil, err
	}
	sendReq := &routerrpc.SendToRouteRequest{
		PaymentHash: paymentHash,
		Route:       route,
	}

	// Send a custom record to the forwarding node.
	route.Hops[0].CustomRecords = map[uint64][]byte{
		customTestKey: customTestValue,
	}

	// Send the payment.
	return c.alice.RouterClient.SendToRouteV2(ctx, sendReq)
}

// buildRoute is a helper function to build a route with given hops.
func (c *interceptorTestContext) buildRoute(ctx context.Context, amtMsat int64,
	hops []*lntest.HarnessNode, payAddr []byte) (*lnrpc.Route, error) {

	rpcHops := make([][]byte, 0, len(hops))
	for _, hop := range hops {
		k := hop.PubKeyStr
		pubkey, err := route.NewVertexFromStr(k)
		if err != nil {
			return nil, fmt.Errorf("error parsing %v: %v",
				k, err)
		}
		rpcHops = append(rpcHops, pubkey[:])
	}

	req := &routerrpc.BuildRouteRequest{
		AmtMsat:        amtMsat,
		FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
		HopPubkeys:     rpcHops,
		PaymentAddr:    payAddr,
	}

	routeResp, err := c.alice.RouterClient.BuildRoute(ctx, req)
	if err != nil {
		return nil, err
	}

	return routeResp.Route, nil
}

// interceptorTestScenario is a helper struct to hold the test context and
// provide the needed functionality.
type interceptorTestScenario struct {
	ht                *lntemp.HarnessTest
	alice, bob, carol *node.HarnessNode
}

// newInterceptorTestScenario initializes a new test scenario with three nodes
// and connects them to have the following topology,
//
//	Alice --> Bob --> Carol
//
// Among them, Alice and Bob are standby nodes and Carol is a new node.
func newInterceptorTestScenario(
	ht *lntemp.HarnessTest) *interceptorTestScenario {

	alice, bob := ht.Alice, ht.Bob
	carol := ht.NewNode("carol", nil)

	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	return &interceptorTestScenario{
		ht:    ht,
		alice: alice,
		bob:   bob,
		carol: carol,
	}
}

// prepareTestCases prepares 4 tests:
// 1. failed htlc.
// 2. resumed htlc.
// 3. settling htlc externally.
// 4. held htlc that is resumed later.
func (c *interceptorTestScenario) prepareTestCases() []*interceptorTestCase {
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
