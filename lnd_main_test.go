package lnd

import (
	"net"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/require"
)

// TestMainErrorShutsDownInterceptor asserts that Main error paths always close
// the signal interceptor so startup can be retried in-process.
func TestMainErrorShutsDownInterceptor(t *testing.T) {
	interceptor, err := signal.Intercept()
	require.NoError(t, err)

	cfg := DefaultConfig()
	cfg.Pprof = &lncfg.Pprof{}
	cfg.RPCListeners = []net.Addr{
		&net.UnixAddr{Net: "invalid-network"},
	}
	mainErr := Main(&cfg, ListenerCfg{}, nil, interceptor)
	require.Error(t, mainErr)

	select {
	case <-interceptor.ShutdownChannel():
	case <-time.After(5 * time.Second):
		t.Fatalf("interceptor wasn't shut down after Main returned " +
			"error")
	}
}
