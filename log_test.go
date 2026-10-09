package lnd

import (
	"io"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/require"
)

// TestCriticalLogExitCode asserts that a critical log entry requests a
// shutdown that exits with the generic critical error code.
func TestCriticalLogExitCode(t *testing.T) {
	interceptor, err := signal.Intercept()
	require.NoError(t, err)

	logger := genSubLogger(
		build.NewSubLoggerManager(btclog.NewDefaultHandler(io.Discard)),
		interceptor,
	)("TEST")

	logger.Criticalf("something went wrong")

	select {
	case <-interceptor.ShutdownChannel():
	case <-time.After(time.Second):
		t.Fatal("expected critical log to request shutdown")
	}

	require.Equal(t, signal.ExitCodeCriticalError, interceptor.ExitCode())
}
