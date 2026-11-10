package routerrpc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

// TestDefaultRouter tests that the payment router defaults to lnd's production
// stack, so that a node which says nothing about routing keeps the behaviour it
// had before the interval router existed.
func TestDefaultRouter(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Equal(t, routing.DefaultPaymentRouter, cfg.PaymentRouter)

	// The selection survives the trip through GetRoutingConfig, which is
	// what the server actually reads.
	require.Equal(
		t, routing.DefaultPaymentRouter,
		GetRoutingConfig(cfg).PaymentRouter,
	)

	cfg.PaymentRouter = routing.IntervalPaymentRouter
	require.Equal(
		t, routing.IntervalPaymentRouter,
		GetRoutingConfig(cfg).PaymentRouter,
	)
}

// TestIntervalFlushInterval tests that the interval router's flush cadence has
// a default of its own and survives the trip through GetRoutingConfig, so that
// tuning it does not mean tuning mission control's cadence as well.
func TestIntervalFlushInterval(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Equal(
		t, routing.DefaultIntervalFlushInterval,
		cfg.IntervalFlushInterval,
	)
	require.Equal(
		t, routing.DefaultIntervalFlushInterval,
		GetRoutingConfig(cfg).IntervalFlushInterval,
	)

	// The two cadences move independently.
	cfg.IntervalFlushInterval = 42 * time.Second
	routingCfg := GetRoutingConfig(cfg)

	require.Equal(t, 42*time.Second, routingCfg.IntervalFlushInterval)
	require.Equal(
		t, routing.DefaultMcFlushInterval, routingCfg.McFlushInterval,
	)
}
