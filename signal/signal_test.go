package signal

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestInterceptor starts an interceptor and registers a cleanup that waits
// for its main handler to exit, so that the next test can start a fresh one.
func newTestInterceptor(t *testing.T) Interceptor {
	t.Helper()

	interceptor, err := Intercept()
	require.NoError(t, err)

	t.Cleanup(func() {
		interceptor.RequestShutdown()
		waitForStopped(t, interceptor)
	})

	return interceptor
}

// waitForStopped waits for the interceptor's main handler to exit.
func waitForStopped(t *testing.T, interceptor Interceptor) {
	t.Helper()

	select {
	case <-interceptor.ShutdownChannel():
	case <-time.After(time.Second):
		t.Fatal("interceptor did not shut down")
	}
}

// TestExitCodeDefault asserts that a shutdown requested without a code, as done
// by lncli stop, exits successfully.
func TestExitCodeDefault(t *testing.T) {
	interceptor := newTestInterceptor(t)

	interceptor.RequestShutdown()
	waitForStopped(t, interceptor)

	require.Equal(t, ExitCodeSuccess, interceptor.ExitCode())
}

// TestExitCodeSignal asserts that a shutdown triggered by an OS signal exits
// successfully.
func TestExitCodeSignal(t *testing.T) {
	interceptor := newTestInterceptor(t)

	interceptor.interruptChannel <- os.Interrupt
	waitForStopped(t, interceptor)

	require.Equal(t, ExitCodeSuccess, interceptor.ExitCode())
}

// TestExitCodeFirstWins asserts that the first non-zero exit code recorded is
// the one reported, so a specific reason is not overwritten by the generic
// critical error raised while logging it, and that the code is visible through
// every copy of the interceptor.
func TestExitCodeFirstWins(t *testing.T) {
	interceptor := newTestInterceptor(t)

	// The interceptor is passed around by value, so record the code on a
	// copy and read it back through the original.
	cp := interceptor
	cp.SetExitCode(ExitCodeDiskSpace)
	cp.SetExitCode(ExitCodeCriticalError)

	interceptor.RequestShutdown()
	waitForStopped(t, interceptor)

	require.Equal(t, ExitCodeDiskSpace, interceptor.ExitCode())
}

// TestExitCodeAfterShutdown asserts that a failure recorded once a shutdown
// is already underway, for example a health check giving up during the
// teardown of lncli stop, does not change the exit code.
func TestExitCodeAfterShutdown(t *testing.T) {
	interceptor := newTestInterceptor(t)

	interceptor.RequestShutdown()
	waitForStopped(t, interceptor)

	interceptor.SetExitCode(ExitCodeChainBackend)

	require.Equal(t, ExitCodeSuccess, interceptor.ExitCode())
}

// TestExitCodeZeroValue asserts that a zero-value interceptor, as returned by
// Intercept on error or used by embedders that never start one, reports a
// successful exit instead of panicking.
func TestExitCodeZeroValue(t *testing.T) {
	var interceptor Interceptor

	require.NotPanics(t, func() {
		interceptor.SetExitCode(ExitCodeChainBackend)
	})
	require.Equal(t, ExitCodeSuccess, interceptor.ExitCode())
}

// TestExitCodeString checks that every documented exit code has a reason, and
// that an unknown code still prints its number.
func TestExitCodeString(t *testing.T) {
	t.Parallel()

	codes := map[ExitCode]string{
		ExitCodeSuccess:       "stop requested",
		ExitCodeCriticalError: "critical error or startup failure",
		ExitCodeChainBackend:  "chain backend health check failed",
		ExitCodeDiskSpace:     "disk space health check failed",
		ExitCodeTLSCert:       "TLS certificate health check failed",
		ExitCodeTorConnection: "tor connection health check failed",
		ExitCodeRemoteSigner:  "remote signer health check failed",
		ExitCodeLeaderStatus:  "leader status health check failed",
	}
	for code, want := range codes {
		require.Equal(t, want, code.String())
	}

	require.Equal(t, "unknown exit code 99", ExitCode(99).String())
}
