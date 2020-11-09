package itest

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
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
	invoice           *lnrpc.Invoice
	shouldHold        bool
	interceptorAction routerrpc.ResolveHoldForwardAction
}

// testForwardInterceptor tests the forward interceptor RPC layer.
// The test creates a cluster of 3 connected nodes: Alice -> Bob -> Carol
// Alice sends 4 different payments to Carol while the interceptor handles
// differently the htlcs.
// The test ensures that:
// 1. Intercepted failed htlcs result in no payment (invoice is not settled).
// 2. Intercepted resumed htlcs result in a payment (invoice is settled).
// 3. Intercepted held htlcs result in no payment (invoice is not settled).
// 4. When Interceptor disconnects it resumes all held htlcs, which result in
//    valid payment (invoice is settled).
func testForwardInterceptor(net *lntest.NetworkHarness, t *harnessTest) {
	// initialize the test context with 3 connected nodes.
	testContext := newInterceptorTestContext(t, net)
	defer testContext.shutdownNodes()

	const (
		chanAmt = btcutil.Amount(300000)
	)

	// Open and wait for channels.
	testContext.openChannel(testContext.alice, testContext.bob, chanAmt)
	testContext.openChannel(testContext.bob, testContext.carol, chanAmt)
	defer testContext.closeChannels()
	testContext.waitForChannels()

	// Connect the interceptor.
	ctx := context.Background()
	ctxt, cancelInterceptor := context.WithTimeout(ctx, defaultTimeout)
	interceptor, err := testContext.bob.RouterClient.HtlcInterceptor(ctxt)
	if err != nil {
		t.Fatalf("failed to create HtlcInterceptor %v", err)
	}

	// Prepare the test cases.
	testCases, err := testContext.prepareTestCases()
	if err != nil {
		t.Fatalf("failed to prepare test cases")
	}

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
				t.t.Errorf("unexpected error in interceptor.Recv() %v", err)
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
				context.Background(), tc.invoice.ValueMsat, tc.invoice.RHash)

			if t.t.Failed() {
				return
			}
			if err != nil {
				t.t.Errorf("failed to send payment %v", err)
			}

			switch tc.interceptorAction {
			// For 'fail' interceptor action we make sure the payment failed.
			case routerrpc.ResolveHoldForwardAction_FAIL:
				if attempt.Status != lnrpc.HTLCAttempt_FAILED {
					t.t.Errorf("expected payment to fail, instead got %v", attempt.Status)
				}

			// For settle and resume we make sure the payment is successful.
			case routerrpc.ResolveHoldForwardAction_SETTLE:
				fallthrough

			case routerrpc.ResolveHoldForwardAction_RESUME:
				if attempt.Status != lnrpc.HTLCAttempt_SUCCEEDED {
					t.t.Errorf("expected payment to succeed, instead got %v", attempt.Status)
				}
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
	if err != nil {
		t.Fatalf("failed to fetch payments")
	}
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
			if foundPayment == nil {
				t.Fatalf("expected to find pending payment for held"+
					"htlc %v", hashStr)
			}
			if foundPayment.ValueMsat != expectedAmt ||
				foundPayment.Status != lnrpc.Payment_IN_FLIGHT {

				t.Fatalf("expected to find in flight payment for"+
					"amount %v, %v", testCase.invoice.ValueMsat, foundPayment.Status)
			}
		}
	}

	// Disconnect interceptor should cause resume held packets.
	// After that we wait for all go routines to finish, including the one
	// that tests the payment final status for the held payment.
	cancelInterceptor()
	wg.Wait()
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
	net *lntest.NetworkHarness) *interceptorTestContext {

	ctxb := context.Background()

	// Create a three-node context consisting of Alice, Bob and Carol
	carol, err := net.NewNode("carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol: %v", err)
	}

	// Connect nodes
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			if err := net.EnsureConnected(ctxt, nodes[i], nodes[j]); err != nil {
				t.Fatalf("unable to connect nodes: %v", err)
			}
		}
	}

	ctx := interceptorTestContext{
		t:     t,
		net:   net,
		alice: net.Alice,
		bob:   net.Bob,
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
func (c *interceptorTestContext) prepareTestCases() (
	[]*interceptorTestCase, error) {

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
		if err != nil {
			return nil, fmt.Errorf("unable to add invoice: %v", err)
		}
		invoice, err := c.carol.LookupInvoice(context.Background(), &lnrpc.PaymentHash{
			RHashStr: hex.EncodeToString(addResponse.RHash),
		})
		if err != nil {
			return nil, fmt.Errorf("unable to add invoice: %v", err)
		}
		t.invoice = invoice
	}
	return cases, nil
}

func (c *interceptorTestContext) openChannel(from, to *lntest.HarnessNode, chanSize btcutil.Amount) {
	ctxb := context.Background()

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := c.net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, from)
	if err != nil {
		c.t.Fatalf("unable to send coins : %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPoint := openChannelAndAssert(
		ctxt, c.t, c.net, from, to,
		lntest.OpenChannelParams{
			Amt: chanSize,
		},
	)

	c.closeChannelFuncs = append(c.closeChannelFuncs, func() {
		ctxt, _ := context.WithTimeout(ctxb, channelCloseTimeout)
		closeChannelAndAssert(
			ctxt, c.t, c.net, from, chanPoint, false,
		)
	})

	c.networkChans = append(c.networkChans, chanPoint)
}

func (c *interceptorTestContext) closeChannels() {
	for _, f := range c.closeChannelFuncs {
		f()
	}
}

func (c *interceptorTestContext) shutdownNodes() {
	shutdownAndAssert(c.net, c.t, c.carol)
}

func (c *interceptorTestContext) waitForChannels() {
	ctxb := context.Background()

	// Wait for all nodes to have seen all channels.
	for _, chanPoint := range c.networkChans {
		for _, node := range c.nodes {
			txid, err := lnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				c.t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				c.t.Fatalf("(%d): timeout waiting for "+
					"channel(%s) open: %v",
					node.NodeID, point, err)
			}
		}
	}
}

// sendAliceToCarolPayment sends a payment from alice to carol and make an
// attempt to pay. The lnrpc.HTLCAttempt is returned.
func (c *interceptorTestContext) sendAliceToCarolPayment(ctx context.Context,
	amtMsat int64, paymentHash []byte) (*lnrpc.HTLCAttempt, error) {

	// Build a route from alice to carol.
	route, err := c.buildRoute(ctx, amtMsat, []*lntest.HarnessNode{c.bob, c.carol})
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
func (c *interceptorTestContext) buildRoute(ctx context.Context, amtMsat int64, hops []*lntest.HarnessNode) (
	*lnrpc.Route, error) {

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
	}

	routeResp, err := c.alice.RouterClient.BuildRoute(ctx, req)
	if err != nil {
		return nil, err
	}

	return routeResp.Route, nil
}
