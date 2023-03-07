package routing

import (
	"testing"
	"time"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	dummyErr = errors.New("dummy")
)

func makeSettledAttempt(total, fee int,
	preimage lntypes.Preimage) channeldb.HTLCAttempt {

	return channeldb.HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(total, total-fee),
		Settle:          &channeldb.HTLCSettleInfo{Preimage: preimage},
	}
}

func makeFailedAttempt(total, fee int) *channeldb.HTLCAttempt {
	return &channeldb.HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(total, total-fee),
		Failure: &channeldb.HTLCFailInfo{
			Reason: channeldb.HTLCFailInternal,
		},
	}
}

func makeAttemptInfo(total, amtForwarded int) channeldb.HTLCAttemptInfo {
	hop := &route.Hop{AmtToForward: lnwire.MilliSatoshi(amtForwarded)}
	return channeldb.HTLCAttemptInfo{
		Route: route.Route{
			TotalAmount: lnwire.MilliSatoshi(total),
			Hops:        []*route.Hop{hop},
		},
	}
}

// TestCheckTimeoutTimedOut checks that when the payment times out, it is
// marked as failed.
func TestCheckTimeoutTimedOut(t *testing.T) {
	t.Parallel()

	p := createTestPaymentLifecycle()

	// Mock the control tower's `FailPayment` method.
	ct := &mockControlTower{}
	ct.On("FailPayment",
		p.identifier, channeldb.FailureReasonTimeout).Return(nil)

	// Mount the mocked control tower.
	p.router.cfg.Control = ct

	// Make the timeout happens instantly.
	p.timeoutChan = time.After(1 * time.Nanosecond)

	// Sleep one millisecond to make sure it timed out.
	time.Sleep(1 * time.Millisecond)

	// Call the function and expect no error.
	err := p.checkTimeout()
	require.NoError(t, err)

	// Assert that `FailPayment` is called as expected.
	ct.AssertExpectations(t)

	// We now test that when `FailPayment` returns an error, it's returned
	// by the function too.
	//
	// Mock `FailPayment` to return a dummy error.
	dummyErr := errors.New("dummy")
	ct = &mockControlTower{}
	ct.On("FailPayment",
		p.identifier, channeldb.FailureReasonTimeout).Return(dummyErr)

	// Mount the mocked control tower.
	p.router.cfg.Control = ct

	// Make the timeout happens instantly.
	p.timeoutChan = time.After(1 * time.Nanosecond)

	// Sleep one millisecond to make sure it timed out.
	time.Sleep(1 * time.Millisecond)

	// Call the function and expect an error.
	err = p.checkTimeout()
	require.ErrorIs(t, err, dummyErr)

	// Assert that `FailPayment` is called as expected.
	ct.AssertExpectations(t)
}

// TestCheckTimeoutOnRouterQuit checks that when the router has quit, an error
// is returned from checkTimeout.
func TestCheckTimeoutOnRouterQuit(t *testing.T) {
	t.Parallel()

	p := createTestPaymentLifecycle()

	close(p.router.quit)
	err := p.checkTimeout()
	require.ErrorIs(t, err, ErrRouterShuttingDown)
}

// createTestPaymentLifecycle creates a `paymentLifecycle` using the mocks.
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
	dummyErr := errors.New("dummy")
	paySession.On("RequestRoute",
		mock.Anything, mock.Anything, mock.Anything, mock.Anything,
	).Return(nil, dummyErr)

	result, err := p.requestRoute(ps)

	// Expect an error is returned since it's critical.
	require.ErrorIs(t, err, dummyErr, "error not matched")
	require.Nil(t, result, "expected no route returned")

	// Assert that `RequestRoute` is called as expected.
	paySession.AssertExpectations(t)
}

// TestRequestRouteHandleNoRouteErr checks that `requestRoute` can successfully
// handle the `noRouteError` returned from payment session.
func TestRequestRouteHandleNoRouteErr(t *testing.T) {
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
	).Return(nil, errNoTlvPayload)

	result, err := p.requestRoute(ps)

	// Expect no error is returned since it's not critical.
	require.NoError(t, err, "expected no error")
	require.Nil(t, result, "expected no route returned")

	// Assert that `RequestRoute` is called as expected.
	paySession.AssertExpectations(t)
}

// TestRequestRouteFailPaymentSucceed checks that `requestRoute` fails the
// payment when received an `noRouteError` returned from payment session while
// it has no inflight attempts.
func TestRequestRouteFailPaymentSucceed(t *testing.T) {
	t.Parallel()

	p := createTestPaymentLifecycle()

	// Create a mock payment session.
	paySession := &mockPaymentSession{}

	// Mock the control tower's `FailPayment` method.
	ct := &mockControlTower{}
	ct.On("FailPayment",
		p.identifier, errNoTlvPayload.FailureReason(),
	).Return(nil)

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

	// Expect no error is returned since it's not critical.
	require.NoError(t, err, "expected no error")
	require.Nil(t, result, "expected no route returned")

	// Assert that `RequestRoute` is called as expected.
	paySession.AssertExpectations(t)

	// Assert that `FailPayment` is called as expected.
	ct.AssertExpectations(t)
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
	dummyErr := errors.New("dummy")
	ct.On("FailPayment",
		p.identifier, errNoTlvPayload.FailureReason(),
	).Return(dummyErr)

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
	require.ErrorIs(t, err, dummyErr, "error not matched")
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
			allowMoreAttempts: &mockReturn{false, dummyErr},
			expectedStep:      stepExit,
			expectedErr:       dummyErr,
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
			needWaitAttempts:  &mockReturn{false, dummyErr},
			expectedStep:      stepExit,
			expectedErr:       dummyErr,
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
