package routing

import (
	"crypto/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const stepTimeout = 5 * time.Second

// createTestRoute builds a route a->b->c paying the given amt to c.
func createTestRoute(amt lnwire.MilliSatoshi,
	aliasMap map[string]route.Vertex) (*route.Route, error) {

	hopFee := lnwire.NewMSatFromSatoshis(3)
	hop1 := aliasMap["b"]
	hop2 := aliasMap["c"]
	hops := []*route.Hop{
		{
			ChannelID:     1,
			PubKeyBytes:   hop1,
			LegacyPayload: true,
			AmtToForward:  amt + hopFee,
		},
		{
			ChannelID:     2,
			PubKeyBytes:   hop2,
			LegacyPayload: true,
			AmtToForward:  amt,
		},
	}

	// We create a simple route that we will supply every time the router
	// requests one.
	return route.NewRouteFromHops(
		amt+2*hopFee, 100, aliasMap["a"], hops,
	)
}

// paymentLifecycleTestCase contains the steps that we expect for a payment
// lifecycle test, and the routes that pathfinding should deliver.
type paymentLifecycleTestCase struct {
	name string

	// steps is a list of steps to perform during the testcase.
	steps []string

	// routes is the sequence of routes we will provide to the
	// router when it requests a new route.
	routes []*route.Route

	// paymentErr is the error we expect our payment to fail with. This
	// should be nil for tests with paymentSuccess steps and non-nil for
	// payments with paymentError steps.
	paymentErr error
}

const (
	// routerInitPayment is a test step where we expect the router
	// to call the InitPayment method on the control tower.
	routerInitPayment = "Router:init-payment"

	// routerRegisterAttempt is a test step where we expect the
	// router to call the RegisterAttempt method on the control
	// tower.
	routerRegisterAttempt = "Router:register-attempt"

	// routerSettleAttempt is a test step where we expect the
	// router to call the SettleAttempt method on the control
	// tower.
	routerSettleAttempt = "Router:settle-attempt"

	// routerFailAttempt is a test step where we expect the router
	// to call the FailAttempt method on the control tower.
	routerFailAttempt = "Router:fail-attempt"

	// routerFailPayment is a test step where we expect the router
	// to call the Fail method on the control tower.
	routerFailPayment = "Router:fail-payment"

	// routeRelease is a test step where we unblock pathfinding and
	// allow it to respond to our test with a route.
	routeRelease = "PaymentSession:release"

	// sendToSwitchSuccess is a step where we expect the router to
	// call send the payment attempt to the switch, and we will
	// respond with a non-error, indicating that the payment
	// attempt was successfully forwarded.
	sendToSwitchSuccess = "SendToSwitch:success"

	// sendToSwitchResultFailure is a step where we expect the
	// router to send the payment attempt to the switch, and we
	// will respond with a forwarding error. This can happen when
	// forwarding fail on our local links.
	sendToSwitchResultFailure = "SendToSwitch:failure"

	// getPaymentResultSuccess is a test step where we expect the
	// router to call the GetPaymentResult method, and we will
	// respond with a successful payment result.
	getPaymentResultSuccess = "GetPaymentResult:success"

	// getPaymentResultTempFailure is a test step where we expect the
	// router to call the GetPaymentResult method, and we will
	// respond with a forwarding error, expecting the router to retry.
	getPaymentResultTempFailure = "GetPaymentResult:temp-failure"

	// getPaymentResultTerminalFailure is a test step where we
	// expect the router to call the GetPaymentResult method, and
	// we will respond with a terminal error, expecting the router
	// to stop making payment attempts.
	getPaymentResultTerminalFailure = "GetPaymentResult:terminal-failure"

	// resendPayment is a test step where we manually try to resend
	// the same payment, making sure the router responds with an
	// error indicating that it is already in flight.
	resendPayment = "ResendPayment"

	// startRouter is a step where we manually start the router,
	// used to test that it automatically will resume payments at
	// startup.
	startRouter = "StartRouter"

	// stopRouter is a test step where we manually make the router
	// shut down.
	stopRouter = "StopRouter"

	// paymentSuccess is a step where assert that we receive a
	// successful result for the original payment made.
	paymentSuccess = "PaymentSuccess"

	// paymentError is a step where assert that we receive an error
	// for the original payment made.
	paymentError = "PaymentError"

	// resentPaymentSuccess is a step where assert that we receive
	// a successful result for a payment that was resent.
	resentPaymentSuccess = "ResentPaymentSuccess"

	// resentPaymentError is a step where assert that we receive an
	// error for a payment that was resent.
	resentPaymentError = "ResentPaymentError"
)

// TestRouterPaymentStateMachine tests that the router interacts as expected
// with the ControlTower during a payment lifecycle, such that it payment
// attempts are not sent twice to the switch, and results are handled after a
// restart.
func TestRouterPaymentStateMachine(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101

	// Setup two simple channels such that we can mock sending along this
	// route.
	chanCapSat := btcutil.Amount(100000)
	testChannels := []*testChannel{
		symmetricTestChannel("a", "b", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 1),
		symmetricTestChannel("b", "c", chanCapSat, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: lnwire.NewMSatFromSatoshis(chanCapSat),
		}, 2),
	}

	testGraph, err := createTestGraphFromChannels(testChannels, "a")
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraph.cleanUp()

	paymentAmt := lnwire.NewMSatFromSatoshis(1000)

	// We create a simple route that we will supply every time the router
	// requests one.
	rt, err := createTestRoute(paymentAmt, testGraph.aliasMap)
	if err != nil {
		t.Fatalf("unable to create route: %v", err)
	}

	halfShard, err := createTestRoute(paymentAmt/2, testGraph.aliasMap)
	require.NoError(t, err, "unable to create half route")

	shard, err := createTestRoute(paymentAmt/4, testGraph.aliasMap)
	if err != nil {
		t.Fatalf("unable to create route: %v", err)
	}

	tests := []paymentLifecycleTestCase{
		{
			// Tests a normal payment flow that succeeds.
			name: "single shot success",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,
				getPaymentResultSuccess,
				routerSettleAttempt,
				paymentSuccess,
			},
			routes: []*route.Route{rt},
		},
		{
			// A payment flow with a failure on the first attempt,
			// but that succeeds on the second attempt.
			name: "single shot retry",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the first sent attempt fail.
				getPaymentResultTempFailure,
				routerFailAttempt,

				// The router should retry.
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the second sent attempt succeed.
				getPaymentResultSuccess,
				routerSettleAttempt,
				paymentSuccess,
			},
			routes: []*route.Route{rt, rt},
		},
		{
			// A payment flow with a forwarding failure first time
			// sending to the switch, but that succeeds on the
			// second attempt.
			name: "single shot switch failure",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,

				// Make the first sent attempt fail.
				sendToSwitchResultFailure,
				routerFailAttempt,

				// The router should retry.
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the second sent attempt succeed.
				getPaymentResultSuccess,
				routerSettleAttempt,
				paymentSuccess,
			},
			routes: []*route.Route{rt, rt},
		},
		{
			// A payment that fails on the first attempt, and has
			// only one route available to try. It will therefore
			// fail permanently.
			name: "single shot route fails",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Make the first sent attempt fail.
				getPaymentResultTempFailure,
				routerFailAttempt,

				routeRelease,

				// Since there are no more routes to try, the
				// payment should fail.
				routerFailPayment,
				paymentError,
			},
			routes:     []*route.Route{rt},
			paymentErr: channeldb.FailureReasonNoRoute,
		},
		{
			// We expect the payment to fail immediately if we have
			// no routes to try.
			name: "single shot no route",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerFailPayment,
				paymentError,
			},
			routes:     []*route.Route{},
			paymentErr: channeldb.FailureReasonNoRoute,
		},
		{
			// A normal payment flow, where we attempt to resend
			// the same payment after each step. This ensures that
			// the router don't attempt to resend a payment already
			// in flight.
			name: "single shot resend",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,

				// Manually resend the payment, the router
				// should attempt to init with the control
				// tower, but fail since it is already in
				// flight.
				resendPayment,
				routerInitPayment,
				resentPaymentError,

				// The original payment should proceed as
				// normal.
				sendToSwitchSuccess,

				// Again resend the payment and assert it's not
				// allowed.
				resendPayment,
				routerInitPayment,
				resentPaymentError,

				// Notify about a success for the original
				// payment.
				getPaymentResultSuccess,
				routerSettleAttempt,

				// Now that the original payment finished,
				// resend it again to ensure this is not
				// allowed.
				resendPayment,
				routerInitPayment,
				resentPaymentError,
				paymentSuccess,
			},
			routes: []*route.Route{rt},
		},
		{
			// Tests that the router is able to handle the
			// receieved payment result after a restart.
			name: "single shot restart",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Shut down the router. The original caller
				// should get notified about this.
				stopRouter,
				paymentError,

				// Start the router again, and ensure the
				// router registers the success with the
				// control tower.
				startRouter,
				getPaymentResultSuccess,
				routerSettleAttempt,
			},
			routes:     []*route.Route{rt},
			paymentErr: ErrRouterShuttingDown,
		},
		{
			// Tests that we are allowed to resend a payment after
			// it has permanently failed.
			name: "single shot resend fail",

			steps: []string{
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// Resending the payment at this stage should
				// not be allowed.
				resendPayment,
				routerInitPayment,
				resentPaymentError,

				// Make the first attempt fail.
				getPaymentResultTempFailure,
				routerFailAttempt,

				// Since we have no more routes to try, the
				// original payment should fail.
				routeRelease,
				routerFailPayment,
				paymentError,

				// Now resend the payment again. This should be
				// allowed, since the payment has failed.
				resendPayment,
				routerInitPayment,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,
				getPaymentResultSuccess,
				routerSettleAttempt,
				resentPaymentSuccess,
			},
			routes:     []*route.Route{rt},
			paymentErr: channeldb.FailureReasonNoRoute,
		},

		// =====================================
		// ||          MPP scenarios          ||
		// =====================================
		{
			// Tests a simple successful MP payment of 4 shards.
			name: "MP success",

			steps: []string{
				routerInitPayment,

				// shard 0
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 1
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 2
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 3
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// All shards succeed.
				getPaymentResultSuccess,
				getPaymentResultSuccess,
				getPaymentResultSuccess,
				getPaymentResultSuccess,

				// Router should settle them all.
				routerSettleAttempt,
				routerSettleAttempt,
				routerSettleAttempt,
				routerSettleAttempt,

				// And the final result is obviously
				// successful.
				paymentSuccess,
			},
			routes: []*route.Route{shard, shard, shard, shard},
		},
		{
			// An MP payment scenario where we need several extra
			// attempts before the payment finally settle.
			name: "MP failed shards",

			steps: []string{
				routerInitPayment,

				// shard 0
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 1
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 2
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 3
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// First two shards fail, two new ones are sent.
				getPaymentResultTempFailure,
				getPaymentResultTempFailure,
				routerFailAttempt,
				routerFailAttempt,

				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// The four shards settle.
				getPaymentResultSuccess,
				getPaymentResultSuccess,
				getPaymentResultSuccess,
				getPaymentResultSuccess,
				routerSettleAttempt,
				routerSettleAttempt,
				routerSettleAttempt,
				routerSettleAttempt,

				// Overall payment succeeds.
				paymentSuccess,
			},
			routes: []*route.Route{
				shard, shard, shard, shard, shard, shard,
			},
		},
		{
			// An MP payment scenario where one of the shards fails,
			// but we still receive a single success shard.
			name: "MP one shard success",

			steps: []string{
				routerInitPayment,

				// shard 0
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 1
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 0 fails, and should be failed by the
				// router.
				getPaymentResultTempFailure,
				routerFailAttempt,

				// We will try one more shard because we haven't
				// sent the full payment amount.
				routeRelease,

				// The second shard succeed against all odds,
				// making the overall payment succeed.
				getPaymentResultSuccess,
				routerSettleAttempt,
				paymentSuccess,
			},
			routes: []*route.Route{halfShard, halfShard},
		},
		{
			// An MP payment scenario a shard fail with a terminal
			// error, causing the router to stop attempting.
			name: "MP terminal",

			steps: []string{
				routerInitPayment,

				// shard 0
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 1
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 2
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 3
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// The first shard fail with a terminal error.
				getPaymentResultTerminalFailure,
				routerFailAttempt,
				routerFailPayment,

				// Remaining 3 shards fail.
				getPaymentResultTempFailure,
				getPaymentResultTempFailure,
				getPaymentResultTempFailure,
				routerFailAttempt,
				routerFailAttempt,
				routerFailAttempt,

				// Payment fails.
				paymentError,
			},
			routes: []*route.Route{
				shard, shard, shard, shard, shard, shard,
			},
			paymentErr: channeldb.FailureReasonPaymentDetails,
		},
		{
			// A MP payment scenario when our path finding returns
			// after we've just received a terminal failure, and
			// attempts to dispatch a new shard. Testing that we
			// correctly abandon the shard and conclude the payment.
			name: "MP path found after failure",

			steps: []string{
				routerInitPayment,

				// shard 0
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// The first shard fail with a terminal error.
				getPaymentResultTerminalFailure,
				routerFailAttempt,
				routerFailPayment,

				// shard 1 fails because we've had a terminal
				// failure.
				routeRelease,
				routerRegisterAttempt,

				// Payment fails.
				paymentError,
			},
			routes: []*route.Route{
				shard, shard,
			},
			paymentErr: channeldb.FailureReasonPaymentDetails,
		},
		{
			// A MP payment scenario when our path finding returns
			// after we've just received a terminal failure, and
			// we have another shard still in flight.
			name: "MP shard in flight after terminal",

			steps: []string{
				routerInitPayment,

				// shard 0
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 1
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// shard 2
				routeRelease,
				routerRegisterAttempt,
				sendToSwitchSuccess,

				// We find a path for another shard.
				routeRelease,

				// shard 0 fails with a terminal error.
				getPaymentResultTerminalFailure,
				routerFailAttempt,
				routerFailPayment,

				// We try to register our final shard after
				// processing a terminal failure.
				routerRegisterAttempt,

				// Our in-flight shards fail.
				getPaymentResultTempFailure,
				getPaymentResultTempFailure,
				routerFailAttempt,
				routerFailAttempt,

				// Payment fails.
				paymentError,
			},
			routes: []*route.Route{
				shard, shard, shard, shard,
			},
			paymentErr: channeldb.FailureReasonPaymentDetails,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testPaymentLifecycle(
				t, test, paymentAmt, startingBlockHeight,
				testGraph,
			)
		})
	}
}

func testPaymentLifecycle(t *testing.T, test paymentLifecycleTestCase,
	paymentAmt lnwire.MilliSatoshi, startingBlockHeight uint32,
	testGraph *testGraphInstance) {

	// Create a mock control tower with channels set up, that we use to
	// synchronize and listen for events.
	control := makeMockControlTower()
	control.init = make(chan initArgs)
	control.registerAttempt = make(chan registerAttemptArgs)
	control.settleAttempt = make(chan settleAttemptArgs)
	control.failAttempt = make(chan failAttemptArgs)
	control.failPayment = make(chan failPaymentArgs)
	control.fetchInFlight = make(chan struct{})

	// setupRouter is a helper method that creates and starts the router in
	// the desired configuration for this test.
	setupRouter := func() (*ChannelRouter, chan error,
		chan *htlcswitch.PaymentResult) {

		chain := newMockChain(startingBlockHeight)
		chainView := newMockChainView(chain)

		// We set uo the use the following channels and a mock Payer to
		// synchonize with the interaction to the Switch.
		sendResult := make(chan error)
		paymentResult := make(chan *htlcswitch.PaymentResult)

		payer := &mockPayer{
			sendResult:    sendResult,
			paymentResult: paymentResult,
		}

		router, err := New(Config{
			Graph:              testGraph.graph,
			Chain:              chain,
			ChainView:          chainView,
			Control:            control,
			SessionSource:      &mockPaymentSessionSource{},
			MissionControl:     &mockMissionControl{},
			Payer:              payer,
			ChannelPruneExpiry: time.Hour * 24,
			GraphPruneInterval: time.Hour * 2,
			QueryBandwidth: func(e *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi {
				return lnwire.NewMSatFromSatoshis(e.Capacity)
			},
			NextPaymentID: func() (uint64, error) {
				next := atomic.AddUint64(&uniquePaymentID, 1)
				return next, nil
			},
			Clock: clock.NewTestClock(time.Unix(1, 0)),
		})
		if err != nil {
			t.Fatalf("unable to create router %v", err)
		}

		// On startup, the router should fetch all pending payments
		// from the ControlTower, so assert that here.
		errCh := make(chan error)
		go func() {
			close(errCh)
			select {
			case <-control.fetchInFlight:
				return
			case <-time.After(1 * time.Second):
				errCh <- errors.New("router did not fetch in flight " +
					"payments")
			}
		}()

		if err := router.Start(); err != nil {
			t.Fatalf("unable to start router: %v", err)
		}

		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("error in anonymous goroutine: %s", err)
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("did not fetch in flight payments at startup")
		}

		return router, sendResult, paymentResult
	}

	router, sendResult, getPaymentResult := setupRouter()
	defer func() {
		if err := router.Stop(); err != nil {
			t.Fatal(err)
		}
	}()

	// Craft a LightningPayment struct.
	var preImage lntypes.Preimage
	if _, err := rand.Read(preImage[:]); err != nil {
		t.Fatalf("unable to generate preimage")
	}

	payHash := preImage.Hash()

	payment := LightningPayment{
		Target:      testGraph.aliasMap["c"],
		Amount:      paymentAmt,
		FeeLimit:    noFeeLimit,
		paymentHash: &payHash,
	}

	// Setup our payment session source to block on release of
	// routes.
	routeChan := make(chan struct{})
	router.cfg.SessionSource = &mockPaymentSessionSource{
		routes:       test.routes,
		routeRelease: routeChan,
	}

	router.cfg.MissionControl = &mockMissionControl{}

	// Send the payment. Since this is new payment hash, the
	// information should be registered with the ControlTower.
	paymentResult := make(chan error)
	done := make(chan struct{})
	go func() {
		_, _, err := router.SendPayment(&payment)
		paymentResult <- err
		close(done)
	}()

	var resendResult chan error
	for _, step := range test.steps {
		switch step {

		case routerInitPayment:
			var args initArgs
			select {
			case args = <-control.init:
			case <-time.After(stepTimeout):
				t.Fatalf("no init payment with control")
			}

			if args.c == nil {
				t.Fatalf("expected non-nil CreationInfo")
			}

		case routeRelease:
			select {
			case <-routeChan:

			case <-time.After(stepTimeout):
				t.Fatalf("no route requested")
			}

		// In this step we expect the router to make a call to
		// register a new attempt with the ControlTower.
		case routerRegisterAttempt:
			var args registerAttemptArgs
			select {
			case args = <-control.registerAttempt:
			case <-time.After(stepTimeout):
				t.Fatalf("attempt not registered " +
					"with control")
			}

			if args.a == nil {
				t.Fatalf("expected non-nil AttemptInfo")
			}

		// In this step we expect the router to call the
		// ControlTower's SettleAttempt method with the preimage.
		case routerSettleAttempt:
			select {
			case <-control.settleAttempt:
			case <-time.After(stepTimeout):
				t.Fatalf("attempt settle not " +
					"registered with control")
			}

		// In this step we expect the router to call the
		// ControlTower's FailAttempt method with a HTLC fail
		// info.
		case routerFailAttempt:
			select {
			case <-control.failAttempt:
			case <-time.After(stepTimeout):
				t.Fatalf("attempt fail not " +
					"registered with control")
			}

		// In this step we expect the router to call the
		// ControlTower's Fail method, to indicate that the
		// payment failed.
		case routerFailPayment:
			select {
			case <-control.failPayment:
			case <-time.After(stepTimeout):
				t.Fatalf("payment fail not " +
					"registered with control")
			}

		// In this step we expect the SendToSwitch method to be
		// called, and we respond with a nil-error.
		case sendToSwitchSuccess:
			select {
			case sendResult <- nil:
			case <-time.After(stepTimeout):
				t.Fatalf("unable to send result")
			}

		// In this step we expect the SendToSwitch method to be
		// called, and we respond with a forwarding error
		case sendToSwitchResultFailure:
			select {
			case sendResult <- htlcswitch.NewForwardingError(
				&lnwire.FailTemporaryChannelFailure{},
				1,
			):
			case <-time.After(stepTimeout):
				t.Fatalf("unable to send result")
			}

		// In this step we expect the GetPaymentResult method
		// to be called, and we respond with the preimage to
		// complete the payment.
		case getPaymentResultSuccess:
			select {
			case getPaymentResult <- &htlcswitch.PaymentResult{
				Preimage: preImage,
			}:
			case <-time.After(stepTimeout):
				t.Fatalf("unable to send result")
			}

		// In this state we expect the GetPaymentResult method
		// to be called, and we respond with a forwarding
		// error, indicating that the router should retry.
		case getPaymentResultTempFailure:
			failure := htlcswitch.NewForwardingError(
				&lnwire.FailTemporaryChannelFailure{},
				1,
			)

			select {
			case getPaymentResult <- &htlcswitch.PaymentResult{
				Error: failure,
			}:
			case <-time.After(stepTimeout):
				t.Fatalf("unable to get result")
			}

		// In this state we expect the router to call the
		// GetPaymentResult method, and we will respond with a
		// terminal error, indiating the router should stop
		// making payment attempts.
		case getPaymentResultTerminalFailure:
			failure := htlcswitch.NewForwardingError(
				&lnwire.FailIncorrectDetails{},
				1,
			)

			select {
			case getPaymentResult <- &htlcswitch.PaymentResult{
				Error: failure,
			}:
			case <-time.After(stepTimeout):
				t.Fatalf("unable to get result")
			}

		// In this step we manually try to resend the same
		// payment, making sure the router responds with an
		// error indicating that it is already in flight.
		case resendPayment:
			resendResult = make(chan error)
			go func() {
				_, _, err := router.SendPayment(&payment)
				resendResult <- err
			}()

		// In this step we manually stop the router.
		case stopRouter:
			// On shutdown, the switch closes our result channel.
			// Mimic this behavior in our mock.
			close(getPaymentResult)

			if err := router.Stop(); err != nil {
				t.Fatalf("unable to restart: %v", err)
			}

		// In this step we manually start the router.
		case startRouter:
			router, sendResult, getPaymentResult = setupRouter()

		// In this state we expect to receive an error for the
		// original payment made.
		case paymentError:
			require.Error(t, test.paymentErr,
				"paymentError not set")

			select {
			case err := <-paymentResult:
				require.Equal(t, test.paymentErr, err)

			case <-time.After(stepTimeout):
				t.Fatalf("got no payment result")
			}

		// In this state we expect the original payment to
		// succeed.
		case paymentSuccess:
			require.Nil(t, test.paymentErr)

			select {
			case err := <-paymentResult:
				if err != nil {
					t.Fatalf("did not expect "+
						"error %v", err)
				}

			case <-time.After(stepTimeout):
				t.Fatalf("got no payment result")
			}

		// In this state we expect to receive an error for the
		// resent payment made.
		case resentPaymentError:
			select {
			case err := <-resendResult:
				if err == nil {
					t.Fatalf("expected error")
				}

			case <-time.After(stepTimeout):
				t.Fatalf("got no payment result")
			}

		// In this state we expect the resent payment to
		// succeed.
		case resentPaymentSuccess:
			select {
			case err := <-resendResult:
				if err != nil {
					t.Fatalf("did not expect error %v", err)
				}

			case <-time.After(stepTimeout):
				t.Fatalf("got no payment result")
			}

		default:
			t.Fatalf("unknown step %v", step)
		}
	}

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatalf("SendPayment didn't exit")
	}
}
