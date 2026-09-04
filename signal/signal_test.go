package signal

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// waitForShutdown waits for the interceptor's shutdown channel to close.
func waitForShutdown(t *testing.T, interceptor Interceptor) {
	t.Helper()

	select {
	case <-interceptor.ShutdownChannel():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown channel")
	}

	// Wait for mainInterruptHandler goroutine to fully exit and reset
	// the global started flag so the next test can call Intercept().
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&started) == 0
	}, time.Second, 10*time.Millisecond, "interceptor did not reset")
}

// TestRequestShutdownSetsFlag tests that calling RequestShutdown sets the
// shutdownRequested flag, which is used to distinguish internal shutdown
// requests (e.g. chain backend failure) from external OS signals.
func TestRequestShutdownSetsFlag(t *testing.T) {
	interceptor, err := Intercept()
	require.NoError(t, err)

	// Before any shutdown, the flag should not be set.
	require.False(t, interceptor.ShutdownWasRequested())

	// Request an internal shutdown.
	interceptor.RequestShutdown()

	waitForShutdown(t, interceptor)

	// The flag should now be set after the handler processed the request.
	require.True(t, interceptor.ShutdownWasRequested())
}

// TestOSSignalDoesNotSetRequestedFlag tests that an OS signal based shutdown
// does not set the shutdownRequested flag, ensuring a clean exit code 0.
func TestOSSignalDoesNotSetRequestedFlag(t *testing.T) {
	interceptor, err := Intercept()
	require.NoError(t, err)

	// Simulate an OS signal.
	interceptor.interruptChannel <- nil

	waitForShutdown(t, interceptor)

	// The flag should NOT be set for OS signal shutdowns.
	require.False(t, interceptor.ShutdownWasRequested())
}

// TestOSSignalThenRequestShutdownNoFlag tests the race condition where a user
// sends SIGINT first, and then an internal component calls RequestShutdown.
// The first shutdown cause (OS signal) should win, so the flag must remain
// unset and the process should exit with code 0.
func TestOSSignalThenRequestShutdownNoFlag(t *testing.T) {
	interceptor, err := Intercept()
	require.NoError(t, err)

	// Simulate an OS signal to initiate shutdown first.
	interceptor.interruptChannel <- nil

	// Wait for the shutdown process to start before sending the second
	// request. This is more robust than a fixed sleep.
	require.Eventually(t, func() bool {
		return !interceptor.Alive()
	}, time.Second, 10*time.Millisecond, "shutdown did not start")

	// Now an internal component also requests shutdown (late arrival).
	interceptor.RequestShutdown()

	waitForShutdown(t, interceptor)

	// The flag must NOT be set -- the OS signal arrived first.
	require.False(t, interceptor.ShutdownWasRequested())
}
