package routing

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// errDummy is used by the mockers to return a dummy error.
var errDummy = errors.New("dummy")

// createTestPaymentLifecycle creates a `paymentLifecycle` without mocks.
func createTestPaymentLifecycle() *paymentLifecycle {
	paymentHash := lntypes.Hash{1, 2, 3}
	quitChan := make(chan struct{})
	rt := &ChannelRouter{
		cfg:  &Config{},
		quit: quitChan,
	}

	return &paymentLifecycle{
		router:     rt,
		identifier: paymentHash,
	}
}

// mockers wraps a list of mocked interfaces used inside payment lifecycle.
type mockers struct {
	shard          *mockShard
	shardTracker   *mockShardTracker
	control        *mockControlTower
	paySession     *mockPaymentSession
	payer          *mockPaymentAttemptDispatcher
	clock          *lnmock.MockClock
	missionControl *mockMissionControl

	// collectResultsCount is the number of times the collectResultAsync
	// has been called.
	collectResultsCount int

	// payment is the mocked `dbMPPayment` used in the test.
	payment *mockMPPayment
}

// newTestPaymentLifecycle creates a `paymentLifecycle` using the mocks. It
// also asserts the mockers are called as expected when the test is finished.
func newTestPaymentLifecycle(t *testing.T) (*paymentLifecycle, *mockers) {
	paymentHash := lntypes.Hash{1, 2, 3}
	quitChan := make(chan struct{})

	// Create a mock shard to be return from `NewShard`.
	mockShard := &mockShard{}

	// Create a list of mocks and add it to the router config.
	mockControlTower := &mockControlTower{}
	mockPayer := &mockPaymentAttemptDispatcher{}
	mockClock := &lnmock.MockClock{}
	mockMissionControl := &mockMissionControl{}

	// Make a channel router.
	rt := &ChannelRouter{
		cfg: &Config{
			Control:        mockControlTower,
			Payer:          mockPayer,
			Clock:          mockClock,
			MissionControl: mockMissionControl,
		},
		quit: quitChan,
	}

	// Create mockers to init a payment lifecycle.
	mockPaymentSession := &mockPaymentSession{}
	mockShardTracker := &mockShardTracker{}

	// Create a test payment lifecycle with no fee limit and no timeout.
	p := newPaymentLifecycle(
		rt, noFeeLimit, paymentHash, mockPaymentSession,
		mockShardTracker, 0,
	)

	// Create a mock payment which is returned from mockControlTower.
	mockPayment := &mockMPPayment{}

	mockers := &mockers{
		shard:          mockShard,
		shardTracker:   mockShardTracker,
		control:        mockControlTower,
		paySession:     mockPaymentSession,
		payer:          mockPayer,
		clock:          mockClock,
		missionControl: mockMissionControl,
		payment:        mockPayment,
	}

	// Overwrite the collectResultAsync to focus on testing the payment
	// lifecycle within the goroutine.
	resultCollector := func(attempt *channeldb.HTLCAttempt) {
		mockers.collectResultsCount++
	}
	p.resultCollector = resultCollector

	// Validate the mockers are called as expected before exiting the test.
	t.Cleanup(func() {
		mockShard.AssertExpectations(t)
		mockShardTracker.AssertExpectations(t)
		mockControlTower.AssertExpectations(t)
		mockPaymentSession.AssertExpectations(t)
		mockPayer.AssertExpectations(t)
		mockClock.AssertExpectations(t)
		mockMissionControl.AssertExpectations(t)
		mockPayment.AssertExpectations(t)
	})

	return p, mockers
}

// setupTestPaymentLifecycle creates a new `paymentLifecycle` and mocks the
// initial steps of the payment lifecycle so we can enter into the loop
// directly.
func setupTestPaymentLifecycle(t *testing.T) (*paymentLifecycle, *mockers) {
	p, m := newTestPaymentLifecycle(t)

	// Mock the first two calls.
	m.control.On("FetchPayment", p.identifier).Return(
		m.payment, nil,
	).Once()

	htlcs := []channeldb.HTLCAttempt{}
	m.payment.On("InFlightHTLCs").Return(htlcs).Once()

	return p, m
}

// resumePaymentResult is used to hold the returned values from
// `resumePayment`.
type resumePaymentResult struct {
	preimage lntypes.Hash
	err      error
}

// sendPaymentAndAssertError calls `resumePayment` and asserts that an error is
// returned.
func sendPaymentAndAssertError(t *testing.T, ctx context.Context,
	p *paymentLifecycle, errExpected error) {

	resultChan := make(chan *resumePaymentResult, 1)

	// We now make a call to `resumePayment` and expect it to return the
	// error.
	go func() {
		preimage, _, err := p.resumePayment(ctx)
		resultChan <- &resumePaymentResult{
			preimage: preimage,
			err:      err,
		}
	}()

	// Validate the returned values or timeout.
	select {
	case r := <-resultChan:
		require.ErrorIs(t, r.err, errExpected, "expected error")
		require.Empty(t, r.preimage, "preimage should be empty")

	case <-time.After(testTimeout):
		require.Fail(t, "timeout waiting for result")
	}
}

// sendPaymentAndAssertSucceeded calls `resumePayment` and asserts that the
// returned preimage is correct.
func sendPaymentAndAssertSucceeded(t *testing.T,
	p *paymentLifecycle, expected lntypes.Preimage) {

	resultChan := make(chan *resumePaymentResult, 1)

	// We now make a call to `resumePayment` and expect it to return the
	// preimage.
	go func() {
		preimage, _, err := p.resumePayment(context.Background())
		resultChan <- &resumePaymentResult{
			preimage: preimage,
			err:      err,
		}
	}()

	// Validate the returned values or timeout.
	select {
	case r := <-resultChan:
		require.NoError(t, r.err, "unexpected error")
		require.EqualValues(t, expected, r.preimage,
			"preimage not match")

	case <-time.After(testTimeout):
		require.Fail(t, "timeout waiting for result")
	}
}

// createDummyRoute builds a route a->b->c paying the given amt to c.
func createDummyRoute(t *testing.T, amt lnwire.MilliSatoshi) *route.Route {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "failed to create private key")
	hop1 := route.NewVertex(priv.PubKey())

	priv, err = btcec.NewPrivateKey()
	require.NoError(t, err, "failed to create private key")
	hop2 := route.NewVertex(priv.PubKey())

	hopFee := lnwire.NewMSatFromSatoshis(3)
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

	priv, err = btcec.NewPrivateKey()
	require.NoError(t, err, "failed to create private key")
	source := route.NewVertex(priv.PubKey())

	// We create a simple route that we will supply every time the router
	// requests one.
	rt, err := route.NewRouteFromHops(amt+2*hopFee, 100, source, hops)
	require.NoError(t, err, "failed to create route")

	return rt
}

func makeSettledAttempt(t *testing.T, total int,
	preimage lntypes.Preimage) *channeldb.HTLCAttempt {

	return &channeldb.HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(t, total),
		Settle:          &channeldb.HTLCSettleInfo{Preimage: preimage},
	}
}

func makeFailedAttempt(t *testing.T, total int) *channeldb.HTLCAttempt {
	return &channeldb.HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(t, total),
		Failure: &channeldb.HTLCFailInfo{
			Reason: channeldb.HTLCFailInternal,
		},
	}
}

func makeAttemptInfo(t *testing.T, amt int) channeldb.HTLCAttemptInfo {
	rt := createDummyRoute(t, lnwire.MilliSatoshi(amt))
	return channeldb.HTLCAttemptInfo{
		Route: *rt,
	}
}

// TestCheckTimeoutTimedOut checks that when the payment times out, it is
// marked as failed.
func TestCheckTimeoutTimedOut(t *testing.T) {
	t.Parallel()

	deadline := time.Now().Add(time.Nanosecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	p := createTestPaymentLifecycle()

	// Mock the control tower's `FailPayment` method.
	ct := &mockControlTower{}
	ct.On("FailPayment",
		p.identifier, channeldb.FailureReasonTimeout).Return(nil)

	// Mount the mocked control tower.
	p.router.cfg.Control = ct

	// Sleep one millisecond to make sure it timed out.
	time.Sleep(1 * time.Millisecond)

	// Call the function and expect no error.
	err := p.checkContext(ctx)
	require.NoError(t, err)

	// Assert that `FailPayment` is called as expected.
	ct.AssertExpectations(t)

	// We now test that when `FailPayment` returns an error, it's returned
	// by the function too.
	//
	// Mock `FailPayment` to return a dummy error.
	ct = &mockControlTower{}
	ct.On("FailPayment",
		p.identifier, channeldb.FailureReasonTimeout).Return(errDummy)

	// Mount the mocked control tower.
	p.router.cfg.Control = ct

	// Make the timeout happens instantly.
	deadline = time.Now().Add(time.Nanosecond)
	ctx, cancel = context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// Sleep one millisecond to make sure it timed out.
	time.Sleep(1 * time.Millisecond)

	// Call the function and expect an error.
	err = p.checkContext(ctx)
	require.ErrorIs(t, err, errDummy)

	// Assert that `FailPayment` is called as expected.
	ct.AssertExpectations(t)
}

// TestCheckTimeoutOnRouterQuit checks that when the router has quit, an error
// is returned from checkTimeout.
func TestCheckTimeoutOnRouterQuit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := createTestPaymentLifecycle()

	close(p.router.quit)
	err := p.checkContext(ctx)
	require.ErrorIs(t, err, ErrRouterShuttingDown)
}

// TestRequestRouteSucceed checks that `requestRoute` can successfully request
// a route.
func TestRequestRouteSucceed(t *testing.T) {
	t.Parallel()

	p := createTestPaymentLifecycle()

	// Create a mock payment session and a dummy route.
	paySession := &mockPaymentSession{}
	dummyRoute := &route.Route{}

	// Mount the mocked payment session.
	p.paySession = paySession

	// Create a dummy payment state.
	ps := &channeldb.MPPaymentState{
		NumAttemptsInFlight: 1,
		RemainingAmt:        1,
		FeesPaid:            100,
	}

	// Mock remainingFees to be 1.
	p.feeLimit = ps.FeesPaid + 1

	// Mock the paySession's `RequestRoute` method to return no error.
	paySession.On("RequestRoute",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything,
	).Return(dummyRoute, nil)

	result, err := p.requestRoute(ps)
	require.NoError(t, err, "expect no error")
	require.Equal(t, dummyRoute, result, "returned route not matched")

	// Assert that `RequestRoute` is called as expected.
	paySession.AssertExpectations(t)
}

// TestRequestRouteHandleCriticalErr checks that `requestRoute` can
// successfully handle a critical error returned from payment session.
func TestRequestRouteHandleCriticalErr(t *testing.T) {
	t.Parallel()

	p := createTestPaymentLifecycle()

	// Create a mock payment session.
	paySession := &mockPaymentSession{}

	// Mount the mocked payment session.
	p.paySession = paySession

	// Create a dummy payment state.
	ps := &channeldb.MPPaymentState{
		NumAttemptsInFlight: 1,
		RemainingAmt:        1,
		FeesPaid:            100,
	}

	// Mock remainingFees to be 1.
	p.feeLimit = ps.FeesPaid + 1

	// Mock the paySession's `RequestRoute` method to return an error.
	paySession.On("RequestRoute",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything,
	).Return(nil, errDummy)

	result, err := p.requestRoute(ps)

	// Expect an error is returned since it's critical.
	require.ErrorIs(t, err, errDummy, "error not matched")
	require.Nil(t, result, "expected no route returned")

	// Assert that `RequestRoute` is called as expected.
	paySession.AssertExpectations(t)
}

// TestRequestRouteHandleNoRouteErr checks that `requestRoute` can successfully
// handle the `noRouteError` returned from payment session.
func TestRequestRouteHandleNoRouteErr(t *testing.T) {
	t.Parallel()

	// Create a paymentLifecycle with mockers.
	p, m := newTestPaymentLifecycle(t)

	// Create a dummy payment state.
	ps := &channeldb.MPPaymentState{
		NumAttemptsInFlight: 1,
		RemainingAmt:        1,
		FeesPaid:            100,
	}

	// Mock remainingFees to be 1.
	p.feeLimit = ps.FeesPaid + 1

	// Mock the paySession's `RequestRoute` method to return a NoRouteErr
	// type.
	m.paySession.On("RequestRoute",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything,
	).Return(nil, errNoTlvPayload)

	// The payment should be failed with reason no route.
	m.control.On("FailPayment",
		p.identifier, channeldb.FailureReasonNoRoute,
	).Return(nil).Once()

	result, err := p.requestRoute(ps)

	// Expect no error is returned since it's not critical.
	require.NoError(t, err, "expected no error")
	require.Nil(t, result, "expected no route returned")
}

// TestRequestRouteFailPaymentError checks that `requestRoute` returns the
// error from calling `FailPayment`.
func TestRequestRouteFailPaymentError(t *testing.T) {
	t.Parallel()

	p := createTestPaymentLifecycle()

	// Create a mock payment session.
	paySession := &mockPaymentSession{}

	// Mock the control tower's `FailPayment` method.
	ct := &mockControlTower{}
	ct.On("FailPayment",
		p.identifier, errNoTlvPayload.FailureReason(),
	).Return(errDummy)

	// Mount the mocked control tower and payment session.
	p.router.cfg.Control = ct
	p.paySession = paySession

	// Create a dummy payment state with zero inflight attempts.
	ps := &channeldb.MPPaymentState{
		NumAttemptsInFlight: 0,
		RemainingAmt:        1,
		FeesPaid:            100,
	}

	// Mock remainingFees to be 1.
	p.feeLimit = ps.FeesPaid + 1

	// Mock the paySession's `RequestRoute` method to return an error.
	paySession.On("RequestRoute",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything,
	).Return(nil, errNoTlvPayload)

	result, err := p.requestRoute(ps)

	// Expect an error is returned.
	require.ErrorIs(t, err, errDummy, "error not matched")
	require.Nil(t, result, "expected no route returned")

	// Assert that `RequestRoute` is called as expected.
	paySession.AssertExpectations(t)

	// Assert that `FailPayment` is called as expected.
	ct.AssertExpectations(t)
}

// TestDecideNextStep checks the method `decideNextStep` behaves as expected.
func TestDecideNextStep(t *testing.T) {
	t.Parallel()

	// mockReturn is used to hold the return values from AllowMoreAttempts
	// or NeedWaitAttempts.
	type mockReturn struct {
		allowOrWait bool
		err         error
	}

	testCases := []struct {
		name              string
		allowMoreAttempts *mockReturn
		needWaitAttempts  *mockReturn

		// When the attemptResultChan has returned.
		closeResultChan bool

		// Whether the router has quit.
		routerQuit bool

		expectedStep stateStep
		expectedErr  error
	}{
		{
			name:              "allow more attempts",
			allowMoreAttempts: &mockReturn{true, nil},
			expectedStep:      stepProceed,
			expectedErr:       nil,
		},
		{
			name:              "error on allow more attempts",
			allowMoreAttempts: &mockReturn{false, errDummy},
			expectedStep:      stepExit,
			expectedErr:       errDummy,
		},
		{
			name:              "no wait and exit",
			allowMoreAttempts: &mockReturn{false, nil},
			needWaitAttempts:  &mockReturn{false, nil},
			expectedStep:      stepExit,
			expectedErr:       nil,
		},
		{
			name:              "wait returns an error",
			allowMoreAttempts: &mockReturn{false, nil},
			needWaitAttempts:  &mockReturn{false, errDummy},
			expectedStep:      stepExit,
			expectedErr:       errDummy,
		},

		{
			name:              "wait and exit on result chan",
			allowMoreAttempts: &mockReturn{false, nil},
			needWaitAttempts:  &mockReturn{true, nil},
			closeResultChan:   true,
			expectedStep:      stepSkip,
			expectedErr:       nil,
		},
		{
			name:              "wait and exit on router quit",
			allowMoreAttempts: &mockReturn{false, nil},
			needWaitAttempts:  &mockReturn{true, nil},
			routerQuit:        true,
			expectedStep:      stepExit,
			expectedErr:       ErrRouterShuttingDown,
		},
	}

	for _, tc := range testCases {
		tc := tc

		// Create a test paymentLifecycle.
		p := createTestPaymentLifecycle()

		// Make a mock payment.
		payment := &mockMPPayment{}

		// Mock the method AllowMoreAttempts.
		payment.On("AllowMoreAttempts").Return(
			tc.allowMoreAttempts.allowOrWait,
			tc.allowMoreAttempts.err,
		).Once()

		// Mock the method NeedWaitAttempts.
		if tc.needWaitAttempts != nil {
			payment.On("NeedWaitAttempts").Return(
				tc.needWaitAttempts.allowOrWait,
				tc.needWaitAttempts.err,
			).Once()
		}

		// Send a nil error to the attemptResultChan if requested.
		if tc.closeResultChan {
			p.resultCollected = make(chan error, 1)
			p.resultCollected <- nil
		}

		// Quit the router if requested.
		if tc.routerQuit {
			close(p.router.quit)
		}

		// Once the setup is finished, run the test cases.
		t.Run(tc.name, func(t *testing.T) {
			step, err := p.decideNextStep(payment)
			require.Equal(t, tc.expectedStep, step)
			require.ErrorIs(t, tc.expectedErr, err)
		})

		// Check the payment's methods are called as expected.
		payment.AssertExpectations(t)
	}
}

// TestResumePaymentFailOnFetchPayment checks when we fail to fetch the
// payment, the error is returned.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnFetchPayment(t *testing.T) {
	// Create a test paymentLifecycle.
	p, m := newTestPaymentLifecycle(t)

	// Mock an error returned.
	m.control.On("FetchPayment", p.identifier).Return(nil, errDummy)

	// Send the payment and assert it failed.
	sendPaymentAndAssertError(t, context.Background(), p, errDummy)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailOnTimeout checks that when timeout is reached, the
// payment is failed.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnTimeout(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	paymentAmt := lnwire.MilliSatoshi(10000)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is not
	// critical, so we loosen the checks on how many times it's been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. make the timeout happens instantly and sleep one millisecond to
	// make sure it timed out.
	deadline := time.Now().Add(time.Nanosecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	time.Sleep(1 * time.Millisecond)

	// 4. the payment should be failed with reason timeout.
	m.control.On("FailPayment",
		p.identifier, channeldb.FailureReasonTimeout,
	).Return(nil).Once()

	// 5. decideNextStep now returns stepExit.
	m.payment.On("AllowMoreAttempts").Return(false, nil).Once().
		On("NeedWaitAttempts").Return(false, nil).Once()

	// 6. control tower deletes failed attempts.
	m.control.On("DeleteFailedAttempts", p.identifier).Return(nil).Once()

	// 7. the payment returns the failed reason.
	reason := channeldb.FailureReasonTimeout
	m.payment.On("TerminalInfo").Return(nil, &reason)

	// Send the payment and assert it failed with the timeout reason.
	sendPaymentAndAssertError(t, ctx, p, reason)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailOnTimeoutErr checks that the lifecycle fails when an
// error is returned from `checkTimeout`.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnTimeoutErr(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	paymentAmt := lnwire.MilliSatoshi(10000)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. quit the router to return an error.
	close(p.router.quit)

	// Send the payment and assert it failed when router is shutting down.
	sendPaymentAndAssertError(
		t, context.Background(), p, ErrRouterShuttingDown,
	)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailContextCancel checks that the lifecycle fails when the
// context is canceled.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailContextCancel(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	// Create the cancelable payment context.
	ctx, cancel := context.WithCancel(context.Background())

	paymentAmt := lnwire.MilliSatoshi(10000)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is not
	// critical, so we loosen the checks on how many times it's been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. Cancel the context and skip the FailPayment error to trigger the
	// context cancellation of the payment.
	cancel()

	m.control.On(
		"FailPayment", p.identifier, channeldb.FailureReasonCanceled,
	).Return(nil).Once()

	// 4. decideNextStep now returns stepExit.
	m.payment.On("AllowMoreAttempts").Return(false, nil).Once().
		On("NeedWaitAttempts").Return(false, nil).Once()

	// 5. Control tower deletes failed attempts.
	m.control.On("DeleteFailedAttempts", p.identifier).Return(nil).Once()

	// 6. We will observe FailureReasonError if the context was cancelled.
	reason := channeldb.FailureReasonError
	m.payment.On("TerminalInfo").Return(nil, &reason)

	// Send the payment and assert it failed with the timeout reason.
	sendPaymentAndAssertError(t, ctx, p, reason)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailOnStepErr checks that the lifecycle fails when an
// error is returned from `decideNextStep`.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnStepErr(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	paymentAmt := lnwire.MilliSatoshi(10000)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. decideNextStep now returns an error.
	m.payment.On("AllowMoreAttempts").Return(false, errDummy).Once()

	// Send the payment and assert it failed.
	sendPaymentAndAssertError(t, context.Background(), p, errDummy)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailOnRequestRouteErr checks that the lifecycle fails when
// an error is returned from `requestRoute`.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnRequestRouteErr(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	paymentAmt := lnwire.MilliSatoshi(10000)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. decideNextStep now returns stepProceed.
	m.payment.On("AllowMoreAttempts").Return(true, nil).Once()

	// 4. mock requestRoute to return an error.
	m.paySession.On("RequestRoute",
		paymentAmt, p.feeLimit, uint32(ps.NumAttemptsInFlight),
		uint32(p.currentHeight),
	).Return(nil, errDummy).Once()

	// Send the payment and assert it failed.
	sendPaymentAndAssertError(t, context.Background(), p, errDummy)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailOnRegisterAttemptErr checks that the lifecycle fails
// when an error is returned from `registerAttempt`.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnRegisterAttemptErr(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	// Create a dummy route that will be returned by `RequestRoute`.
	paymentAmt := lnwire.MilliSatoshi(10000)
	rt := createDummyRoute(t, paymentAmt)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. decideNextStep now returns stepProceed.
	m.payment.On("AllowMoreAttempts").Return(true, nil).Once()

	// 4. mock requestRoute to return an route.
	m.paySession.On("RequestRoute",
		paymentAmt, p.feeLimit, uint32(ps.NumAttemptsInFlight),
		uint32(p.currentHeight),
	).Return(rt, nil).Once()

	// 5. mock shardTracker used in `createNewPaymentAttempt` to return an
	// error.
	//
	// Mock NextPaymentID to always return the attemptID.
	attemptID := uint64(1)
	p.router.cfg.NextPaymentID = func() (uint64, error) {
		return attemptID, nil
	}

	// Return an error to end the lifecycle.
	m.shardTracker.On("NewShard",
		attemptID, true,
	).Return(nil, errDummy).Once()

	// Send the payment and assert it failed.
	sendPaymentAndAssertError(t, context.Background(), p, errDummy)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentFailOnSendAttemptErr checks that the lifecycle fails when
// an error is returned from `sendAttempt`.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentFailOnSendAttemptErr(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	// Create a dummy route that will be returned by `RequestRoute`.
	paymentAmt := lnwire.MilliSatoshi(10000)
	rt := createDummyRoute(t, paymentAmt)

	// We now enter the payment lifecycle loop.
	//
	// 1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 3. decideNextStep now returns stepProceed.
	m.payment.On("AllowMoreAttempts").Return(true, nil).Once()

	// 4. mock requestRoute to return an route.
	m.paySession.On("RequestRoute",
		paymentAmt, p.feeLimit, uint32(ps.NumAttemptsInFlight),
		uint32(p.currentHeight),
	).Return(rt, nil).Once()

	// 5. mock `registerAttempt` to return an attempt.
	//
	// Mock NextPaymentID to always return the attemptID.
	attemptID := uint64(1)
	p.router.cfg.NextPaymentID = func() (uint64, error) {
		return attemptID, nil
	}

	// Mock shardTracker to return the mock shard.
	m.shardTracker.On("NewShard",
		attemptID, true,
	).Return(m.shard, nil).Once()

	// Mock the methods on the shard.
	m.shard.On("MPP").Return(&record.MPP{}).Twice().
		On("AMP").Return(nil).Once().
		On("Hash").Return(p.identifier).Once()

	// Mock the time and expect it to be call twice.
	m.clock.On("Now").Return(time.Now()).Twice()

	// We now register attempt and return no error.
	m.control.On("RegisterAttempt",
		p.identifier, mock.Anything,
	).Return(nil).Once()

	// 6. mock `sendAttempt` to return an error.
	m.payer.On("SendHTLC",
		mock.Anything, attemptID, mock.Anything,
	).Return(errDummy).Once()

	// The above error will end up being handled by `handleSwitchErr`, in
	// which we'd fail the payment, cancel the shard and fail the attempt.
	//
	// `FailPayment` should be called with an internal reason.
	reason := channeldb.FailureReasonError
	m.control.On("FailPayment", p.identifier, reason).Return(nil).Once()

	// `CancelShard` should be called with the attemptID.
	m.shardTracker.On("CancelShard", attemptID).Return(nil).Once()

	// Mock `FailAttempt` to return a dummy error to exit the loop.
	m.control.On("FailAttempt",
		p.identifier, attemptID, mock.Anything,
	).Return(nil, errDummy).Once()

	// Send the payment and assert it failed.
	sendPaymentAndAssertError(t, context.Background(), p, errDummy)

	// Expected collectResultAsync to not be called.
	require.Zero(t, m.collectResultsCount)
}

// TestResumePaymentSuccess checks that a normal payment flow that is
// succeeded.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentSuccess(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	// Create a dummy route that will be returned by `RequestRoute`.
	paymentAmt := lnwire.MilliSatoshi(10000)
	rt := createDummyRoute(t, paymentAmt)

	// We now enter the payment lifecycle loop.
	//
	// 1.1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 1.2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 1.3. decideNextStep now returns stepProceed.
	m.payment.On("AllowMoreAttempts").Return(true, nil).Once()

	// 1.4. mock requestRoute to return an route.
	m.paySession.On("RequestRoute",
		paymentAmt, p.feeLimit, uint32(ps.NumAttemptsInFlight),
		uint32(p.currentHeight),
	).Return(rt, nil).Once()

	// 1.5. mock `registerAttempt` to return an attempt.
	//
	// Mock NextPaymentID to always return the attemptID.
	attemptID := uint64(1)
	p.router.cfg.NextPaymentID = func() (uint64, error) {
		return attemptID, nil
	}

	// Mock shardTracker to return the mock shard.
	m.shardTracker.On("NewShard",
		attemptID, true,
	).Return(m.shard, nil).Once()

	// Mock the methods on the shard.
	m.shard.On("MPP").Return(&record.MPP{}).Twice().
		On("AMP").Return(nil).Once().
		On("Hash").Return(p.identifier).Once()

	// Mock the time and expect it to be called.
	m.clock.On("Now").Return(time.Now())

	// We now register attempt and return no error.
	m.control.On("RegisterAttempt",
		p.identifier, mock.Anything,
	).Return(nil).Once()

	// 1.6. mock `sendAttempt` to succeed, which brings us into the next
	// iteration of the lifecycle.
	m.payer.On("SendHTLC",
		mock.Anything, attemptID, mock.Anything,
	).Return(nil).Once()

	// We now enter the second iteration of the lifecycle loop.
	//
	// 2.1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2.2. calls `GetState` and return the state.
	m.payment.On("GetState").Return(ps).Run(func(args mock.Arguments) {
		ps.RemainingAmt = 0
	}).Once()

	// 2.3. decideNextStep now returns stepExit and exits the loop.
	m.payment.On("AllowMoreAttempts").Return(false, nil).Once().
		On("NeedWaitAttempts").Return(false, nil).Once()

	// We should perform an optional deletion over failed attempts.
	m.control.On("DeleteFailedAttempts", p.identifier).Return(nil).Once()

	// Finally, mock the `TerminalInfo` to return the settled attempt.
	// Create a SettleAttempt.
	testPreimage := lntypes.Preimage{1, 2, 3}
	settledAttempt := makeSettledAttempt(t, int(paymentAmt), testPreimage)
	m.payment.On("TerminalInfo").Return(settledAttempt, nil).Once()

	// Send the payment and assert the preimage is matched.
	sendPaymentAndAssertSucceeded(t, p, testPreimage)

	// Expected collectResultAsync to called.
	require.Equal(t, 1, m.collectResultsCount)
}

// TestResumePaymentSuccessWithTwoAttempts checks a successful payment flow
// with two HTLC attempts.
//
// NOTE: No parallel test because it overwrites global variables.
//
//nolint:paralleltest
func TestResumePaymentSuccessWithTwoAttempts(t *testing.T) {
	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := setupTestPaymentLifecycle(t)

	// Create a dummy route that will be returned by `RequestRoute`.
	paymentAmt := lnwire.MilliSatoshi(10000)
	rt := createDummyRoute(t, paymentAmt/2)

	// We now enter the payment lifecycle loop.
	//
	// 1.1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 1.2. calls `GetState` and return the state.
	ps := &channeldb.MPPaymentState{
		RemainingAmt: paymentAmt,
	}
	m.payment.On("GetState").Return(ps).Once()

	// NOTE: GetStatus is only used to populate the logs which is
	// not critical so we loosen the checks on how many times it's
	// been called.
	m.payment.On("GetStatus").Return(channeldb.StatusInFlight)

	// 1.3. decideNextStep now returns stepProceed.
	m.payment.On("AllowMoreAttempts").Return(true, nil).Once()

	// 1.4. mock requestRoute to return an route.
	m.paySession.On("RequestRoute",
		paymentAmt, p.feeLimit, uint32(ps.NumAttemptsInFlight),
		uint32(p.currentHeight),
	).Return(rt, nil).Once()

	// Create two attempt IDs here.
	attemptID1 := uint64(1)
	attemptID2 := uint64(2)

	// 1.5. mock `registerAttempt` to return an attempt.
	//
	// Mock NextPaymentID to return the first attemptID on the first call
	// and the second attemptID on the second call.
	var numAttempts atomic.Uint64
	p.router.cfg.NextPaymentID = func() (uint64, error) {
		numAttempts.Add(1)
		if numAttempts.Load() == 1 {
			return attemptID1, nil
		}

		return attemptID2, nil
	}

	// Mock shardTracker to return the mock shard.
	m.shardTracker.On("NewShard",
		attemptID1, false,
	).Return(m.shard, nil).Once()

	// Mock the methods on the shard.
	m.shard.On("MPP").Return(&record.MPP{}).Twice().
		On("AMP").Return(nil).Once().
		On("Hash").Return(p.identifier).Once()

	// Mock the time and expect it to be called.
	m.clock.On("Now").Return(time.Now())

	// We now register attempt and return no error.
	m.control.On("RegisterAttempt",
		p.identifier, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		ps.RemainingAmt = paymentAmt / 2
	}).Once()

	// 1.6. mock `sendAttempt` to succeed, which brings us into the next
	// iteration of the lifecycle where we mock a temp failure.
	m.payer.On("SendHTLC",
		mock.Anything, attemptID1, mock.Anything,
	).Return(nil).Once()

	// We now enter the second iteration of the lifecycle loop.
	//
	// 2.1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 2.2. calls `GetState` and return the state.
	m.payment.On("GetState").Return(ps).Once()

	// 2.3. decideNextStep now returns stepProceed so we can send the
	// second attempt.
	m.payment.On("AllowMoreAttempts").Return(true, nil).Once()

	// 2.4. mock requestRoute to return an route.
	m.paySession.On("RequestRoute",
		paymentAmt/2, p.feeLimit, uint32(ps.NumAttemptsInFlight),
		uint32(p.currentHeight),
	).Return(rt, nil).Once()

	// 2.5. mock `registerAttempt` to return an attempt.
	//
	// Mock shardTracker to return the mock shard.
	m.shardTracker.On("NewShard",
		attemptID2, true,
	).Return(m.shard, nil).Once()

	// Mock the methods on the shard.
	m.shard.On("MPP").Return(&record.MPP{}).Twice().
		On("AMP").Return(nil).Once().
		On("Hash").Return(p.identifier).Once()

	// We now register attempt and return no error.
	m.control.On("RegisterAttempt",
		p.identifier, mock.Anything,
	).Return(nil).Once()

	// 2.6. mock `sendAttempt` to succeed, which brings us into the next
	// iteration of the lifecycle.
	m.payer.On("SendHTLC",
		mock.Anything, attemptID2, mock.Anything,
	).Return(nil).Once()

	// We now enter the third iteration of the lifecycle loop.
	//
	// 3.1. calls `FetchPayment` and return the payment.
	m.control.On("FetchPayment", p.identifier).Return(m.payment, nil).Once()

	// 3.2. calls `GetState` and return the state.
	m.payment.On("GetState").Return(ps).Once()

	// 3.3. decideNextStep now returns stepExit to exit the loop.
	m.payment.On("AllowMoreAttempts").Return(false, nil).Once().
		On("NeedWaitAttempts").Return(false, nil).Once()

	// We should perform an optional deletion over failed attempts.
	m.control.On("DeleteFailedAttempts", p.identifier).Return(nil).Once()

	// Finally, mock the `TerminalInfo` to return the settled attempt.
	// Create a SettleAttempt.
	testPreimage := lntypes.Preimage{1, 2, 3}
	settledAttempt := makeSettledAttempt(t, int(paymentAmt), testPreimage)
	m.payment.On("TerminalInfo").Return(settledAttempt, nil).Once()

	// Send the payment and assert the preimage is matched.
	sendPaymentAndAssertSucceeded(t, p, testPreimage)

	// Expected collectResultAsync to called.
	require.Equal(t, 2, m.collectResultsCount)
}

// TestCollectResultExitOnErr checks that when there's an error returned from
// htlcswitch via `GetAttemptResult`, it's handled and returned.
func TestCollectResultExitOnErr(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	attempt := makeFailedAttempt(t, paymentAmt)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a dummy error.
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(nil, errDummy).Once()

	// The above error will end up being handled by `handleSwitchErr`, in
	// which we'd fail the payment, cancel the shard and fail the attempt.
	//
	// `FailPayment` should be called with an internal reason.
	reason := channeldb.FailureReasonError
	m.control.On("FailPayment", p.identifier, reason).Return(nil).Once()

	// `CancelShard` should be called with the attemptID.
	m.shardTracker.On("CancelShard", attempt.AttemptID).Return(nil).Once()

	// Mock `FailAttempt` to return a dummy error.
	m.control.On("FailAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(nil, errDummy).Once()

	// Mock the clock to return a current time.
	m.clock.On("Now").Return(time.Now())

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.ErrorIs(t, err, errDummy, "expected dummy error")
	require.Nil(t, result, "expected nil attempt")
}

// TestCollectResultExitOnResultErr checks that when there's an error returned
// from htlcswitch via the result channel, it's handled and returned.
func TestCollectResultExitOnResultErr(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	attempt := makeFailedAttempt(t, paymentAmt)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Send an error to the result chan.
		resultChan <- &htlcswitch.PaymentResult{
			Error: errDummy,
		}
	})

	// The above error will end up being handled by `handleSwitchErr`, in
	// which we'd fail the payment, cancel the shard and fail the attempt.
	//
	// `FailPayment` should be called with an internal reason.
	reason := channeldb.FailureReasonError
	m.control.On("FailPayment", p.identifier, reason).Return(nil).Once()

	// `CancelShard` should be called with the attemptID.
	m.shardTracker.On("CancelShard", attempt.AttemptID).Return(nil).Once()

	// Mock `FailAttempt` to return a dummy error.
	m.control.On("FailAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(nil, errDummy).Once()

	// Mock the clock to return a current time.
	m.clock.On("Now").Return(time.Now())

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.ErrorIs(t, err, errDummy, "expected dummy error")
	require.Nil(t, result, "expected nil attempt")
}

// TestCollectResultExitOnSwitcQuit checks that when the htlcswitch is shutting
// down an error is returned.
func TestCollectResultExitOnSwitchQuit(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	attempt := makeFailedAttempt(t, paymentAmt)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Close the result chan to simulate a htlcswitch quit.
		close(resultChan)
	})

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.ErrorIs(t, err, htlcswitch.ErrSwitchExiting,
		"expected switch exit")
	require.Nil(t, result, "expected nil attempt")
}

// TestCollectResultExitOnRouterQuit checks that when the channel router is
// shutting down an error is returned.
func TestCollectResultExitOnRouterQuit(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	attempt := makeFailedAttempt(t, paymentAmt)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Close the channel router.
		close(p.router.quit)
	})

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.ErrorIs(t, err, ErrRouterShuttingDown, "expected router exit")
	require.Nil(t, result, "expected nil attempt")
}

// TestCollectResultExitOnLifecycleQuit checks that when the payment lifecycle
// is shutting down an error is returned.
func TestCollectResultExitOnLifecycleQuit(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	attempt := makeFailedAttempt(t, paymentAmt)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Stop the lifecycle.
		p.stop()
	})

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.ErrorIs(t, err, ErrPaymentLifecycleExiting,
		"expected lifecycle exit")
	require.Nil(t, result, "expected nil attempt")
}

// TestCollectResultExitOnSettleErr checks that when settling the attempt
// fails an error is returned.
func TestCollectResultExitOnSettleErr(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	preimage := lntypes.Preimage{1}
	attempt := makeSettledAttempt(t, paymentAmt, preimage)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Send the preimage to the result chan.
		resultChan <- &htlcswitch.PaymentResult{
			Preimage: preimage,
		}
	})

	// Once the result is received, `ReportPaymentSuccess` should be
	// called.
	m.missionControl.On("ReportPaymentSuccess",
		attempt.AttemptID, &attempt.Route,
	).Return(nil).Once()

	// Now mock an error being returned from `SettleAttempt`.
	m.control.On("SettleAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(nil, errDummy).Once()

	// Mock the clock to return a current time.
	m.clock.On("Now").Return(time.Now())

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.ErrorIs(t, err, errDummy, "expected settle error")
	require.Nil(t, result, "expected nil attempt")
}

// TestCollectResultSuccess checks a successful htlc settlement.
func TestCollectResultSuccess(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	preimage := lntypes.Preimage{1}
	attempt := makeSettledAttempt(t, paymentAmt, preimage)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Send the preimage to the result chan.
		resultChan <- &htlcswitch.PaymentResult{
			Preimage: preimage,
		}
	})

	// Once the result is received, `ReportPaymentSuccess` should be
	// called.
	m.missionControl.On("ReportPaymentSuccess",
		attempt.AttemptID, &attempt.Route,
	).Return(nil).Once()

	// Now the settled htlc being returned from `SettleAttempt`.
	m.control.On("SettleAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(attempt, nil).Once()

	// Mock the clock to return a current time.
	m.clock.On("Now").Return(time.Now())

	// Now call the method under test.
	result, err := p.collectResult(attempt)
	require.NoError(t, err, "expected no error")
	require.Equal(t, preimage, result.attempt.Settle.Preimage,
		"preimage mismatch")
}

// TestCollectResultAsyncSuccess checks a successful htlc settlement.
func TestCollectResultAsyncSuccess(t *testing.T) {
	t.Parallel()

	// Create a test paymentLifecycle with the initial two calls mocked.
	p, m := newTestPaymentLifecycle(t)

	paymentAmt := 10_000
	preimage := lntypes.Preimage{1}
	attempt := makeSettledAttempt(t, paymentAmt, preimage)

	// Mock shardTracker to return the payment hash.
	m.shardTracker.On("GetHash",
		attempt.AttemptID,
	).Return(p.identifier, nil).Once()

	// Mock the htlcswitch to return a the result chan.
	resultChan := make(chan *htlcswitch.PaymentResult, 1)
	m.payer.On("GetAttemptResult",
		attempt.AttemptID, p.identifier, mock.Anything,
	).Return(resultChan, nil).Once().Run(func(args mock.Arguments) {
		// Send the preimage to the result chan.
		resultChan <- &htlcswitch.PaymentResult{
			Preimage: preimage,
		}
	})

	// Once the result is received, `ReportPaymentSuccess` should be
	// called.
	m.missionControl.On("ReportPaymentSuccess",
		attempt.AttemptID, &attempt.Route,
	).Return(nil).Once()

	// Now the settled htlc being returned from `SettleAttempt`.
	m.control.On("SettleAttempt",
		p.identifier, attempt.AttemptID, mock.Anything,
	).Return(attempt, nil).Once()

	// Mock the clock to return a current time.
	m.clock.On("Now").Return(time.Now())

	// Now call the method under test.
	p.collectResultAsync(attempt)

	// Assert the result is returned within 5 seconds.
	var err error
	waitErr := wait.NoError(func() error {
		err = <-p.resultCollected
		return nil
	}, testTimeout)
	require.NoError(t, waitErr, "timeout waiting for result")

	// Assert that a nil error is received.
	require.NoError(t, err, "expected no error")
}
