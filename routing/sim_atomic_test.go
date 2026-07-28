package routing

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// atomicFinalCltv is the final cltv delta the hand-built test routes use.
const atomicFinalCltv = 40

// atomicChanCapSat is the capacity of every channel in the atomic mpp
// fixture, large enough that the shard amounts the tests send are a small
// fraction of it.
const atomicChanCapSat btcutil.Amount = 1_000_000

// atomicTestPolicy is the forwarding policy both ends of every fixture
// channel announce. The fees are deliberately non-zero so that the settlement
// tests compare something more interesting than a bare amount.
var atomicTestPolicy = SimPolicy{
	BaseFeeMsat:   1_000,
	FeeRatePPM:    100,
	TimeLockDelta: 40,
}

// atomicTestGraph builds the fixture the atomic mpp tests route over: a
// source with two disjoint two-hop paths to a single target.
//
//	S ── c1 ── A ── c2 ── T
//	└── c3 ── B ── c4 ──┘
//
// Every channel starts at an even split; the tests move the balances they
// care about with atomicSetBalance.
func atomicTestGraph(t *testing.T) (*SimGraph, [4]route.Vertex) {
	t.Helper()

	graph := NewSimGraph()

	var nodes [4]route.Vertex
	for i := range nodes {
		nodes[i] = SimNodePubKey(uint32(i + 1))

		_, err := graph.AddNode(nodes[i], fmt.Sprintf("n%d", i+1))
		require.NoError(t, err)
	}

	source, nodeA, nodeB, target := nodes[0], nodes[1], nodes[2], nodes[3]

	links := []struct {
		id   uint64
		a, b route.Vertex
	}{
		{1, source, nodeA},
		{2, nodeA, target},
		{3, source, nodeB},
		{4, nodeB, target},
	}
	for _, link := range links {
		require.NoError(t, graph.AddChannel(
			link.id, link.a, link.b, atomicChanCapSat,
			atomicTestPolicy, atomicTestPolicy,
		))
	}

	return graph, nodes
}

// atomicSetBalance pins the outbound balance of one end of a channel, giving
// the remainder of the capacity to the other end.
func atomicSetBalance(t *testing.T, g *SimGraph, chanID uint64,
	owner route.Vertex, balance lnwire.MilliSatoshi) {

	t.Helper()

	channel, ok := g.channels[chanID]
	require.True(t, ok, "unknown channel %d", chanID)

	capacity := lnwire.NewMSatFromSatoshis(channel.Capacity)
	require.LessOrEqual(t, balance, capacity, "balance above capacity")

	end := channel.end(owner)
	require.NotNil(t, end, "node is not a party to channel %d", chanID)

	end.balance = balance
	channel.otherEnd(owner).balance = capacity - balance
}

// atomicBalance returns the current outbound balance of one end of a channel.
func atomicBalance(t *testing.T, g *SimGraph, chanID uint64,
	owner route.Vertex) lnwire.MilliSatoshi {

	t.Helper()

	channel, ok := g.channels[chanID]
	require.True(t, ok, "unknown channel %d", chanID)

	end := channel.end(owner)
	require.NotNil(t, end, "node is not a party to channel %d", chanID)

	return end.balance
}

// atomicTestRoute builds a well-formed route from the source over the given
// channels, delivering amt to the far end of the last one. Amounts and
// expiries accumulate backward the way a real sender's path finding does, so
// the route clears the forwarding checks of every node it crosses.
func atomicTestRoute(t *testing.T, g *SimGraph, source route.Vertex,
	chanIDs []uint64, amt lnwire.MilliSatoshi) *route.Route {

	t.Helper()

	require.NotEmpty(t, chanIDs, "route needs at least one channel")

	// Walk forward to learn the node sequence the channels describe.
	nodes := []route.Vertex{source}
	for _, id := range chanIDs {
		channel, ok := g.channels[id]
		require.True(t, ok, "unknown channel %d", id)

		next := channel.otherEnd(nodes[len(nodes)-1])
		require.NotNil(t, next, "channel %d does not extend the "+
			"route", id)

		nodes = append(nodes, next.owner)
	}

	// Walk backward to accumulate the amount and expiry each channel has
	// to carry, adding the fee and delta of the node that forwards onto
	// the following channel.
	last := len(chanIDs) - 1
	amts := make([]lnwire.MilliSatoshi, len(chanIDs))
	expiries := make([]uint32, len(chanIDs))
	amts[last] = amt
	expiries[last] = atomicFinalCltv

	for k := last - 1; k >= 0; k-- {
		policy := &g.channels[chanIDs[k+1]].end(nodes[k+1]).policy

		amts[k] = amts[k+1] + policy.fee(amts[k+1])
		expiries[k] = expiries[k+1] + uint32(policy.TimeLockDelta)
	}

	// AmtToForward is the amount the hop's node sends ONWARD, which is
	// what the next channel carries; the final hop forwards nothing, so it
	// carries the delivered amount itself.
	hops := make([]*route.Hop, len(chanIDs))
	for j := range chanIDs {
		amtToForward, outgoingTimeLock := amt, uint32(atomicFinalCltv)
		if j < last {
			amtToForward = amts[j+1]
			outgoingTimeLock = expiries[j+1]
		}

		hops[j] = &route.Hop{
			PubKeyBytes:      nodes[j+1],
			ChannelID:        chanIDs[j],
			AmtToForward:     amtToForward,
			OutgoingTimeLock: outgoingTimeLock,
		}
	}

	return &route.Route{
		TotalAmount:   amts[0],
		TotalTimeLock: expiries[0],
		SourcePubKey:  source,
		Hops:          hops,
	}
}

// scriptedRouter is a SimRouter that hands back a fixed list of routes in
// order and records what came of each. It stands in for a candidate algorithm
// wherever a test needs the shard sequence to be exactly what it asked for.
type scriptedRouter struct {
	routes []*route.Route

	// next is the index of the route the following RequestRoute returns.
	next int

	// results holds the resolution of every attempt, in order.
	results []SimHtlcResult

	// onReport, when set, runs after each attempt is recorded, the hook a
	// test uses to observe the network mid-payment.
	onReport func()
}

// RequestRoute returns the next scripted route, failing the payment once the
// script runs out.
//
// NOTE: Part of the SimRouter interface.
func (s *scriptedRouter) RequestRoute(_ lnwire.MilliSatoshi,
	_ uint32) (*route.Route, error) {

	if s.next >= len(s.routes) {
		return nil, fmt.Errorf("scripted router out of routes")
	}

	rt := s.routes[s.next]
	s.next++

	return rt, nil
}

// ReportAttempt records the outcome and runs the observation hook.
//
// NOTE: Part of the SimRouter interface.
func (s *scriptedRouter) ReportAttempt(_ uint64, _ *route.Route,
	result SimHtlcResult) error {

	s.results = append(s.results, result)
	if s.onReport != nil {
		s.onReport()
	}

	return nil
}

// atomicRunner builds a runner over the given graph that always uses the
// supplied scripted router.
func atomicRunner(t *testing.T, g *SimGraph, source route.Vertex,
	router *scriptedRouter) *SimRunner {

	t.Helper()

	runner, err := NewSimRunner(g, DefaultSimParams(), source, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	runner.SetRouterFactory(func(_ SimNetworkView, _ route.Vertex,
		_ map[uint64]lnwire.MilliSatoshi,
		_ *SimPaymentSpec) (SimRouter, error) {

		return router, nil
	})

	return runner
}

// requireNoHolds asserts that the graph is not holding any liquidity, the
// invariant that must be true whenever a payment has finished resolving.
func requireNoHolds(t *testing.T, g *SimGraph) {
	t.Helper()

	require.Empty(t, g.holds, "holds outlived the payment")

	for id, channel := range g.channels {
		for i := range channel.ends {
			require.Zero(
				t, channel.ends[i].held,
				"channel %d end %d still holds liquidity",
				id, i,
			)
		}
	}
}

// TestSimAtomicMppRollsBackFailedPayment is the atomicity test: a payment that
// delivers a shard and then runs out of routes must leave every hidden balance
// exactly where it found it, and must pay nothing for the shard it rolled
// back.
func TestSimAtomicMppRollsBackFailedPayment(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	graph, nodes := atomicTestGraph(t)
	source, nodeB, target := nodes[0], nodes[2], nodes[3]

	// The path through B is a dead end: B has next to nothing on its side
	// of the channel into the target, so the second shard cannot cross it.
	atomicSetBalance(t, graph, 4, nodeB, 1_000)

	routes := []*route.Route{
		atomicTestRoute(t, graph, source, []uint64{1, 2}, shard),
		atomicTestRoute(t, graph, source, []uint64{3, 4}, shard),
	}
	router := &scriptedRouter{routes: routes}
	runner := atomicRunner(t, graph, source, router)

	before := balanceSnapshot(graph)

	result, err := runner.RunScenario(&SimScenario{
		Target:    target.String(),
		AmtMsat:   uint64(2 * shard),
		MaxParts:  2,
		AtomicMpp: true,
	})
	require.NoError(t, err)

	// The first shard arrived, the second failed on B's depleted channel,
	// and the router then ran out of routes.
	require.False(t, result.Success)
	require.Len(t, result.Attempts, 2)
	require.True(t, result.Attempts[0].Success)
	require.False(t, result.Attempts[1].Success)

	// Nothing settled, so nothing moved and nothing was paid for.
	require.Equal(t, before, balanceSnapshot(graph), "failed atomic mpp "+
		"left balances moved")
	require.Zero(t, result.FeeMsat)
	require.EqualValues(t, shard, result.HeldReleasedMsat)
	requireNoHolds(t, graph)
}

// TestSimAtomicMppFlagOff is the regression guard on the historical
// behavior: with the flag off the same payment settles its first shard the
// instant it arrives, so a failure leaves that shard's balances moved and its
// fee paid.
func TestSimAtomicMppFlagOff(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	graph, nodes := atomicTestGraph(t)
	source, nodeA, nodeB, target := nodes[0], nodes[1], nodes[2], nodes[3]

	atomicSetBalance(t, graph, 4, nodeB, 1_000)

	first := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)
	routes := []*route.Route{
		first,
		atomicTestRoute(t, graph, source, []uint64{3, 4}, shard),
	}
	router := &scriptedRouter{routes: routes}
	runner := atomicRunner(t, graph, source, router)

	var (
		sourceOnC1 = atomicBalance(t, graph, 1, source)
		nodeAOnC1  = atomicBalance(t, graph, 1, nodeA)
		nodeAOnC2  = atomicBalance(t, graph, 2, nodeA)
		targetOnC2 = atomicBalance(t, graph, 2, target)
	)
	beforeB := balanceSnapshot(graph)

	result, err := runner.RunScenario(&SimScenario{
		Target:   target.String(),
		AmtMsat:  uint64(2 * shard),
		MaxParts: 2,
	})
	require.NoError(t, err)

	require.False(t, result.Success)
	require.Len(t, result.Attempts, 2)

	// The settled shard moved its route total onto the first channel and
	// the shard amount onto the second, leaving A the fee in between.
	require.Equal(
		t, sourceOnC1-first.TotalAmount,
		atomicBalance(t, graph, 1, source),
	)
	require.Equal(
		t, nodeAOnC1+first.TotalAmount,
		atomicBalance(t, graph, 1, nodeA),
	)
	require.Equal(t, nodeAOnC2-shard, atomicBalance(t, graph, 2, nodeA))
	require.Equal(t, targetOnC2+shard, atomicBalance(t, graph, 2, target))

	// The failed shard unwound completely, as it always has.
	after := balanceSnapshot(graph)
	require.Equal(t, beforeB[3], after[3], "failed shard moved channel 3")
	require.Equal(t, beforeB[4], after[4], "failed shard moved channel 4")

	// The fee of the settled shard is charged even though the payment
	// failed, and the held-liquidity accounting stays out of the way.
	require.EqualValues(t, first.TotalFees(), result.FeeMsat)
	require.Zero(t, result.HeldReleasedMsat)
	requireNoHolds(t, graph)
}

// TestSimAtomicMppShardContention asserts that held shards genuinely reserve
// liquidity: two shards over one channel that only covers one of them fail the
// same way a plain liquidity shortfall does today.
func TestSimAtomicMppShardContention(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	// run sends two identical shards down the same path, over a channel
	// whose middle hop can fund one and a half of them.
	run := func(atomicMpp bool) ([]SimHtlcResult, []SimAttemptTrace) {
		graph, nodes := atomicTestGraph(t)
		source, nodeA, target := nodes[0], nodes[1], nodes[3]

		atomicSetBalance(t, graph, 2, nodeA, shard+shard/2)

		rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)
		router := &scriptedRouter{routes: []*route.Route{rt, rt}}
		runner := atomicRunner(t, graph, source, router)

		result, err := runner.RunScenario(&SimScenario{
			Target:    target.String(),
			AmtMsat:   uint64(2 * shard),
			MaxParts:  2,
			AtomicMpp: atomicMpp,
		})
		require.NoError(t, err)
		require.False(t, result.Success)
		requireNoHolds(t, graph)

		return router.results, result.Attempts
	}

	held, heldTraces := run(true)
	settled, settledTraces := run(false)

	require.Len(t, held, 2)
	require.Len(t, settled, 2)

	// The first shard clears in both modes; the second runs into the same
	// shortfall at the same node whether the first one is held or settled.
	require.Nil(t, held[0].Failure)
	require.Nil(t, settled[0].Failure)

	require.IsType(
		t, &lnwire.FailTemporaryChannelFailure{}, held[1].Failure,
		"held shard did not reserve the channel",
	)
	require.Equal(t, settled[1].FailureSource, held[1].FailureSource)
	require.IsType(t, settled[1].Failure, held[1].Failure)
	require.Equal(t, settledTraces, heldTraces)
}

// TestSimAtomicMppSettlesLikeNonAtomic asserts that a payment that does
// complete moves exactly the balances and charges exactly the fees the
// eagerly settling simulator would have.
func TestSimAtomicMppSettlesLikeNonAtomic(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	run := func(atomicMpp bool) (*SimScenarioResult,
		map[uint64][2]lnwire.MilliSatoshi) {

		graph, nodes := atomicTestGraph(t)
		source, target := nodes[0], nodes[3]

		routes := []*route.Route{
			atomicTestRoute(
				t, graph, source, []uint64{1, 2}, shard,
			),
			atomicTestRoute(
				t, graph, source, []uint64{3, 4}, shard,
			),
		}
		router := &scriptedRouter{routes: routes}
		runner := atomicRunner(t, graph, source, router)

		result, err := runner.RunScenario(&SimScenario{
			Target:    target.String(),
			AmtMsat:   uint64(2 * shard),
			MaxParts:  2,
			AtomicMpp: atomicMpp,
		})
		require.NoError(t, err)
		require.True(t, result.Success, "two shard payment failed")
		requireNoHolds(t, graph)

		return result, balanceSnapshot(graph)
	}

	atomicResult, atomicBalances := run(true)
	plainResult, plainBalances := run(false)

	require.Equal(t, plainBalances, atomicBalances, "atomic settlement "+
		"moved different balances")

	// The echoed scenario is the one thing that legitimately differs, so
	// normalize the flag away and require everything else to match.
	atomicResult.Scenario.AtomicMpp = false
	require.Equal(t, plainResult, atomicResult, "atomic settlement "+
		"produced a different result")
	require.Positive(t, atomicResult.FeeMsat, "fee assertion is vacuous")
}

// TestSimAtomicMppDrift asserts that the world keeps turning during an atomic
// payment: background traffic moves hidden liquidity between attempts, it does
// so identically for a given seed, and with the flag off the network stays
// frozen for the duration of the payment as it always has.
func TestSimAtomicMppDrift(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(1_000_000)

	// run scripts three shards down a path whose second hop is switched
	// off, so every attempt fails and the only thing that can move a
	// balance is the background traffic.
	run := func(atomicMpp bool,
		trafficSeed int64) []map[uint64][2]lnwire.MilliSatoshi {

		graph, nodes := atomicTestGraph(t)
		source, nodeB, target := nodes[0], nodes[2], nodes[3]

		graph.channels[4].end(nodeB).policy.Disabled = true

		rt := atomicTestRoute(t, graph, source, []uint64{3, 4}, shard)
		router := &scriptedRouter{routes: []*route.Route{rt, rt, rt}}

		var snapshots []map[uint64][2]lnwire.MilliSatoshi
		router.onReport = func() {
			snapshots = append(snapshots, balanceSnapshot(graph))
		}

		runner := atomicRunner(t, graph, source, router)
		runner.SetVirtualClock(&SimClockParams{
			StartUnix:     1_800_000_000,
			PaymentGapSec: 600,
			AttemptSec:    60,
		})
		require.NoError(t, runner.SetBackgroundTraffic(
			&SimTrafficParams{
				PaymentsPerGap: 100,
				MinAmtMsat:     100_000,
				MaxAmtMsat:     10_000_000,
				Seed:           trafficSeed,
			},
		))

		result, err := runner.RunScenario(&SimScenario{
			Target:    target.String(),
			AmtMsat:   uint64(4 * shard),
			MaxParts:  4,
			AtomicMpp: atomicMpp,
		})
		require.NoError(t, err)
		require.False(t, result.Success)
		require.Len(t, result.Attempts, 3)
		requireNoHolds(t, graph)

		return snapshots
	}

	// One attempt is a tenth of a gap, so ten of the hundred background
	// payments of a gap land in each attempt's window.
	drifting := run(true, 21)
	require.Len(t, drifting, 3)
	require.NotEqual(
		t, drifting[0], drifting[2],
		"liquidity did not drift during the payment",
	)

	// The exogenous process is still a function of its seed alone.
	require.Equal(t, drifting, run(true, 21), "same seed diverged")
	require.NotEqual(t, drifting, run(true, 22), "different seeds agreed")

	// With the flag off the world freezes for the duration of a payment,
	// exactly as it did before.
	frozen := run(false, 21)
	require.Len(t, frozen, 3)
	require.Equal(t, frozen[0], frozen[2], "liquidity drifted with atomic "+
		"mpp off")
}

// TestSimHoldReservations asserts that a hold can say which directed edges it
// reserves and how much of each, which is what self-contention attribution is
// keyed on.
func TestSimHoldReservations(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	graph, nodes := atomicTestGraph(t)
	source, nodeA := nodes[0], nodes[1]

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)

	result, id, err := graph.HoldHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)
	require.NotZero(t, id)

	// A two hop route reserves the source's outbound side of the first
	// channel and node A's outbound side of the second, each for the amount
	// that channel carried.
	require.Equal(t, []simHoldReservation{
		{
			edge: simHoldEdge{ChanID: 1, From: source},
			amt:  rt.TotalAmount,
		},
		{
			edge: simHoldEdge{ChanID: 2, From: nodeA},
			amt:  rt.Hops[0].AmtToForward,
		},
	}, graph.holdReservations(id))

	// An unknown hold reserves nothing.
	require.Nil(t, graph.holdReservations(id+1))

	// Releasing it takes the reservations with it.
	graph.ReleaseHold(id)
	require.Nil(t, graph.holdReservations(id))
	requireNoHolds(t, graph)
}

// TestSimEndLiquidity asserts that the runner can read the true balance and
// the held part of a directed channel end, which is the only way to tell an
// htlc that failed because a sibling was holding the liquidity from one that
// would have failed anyway.
func TestSimEndLiquidity(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	graph, nodes := atomicTestGraph(t)
	source := nodes[0]

	atomicSetBalance(t, graph, 1, source, 2*shard)

	balance, held, ok := graph.endLiquidity(1, source)
	require.True(t, ok)
	require.Equal(t, 2*shard, balance)
	require.Zero(t, held)

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)
	_, id, err := graph.HoldHtlc(rt)
	require.NoError(t, err)

	// A held htlc moves nothing and reserves everything it crossed.
	balance, held, ok = graph.endLiquidity(1, source)
	require.True(t, ok)
	require.Equal(t, 2*shard, balance)
	require.Equal(t, rt.TotalAmount, held)

	// Settling turns the reservation into movement.
	graph.SettleHold(id)
	balance, held, ok = graph.endLiquidity(1, source)
	require.True(t, ok)
	require.Equal(t, 2*shard-rt.TotalAmount, balance)
	require.Zero(t, held)

	// An unknown channel and a node that is not a party to a known one both
	// report nothing rather than a zero balance.
	_, _, ok = graph.endLiquidity(99, source)
	require.False(t, ok)

	_, _, ok = graph.endLiquidity(2, source)
	require.False(t, ok)
}
