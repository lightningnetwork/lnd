package routing

import (
	"sort"
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

// TestSimTrafficSettleRate asserts that the exogenous process actually moves
// the liquidity a scenario asks it to move.
//
// This is a regression test for a defect that silently weakened every
// experiment that turned the traffic knob. A failed background payment moves
// no balance at all, so the settle rate is the factor between the churn a
// scenario file configures and the churn it gets; the first version of this
// engine settled well under half its payments, and under a fifth of them on
// the mainnet topology. The threshold below is deliberately far under what
// the engine now achieves, so this fails on a real regression rather than on
// ordinary drift in the generators.
func TestSimTrafficSettleRate(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)

	traffic, err := newSimTraffic(graph, &SimTrafficParams{
		PaymentsPerGap: 200,

		// A range whose upper end far exceeds what a bimodal channel
		// typically holds: the engine has to scale amounts down to
		// settle these, which is exactly the behaviour under test.
		MinAmtMsat: 100_000,
		MaxAmtMsat: 500_000_000,
		Seed:       7,
	})
	require.NoError(t, err)

	traffic.run()

	require.Positive(t, traffic.Sent)
	rate := float64(traffic.Settled) / float64(traffic.Sent)
	require.Greater(t, rate, 0.6, "only %d of %d background payments "+
		"settled; a failed payment moves no liquidity, so the "+
		"exogenous process is weaker than configured",
		traffic.Settled, traffic.Sent)
}

// TestSimTrafficPrefersConnectedNodes asserts that endpoints are drawn in
// proportion to degree rather than uniformly. On a real topology the median
// node holds a single channel, so uniform draws pick leaf-to-leaf pairs with
// no path between them at any amount.
func TestSimTrafficPrefersConnectedNodes(t *testing.T) {
	t.Parallel()

	// A hub-and-spoke graph makes the difference measurable: one node
	// holds most of the channels, so degree-weighted draws must select
	// it far more often than 1/n of the time.
	graph, err := GenerateSimGraph(&SimTopologySpec{
		Type:           "hubspoke",
		NumNodes:       50,
		ChannelSizeSat: 1_000_000,
		Seed:           5,
	})
	require.NoError(t, err)
	require.NoError(t, graph.AssignLiquidity(LiquidityBimodal, 3))

	traffic, err := newSimTraffic(graph, &SimTrafficParams{
		PaymentsPerGap: 1,
		MinAmtMsat:     1_000,
		MaxAmtMsat:     10_000,
		Seed:           42,
	})
	require.NoError(t, err)

	// Find the highest-degree node, which the draw should favour.
	var (
		hub    route.Vertex
		hubDeg int
	)
	for v, node := range graph.nodes {
		if len(node.channels) > hubDeg {
			hub, hubDeg = v, len(node.channels)
		}
	}

	const draws = 2000
	hits := 0
	for i := 0; i < draws; i++ {
		if traffic.pickNode() == hub {
			hits++
		}
	}

	uniform := 1.0 / float64(len(traffic.nodes))
	observed := float64(hits) / draws
	require.Greater(t, observed, 4*uniform,
		"hub with %d of the graph's channels drawn only %.3f of the "+
			"time, barely above the uniform rate %.3f",
		hubDeg, observed, uniform)
}

// TestSimTrafficFocus asserts that the focus set steers a share of the churn
// onto the corridors under test. Traffic spread evenly over a large graph
// almost never touches the few channels a scored payment uses, which makes
// the traffic knob move the network everywhere except where it is measured.
func TestSimTrafficFocus(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)

	nodes := make([]route.Vertex, 0, len(graph.nodes))
	for v := range graph.nodes {
		nodes = append(nodes, v)
	}
	focus := nodes[:1]

	traffic, err := newSimTraffic(graph, &SimTrafficParams{
		PaymentsPerGap: 1,
		MinAmtMsat:     1_000,
		MaxAmtMsat:     10_000,
		FocusFraction:  1.0,
		Seed:           9,
	})
	require.NoError(t, err)
	traffic.SetFocus(focus)

	// With the fraction at one, every pair must contain the focus node.
	for i := 0; i < 200; i++ {
		sender, receiver := traffic.pickPair()
		require.True(t, sender == focus[0] || receiver == focus[0],
			"focused pair %v -> %v contains neither focus node",
			sender, receiver)
	}

	// Without a focus set the fraction must have no effect.
	traffic.SetFocus(nil)
	var offFocus int
	for i := 0; i < 200; i++ {
		sender, receiver := traffic.pickPair()
		if sender != focus[0] && receiver != focus[0] {
			offFocus++
		}
	}
	require.Positive(t, offFocus, "empty focus set still steered traffic")
}

// TestSimScenarioGaveUp asserts that a payment the router ABANDONS is marked
// as such, and one that merely fails its attempts is not.
//
// exp-013 evolved a candidate that improved its attempt count by quitting on
// payments it could have completed. Under the composite objective that is
// indistinguishable from genuine efficiency — both show up as fewer attempts
// — so abandonment is reported on its own.
func TestSimScenarioGaveUp(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)

	nodes := make([]route.Vertex, 0, len(graph.nodes))
	for v := range graph.nodes {
		nodes = append(nodes, v)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].String() < nodes[j].String()
	})
	source, target := nodes[0], nodes[len(nodes)-1]

	// A router with an empty script gives up on its very first request.
	router := &scriptedRouter{}
	runner := atomicRunner(t, graph, source, router)

	result, err := runner.RunScenario(&SimScenario{
		Target:  target.String(),
		AmtMsat: 1_000_000,
	})
	require.NoError(t, err)

	require.False(t, result.Success)
	require.True(t, result.GaveUp, "router returned an error from "+
		"RequestRoute but the payment was not marked abandoned")
	require.Empty(t, result.Attempts, "a payment abandoned before its "+
		"first route should cost no attempts")
}
