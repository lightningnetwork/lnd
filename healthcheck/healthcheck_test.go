package healthcheck

import (
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

var (
	errNonNil = errors.New("non-nil test error")
	timeout   = time.Second
	testTime  = time.Unix(1, 2)
)

type mockedCheck struct {
	t       *testing.T
	errChan chan error
}

// newMockCheck creates a new mock.
func newMockCheck(t *testing.T) *mockedCheck {
	return &mockedCheck{
		t:       t,
		errChan: make(chan error),
	}
}

// call returns our mock's error channel, which we can send responses on.
func (m *mockedCheck) call() chan error {
	return m.errChan
}

// sendError sends an error into our mock's error channel, mocking the sending
// of a response from our check function.
func (m *mockedCheck) sendError(err error) {
	select {
	case m.errChan <- err:
	case <-time.After(timeout):
		m.t.Fatalf("could not send error: %v", err)
	}
}

// TestMonitor tests creation and triggering of a monitor with a health check.
func TestMonitor(t *testing.T) {
	intervalTicker := ticker.NewForce(time.Hour)

	mock := newMockCheck(t)
	shutdown := make(chan struct{})

	// Create our config for monitoring. We will use a 0 back off so that
	// out test does not need to wait.
	cfg := &Config{
		Checks: []*Observation{
			{
				Check:     mock.call,
				Interval:  intervalTicker,
				Attempts:  2,
				Backoff:   0,
				Timeout:   time.Hour,
				OnSuccess: noOpCallback,
				OnFailure: noOpCallback,
			},
		},
		Shutdown: func(string, ...interface{}) {
			shutdown <- struct{}{}
		},
	}
	monitor := NewMonitor(cfg)

	require.NoError(t, monitor.Start(), "could not start monitor")

	// Tick is a helper we will use to tick our interval.
	tick := func() {
		select {
		case intervalTicker.Force <- testTime:
		case <-time.After(timeout):
			t.Fatal("could not tick timer")
		}
	}

	// Tick our timer and provide our error channel with a nil error. This
	// mocks our check function succeeding on the first call.
	tick()
	mock.sendError(nil)

	// Now we tick our timer again. This time send a non-nil error, followed
	// by a nil error. This tests our retry logic, because we allow 2
	// retries, so should recover without needing to shutdown.
	tick()
	mock.sendError(errNonNil)
	mock.sendError(nil)

	// Finally, we tick our timer once more, and send two non-nil errors
	// into our error channel. This mocks our check function failing twice.
	tick()
	mock.sendError(errNonNil)
	mock.sendError(errNonNil)

	// Since we have failed within our allowed number of retries, we now
	// expect a call to our shutdown function.
	select {
	case <-shutdown:
	case <-time.After(timeout):
		t.Fatal("expected shutdown")
	}

	require.NoError(t, monitor.Stop(), "could not stop monitor")
}

// TestRetryCheck tests our retry logic. It does not include a test for exiting
// during the back off period.
func TestRetryCheck(t *testing.T) {
	tests := []struct {
		name string

		// errors provides an in-order list of errors that we expect our
		// health check to respond with. The number of errors in this
		// list indicates the number of times we expect our check to
		// be called, because our test will fail if we do not consume
		// every error.
		errors []error

		// attempts is the number of times we call a check before
		// failing.
		attempts int

		// timeout is the time we allow our check to take before we
		// fail them.
		timeout time.Duration

		// expectedShutdown is true if we expect a shutdown to be
		// triggered because all of our calls failed.
		expectedShutdown bool

		// maxAttemptsReached specifies whether the max allowed
		// attempts are reached from calling retryCheck.
		maxAttemptsReached bool
	}{
		{
			name:               "first call succeeds",
			errors:             []error{nil},
			attempts:           2,
			timeout:            time.Hour,
			expectedShutdown:   false,
			maxAttemptsReached: false,
		},
		{
			name:               "first call fails",
			errors:             []error{errNonNil},
			attempts:           1,
			timeout:            time.Hour,
			expectedShutdown:   true,
			maxAttemptsReached: true,
		},
		{
			name:               "fail then recover",
			errors:             []error{errNonNil, nil},
			attempts:           2,
			timeout:            time.Hour,
			expectedShutdown:   false,
			maxAttemptsReached: false,
		},
		{
			name:               "always fail",
			errors:             []error{errNonNil, errNonNil},
			attempts:           2,
			timeout:            time.Hour,
			expectedShutdown:   true,
			maxAttemptsReached: true,
		},
		{
			name:               "no calls",
			errors:             nil,
			attempts:           0,
			timeout:            time.Hour,
			expectedShutdown:   false,
			maxAttemptsReached: false,
		},
		{
			name:               "call times out",
			errors:             nil,
			attempts:           1,
			timeout:            1,
			expectedShutdown:   true,
			maxAttemptsReached: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			var shutdown bool
			shutdownFunc := func(string, ...interface{}) {
				shutdown = true
			}

			mock := newMockCheck(t)

			// Create an observation that calls our call counting
			// function. We set a zero back off so that the test
			// will not wait.
			observation := &Observation{
				Check:     mock.call,
				Attempts:  test.attempts,
				Timeout:   test.timeout,
				Backoff:   0,
				OnSuccess: noOpCallback,
				OnFailure: noOpCallback,
			}
			quit := make(chan struct{})

			// Run our retry check in a goroutine because it blocks
			// on us sending errors into the mocked caller's error
			// channel.
			done := make(chan struct{})
			retryResult := false
			go func() {
				retryResult = observation.retryCheck(
					quit, shutdownFunc,
				)
				close(done)
			}()

			// Prompt our mock caller to send responses for calls
			// to our call function.
			for _, err := range test.errors {
				mock.sendError(err)
			}

			// Make sure that we have finished running our retry
			// check function before we start checking results.
			<-done

			require.Equal(t, test.maxAttemptsReached, retryResult,
				"retryCheck returned unexpected error")
			require.Equal(t, test.expectedShutdown, shutdown,
				"unexpected shutdown state")
		})
	}
}

// TestCallbacks verifies that we fire the OnSuccess/OnFailure callbacks
// as expected.
//
// - When the health check succeeds, the OnSuccess callback should fire.
// - When the failure threshold is reached, the OnFailure callback should fire.
func TestCallbacks(t *testing.T) {
	intervalTicker := ticker.NewForce(time.Hour)

	mock := newMockCheck(t)
	failureThreshold := 3

	successChan := make(chan struct{})
	failChan := make(chan struct{})
	shutdown := make(chan struct{})

	// Create our config for monitoring. We will use a 0 back off so that
	// out test does not need to wait.
	observation := &Observation{
		Check:    mock.call,
		Interval: intervalTicker,
		Attempts: failureThreshold,
		Backoff:  0,
		Timeout:  time.Hour,
		OnSuccess: func() {
			select {
			case successChan <- struct{}{}:
			case <-time.After(timeout):
				t.Fatal("unable to fire onSuccess callback")
			}
		},
		OnFailure: func() {
			close(failChan)
		},
	}

	cfg := &Config{
		Checks: []*Observation{observation},
		Shutdown: func(string, ...interface{}) {
			shutdown <- struct{}{}
		},
	}
	monitor := NewMonitor(cfg)
	require.NoError(t, monitor.Start(), "could not start monitor")

	// Tick is a helper we will use to tick our interval.
	tick := func() {
		select {
		case intervalTicker.Force <- testTime:
		case <-time.After(timeout):
			t.Fatal("could not tick timer")
		}
	}

	// We expect that the onSuccess callback is fired after each successful
	// check.
	for i := 0; i < failureThreshold; i++ {
		tick()
		mock.sendError(nil)

		// We expect that the onSuccess callback will have fired.
		select {
		case <-successChan:
		case <-time.After(timeout):
			t.Fatal("expected success callback")
		}

	}

	// Kick off another health check iteration. The monitor's internal
	// retry mechanism will re-attempt the check until it has reached
	// the configured maximum # of attempts.
	//
	// This mocks our check function failing the maximum # of times
	// consecutively that it is allowed.
	tick()
	for i := 1; i <= failureThreshold; i++ {
		mock.sendError(errNonNil)

		// Verify that the onFailure callback does not fire unless
		// the failure threshold (maximum # of attempts) is reached.
		if i < failureThreshold {
			select {
			case <-failChan:
				t.Fatal("unexpected onFailure callback")
			default:
			}
		}
	}

	// After reaching the failure threshold for this health check,
	// we expect that the onFailure callback will have fired.
	select {
	case <-failChan:
	case <-time.After(timeout):
		t.Fatal("expected onFailure callback")
	}

	// Since we have failed within our allowed number of retries, we now
	// expect a call to our shutdown function.
	select {
	case <-shutdown:
	case <-time.After(timeout):
		t.Fatal("expected shutdown")
	}
	require.NoError(t, monitor.Stop(), "could not stop monitor")
}

// TestDynamicChecks verifies that we actually kick off health check routines
// for observations that are added after starting the monitor.
func TestDynamicChecks(t *testing.T) {
	intervalTicker := ticker.NewForce(time.Hour)

	mock := newMockCheck(t)

	successChan := make(chan struct{})
	shutdown := make(chan struct{})

	// Don't configure any health checks for this monitor.
	// We'd like to verify that we can add checks after startup.
	cfg := &Config{
		Checks: []*Observation{},
		Shutdown: func(string, ...interface{}) {
			shutdown <- struct{}{}
		},
	}
	monitor := NewMonitor(cfg)
	require.NoError(t, monitor.Start(), "could not start monitor")

	// Tick is a helper we will use to tick our interval.
	tick := func() {
		select {
		case intervalTicker.Force <- testTime:
		case <-time.After(timeout):
			t.Fatal("could not tick timer")
		}
	}

	observation := &Observation{
		Check:    mock.call,
		Interval: intervalTicker,
		Attempts: 2,
		Backoff:  0,
		Timeout:  time.Hour,
		OnSuccess: func() {
			select {
			case successChan <- struct{}{}:
			case <-time.After(timeout):
				t.Fatal("unable to fire onSuccess callback")
			}
		},
		OnFailure: noOpCallback,
	}

	// Add the check after having started the monitor.
	err := monitor.AddCheck(observation)
	require.NoError(t, err, "could not add new observation")

	// This should initiate the check we dynamically added above.
	tick()

	// Verify that we can fire the OnSuccess callback.
	mock.sendError(errNonNil)
	mock.sendError(nil)
	select {
	case <-successChan:
	case <-time.After(timeout):
		t.Fatal("expected success callback")
	}

	// Verify that we correctly shutdown if the added health check fails.
	tick()
	mock.sendError(errNonNil)
	mock.sendError(errNonNil)

	// Since we have failed within our allowed number of retries, we now
	// expect a call to our shutdown function.
	select {
	case <-shutdown:
	case <-time.After(timeout):
		t.Fatal("expected shutdown")
	}
	require.NoError(t, monitor.Stop(), "could not stop monitor")
}
