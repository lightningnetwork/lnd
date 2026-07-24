package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// trafficTestGraph builds a well-connected network with assigned liquidity
// for the traffic tests.
func trafficTestGraph(t *testing.T, liquiditySeed int64) *SimGraph {
	t.Helper()

	graph, err := GenerateSimGraph(&SimTopologySpec{
		Type:           "smallworld",
		NumNodes:       40,
		ChannelSizeSat: 1_000_000,
		Seed:           11,
		AvgDegree:      6,
	})
	require.NoError(t, err)

	require.NoError(t, graph.AssignLiquidity(
		LiquidityBimodal, liquiditySeed,
	))

	return graph
}

// balanceSnapshot captures every directional balance of the graph.
func balanceSnapshot(g *SimGraph) map[uint64][2]lnwire.MilliSatoshi {
	snapshot := make(map[uint64][2]lnwire.MilliSatoshi)
	for id, channel := range g.channels {
		snapshot[id] = [2]lnwire.MilliSatoshi{
			channel.ends[0].balance,
			channel.ends[1].balance,
		}
	}

	return snapshot
}

// TestSimTrafficMovesLiquidity asserts that background traffic settles
// payments, moves hidden balances, and conserves per-channel totals.
func TestSimTrafficMovesLiquidity(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	before := balanceSnapshot(graph)

	traffic, err := newSimTraffic(graph, &SimTrafficParams{
		PaymentsPerGap: 50,
		MinAmtMsat:     100_000,
		MaxAmtMsat:     50_000_000,
		Seed:           7,
	})
	require.NoError(t, err)

	traffic.run()

	require.Positive(t, traffic.Sent)
	require.Positive(t, traffic.Settled, "no background payment settled")

	after := balanceSnapshot(graph)

	var moved int
	for id, endsBefore := range before {
		endsAfter := after[id]

		// Per-channel conservation: the two ends always sum to the
		// same total, HTLCs only shift balance across a channel.
		require.Equal(
			t,
			endsBefore[0]+endsBefore[1],
			endsAfter[0]+endsAfter[1],
			"channel %d total changed", id,
		)

		if endsBefore != endsAfter {
			moved++
		}
	}

	require.Positive(t, moved, "traffic moved no balances")
}

// TestSimTrafficDeterminism asserts that the same seed produces the same
// traffic effects and a different seed does not.
func TestSimTrafficDeterminism(t *testing.T) {
	t.Parallel()

	run := func(trafficSeed int64) map[uint64][2]lnwire.MilliSatoshi {
		graph := trafficTestGraph(t, 3)

		traffic, err := newSimTraffic(graph, &SimTrafficParams{
			PaymentsPerGap: 30,
			MinAmtMsat:     100_000,
			MaxAmtMsat:     20_000_000,
			Seed:           trafficSeed,
		})
		require.NoError(t, err)

		traffic.run()

		return balanceSnapshot(graph)
	}

	require.Equal(t, run(1), run(1), "same seed diverged")
	require.NotEqual(t, run(1), run(2), "different seeds agreed")
}

// TestSimVirtualClock asserts that the virtual clock advances between
// payments and attempts, is visible to routers through the view, and reaches
// the mission control stack.
func TestSimVirtualClock(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 5)

	source, err := graph.ResolveNode("1")
	require.NoError(t, err)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	defer runner.Close()

	start := int64(1_800_000_000)
	runner.SetVirtualClock(&SimClockParams{
		StartUnix:     start,
		PaymentGapSec: 600,
		AttemptSec:    1,
	})

	// The mission control stack must observe the virtual clock, not the
	// wall clock.
	require.Equal(
		t, time.Unix(start, 0), runner.mcc.cfg.clock.Now(),
		"mission control still on the wall clock",
	)

	// Capture the time the router observes through its view on each
	// payment.
	var observed []time.Time
	baseFactory := runner.routerFactory
	runner.SetRouterFactory(func(view SimNetworkView, src route.Vertex,
		localBalances map[uint64]lnwire.MilliSatoshi,
		spec *SimPaymentSpec) (SimRouter, error) {

		observed = append(observed, view.Now())
		return baseFactory(view, src, localBalances, spec)
	})

	for _, target := range []string{"10", "20"} {
		_, err := runner.RunScenario(&SimScenario{
			Target:   target,
			AmtMsat:  1_000_000,
			MaxParts: 2,
		})
		require.NoError(t, err)
	}

	require.Len(t, observed, 2)

	// The first payment starts one gap after the epoch, and the second
	// starts at least another gap later (plus attempt seconds).
	firstGap := observed[0].Sub(time.Unix(start, 0))
	require.Equal(t, 600*time.Second, firstGap)

	secondGap := observed[1].Sub(observed[0])
	require.GreaterOrEqual(t, secondGap, 600*time.Second)
}
