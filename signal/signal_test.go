package signal

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestInterceptor builds an Interceptor and starts its handler goroutine
// without going through Intercept(), which holds a package-level lock that
// prevents more than one active interceptor at a time. This lets each subtest
// own an independent interceptor.
func newTestInterceptor() *Interceptor {
	c := &Interceptor{
		interruptChannel:       make(chan os.Signal, 1),
		shutdownChannel:        make(chan struct{}),
		shutdownRequestChannel: make(chan struct{}),
		quit:                   make(chan struct{}),
	}
	go c.mainInterruptHandler()
	return c
}

// TestRequestedShutdown checks that RequestedShutdown returns false when lnd
// exits due to an OS signal and true when shutdown is triggered
// programmatically via RequestShutdown (e.g. a health check failure).
func TestRequestedShutdown(t *testing.T) {
	t.Parallel()

	t.Run("os signal does not set requested shutdown", func(t *testing.T) {
		t.Parallel()

		interceptor := newTestInterceptor()

		// Simulate an OS signal (SIGINT) the way the OS would deliver
		// it, bypassing RequestShutdown.
		interceptor.interruptChannel <- syscall.SIGINT

		// Wait for the shutdown goroutine to finish.
		<-interceptor.ShutdownChannel()

		// A signal-driven shutdown must not set the flag; exiting with
		// code 0 is the correct behaviour in that case.
		require.False(t, interceptor.RequestedShutdown())
	})

	t.Run("programmatic shutdown sets requested shutdown", func(t *testing.T) {
		t.Parallel()

		interceptor := newTestInterceptor()

		// Trigger a programmatic shutdown, as the health check does
		// when bitcoind becomes unreachable.
		interceptor.RequestShutdown()

		// Wait for the shutdown goroutine to finish.
		<-interceptor.ShutdownChannel()

		// A programmatic shutdown must set the flag so that main exits
		// with code 1, allowing systemd (Restart=on-failure) to
		// restart lnd automatically.
		require.True(t, interceptor.RequestedShutdown())
	})
}
