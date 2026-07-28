package routing

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSimFeeBudgetMsat checks the ppm arithmetic against hand-computed values,
// including the two cases the split multiplication exists for: an amount whose
// remainder carries the whole budget, and an amount large enough that the
// direct product would leave the range of a uint64.
func TestSimFeeBudgetMsat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		amt  lnwire.MilliSatoshi
		ppm  uint32
		want lnwire.MilliSatoshi
	}{
		{
			name: "no limit",
			amt:  1_000_000_000,
			ppm:  0,
			want: lnwire.MaxMilliSatoshi,
		},
		{
			name: "3000 ppm of a million sats",
			amt:  1_000_000_000,
			ppm:  3_000,
			want: 3_000_000,
		},
		{
			name: "one ppm is the smallest real budget",
			amt:  1_000_000,
			ppm:  1,
			want: 1,
		},
		{
			// The quotient is zero here, so the whole budget comes
			// out of the remainder term.
			name: "amount below a million msat",
			amt:  999_999,
			ppm:  500_000,
			want: 499_999,
		},
		{
			// Rounding is down, as it is everywhere else a fee is
			// computed: a budget is what the sender WILL pay.
			name: "rounds down",
			amt:  1_500_000,
			ppm:  1,
			want: 1,
		},
		{
			// 21e14 msat times 1e7 ppm overflows a uint64 if it is
			// multiplied out directly. Split, it does not.
			name: "whole supply at a thousand percent",
			amt:  2_100_000_000_000_000,
			ppm:  10_000_000,
			want: 21_000_000_000_000_000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.want,
				simFeeBudgetMsat(test.amt, test.ppm),
			)
		})
	}
}

// TestSimRemainingBudget checks that the budget left after committed fees is
// lnd's calcFeeBudget: the difference, floored at zero on overrun.
func TestSimRemainingBudget(t *testing.T) {
	t.Parallel()

	require.EqualValues(t, 700, simRemainingBudget(1_000, 300))
	require.EqualValues(t, 0, simRemainingBudget(1_000, 1_000))
	require.EqualValues(t, 0, simRemainingBudget(1_000, 1_500))
	require.Equal(
		t, lnwire.MaxMilliSatoshi,
		simRemainingBudget(lnwire.MaxMilliSatoshi, 0),
	)
}

// feeLimitSpecRunner builds a runner over the two-path fixture whose router
// records the payment spec it was handed and then abandons the payment, which
// is all a test of the contract surface needs: the spec is delivered at
// construction, before any route is requested.
func feeLimitSpecRunner(t *testing.T) (*SimRunner, *[]lnwire.MilliSatoshi) {
	t.Helper()

	graph, nodes := atomicTestGraph(t)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), nodes[0], t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	var seen []lnwire.MilliSatoshi
	runner.SetRouterFactory(func(_ SimNetworkView, _ route.Vertex,
		_ map[uint64]lnwire.MilliSatoshi,
		spec *SimPaymentSpec) (SimRouter, error) {

		seen = append(seen, spec.FeeLimitMsat)

		return &scriptedRouter{}, nil
	})

	return runner, &seen
}

// TestSimFeeLimitAbsentGolden is the identity claim of stage C's contract
// half: a scenario that names no fee limit hands its router the same unlimited
// budget the lnd arm has been constructed with since the program began. If
// this number ever changes, every published result moves, because a finite
// budget is a constraint on which routes exist.
func TestSimFeeLimitAbsentGolden(t *testing.T) {
	t.Parallel()

	runner, seen := feeLimitSpecRunner(t)

	_, err := runner.RunScenario(&SimScenario{
		Target:   "4",
		AmtMsat:  100_000_000,
		MaxParts: 1,
	})
	require.NoError(t, err)

	require.Equal(t, []lnwire.MilliSatoshi{lnwire.MaxMilliSatoshi}, *seen)
}

// TestSimFeeLimitReachesTheSpec is the other half of the golden above: a limit
// that IS named arrives at the router as a real budget, so the sentinel is a
// sentinel rather than an accident of dead plumbing.
func TestSimFeeLimitReachesTheSpec(t *testing.T) {
	t.Parallel()

	runner, seen := feeLimitSpecRunner(t)

	_, err := runner.RunScenario(&SimScenario{
		Target:      "4",
		AmtMsat:     100_000_000,
		MaxParts:    1,
		FeeLimitPPM: 3_000,
	})
	require.NoError(t, err)

	require.Equal(t, []lnwire.MilliSatoshi{300_000}, *seen)
}

// feeLimitLndBatch is the payment batch the lnd arm runs against a ladder of
// budgets below. The targets are far enough from the source on the fixture
// network that the routes carry real fees, which is what makes a budget bind
// on some of them.
func feeLimitLndBatch() []SimScenario {
	return []SimScenario{
		{Target: "20", AmtMsat: 200_000_000, MaxParts: 4},
		{Target: "31", AmtMsat: 300_000_000, MaxParts: 4},
		{Target: "12", AmtMsat: 250_000_000, MaxParts: 4},
		{Target: "37", AmtMsat: 150_000_000, MaxParts: 4},
	}
}

// feeLimitLndRun runs that batch on the stock lnd stack under one budget, and
// reports what it completed, what it paid and what the backstop saw.
func feeLimitLndRun(t *testing.T, ppm uint32) (int, lnwire.MilliSatoshi,
	SimFeeLimitStats) {

	t.Helper()

	graph, err := GenerateSimGraph(&SimTopologySpec{
		Type:           "smallworld",
		NumNodes:       40,
		ChannelSizeSat: 1_000_000,
		Seed:           11,
		AvgDegree:      6,
	})
	require.NoError(t, err)
	require.NoError(t, graph.AssignLiquidity(LiquidityBimodal, 5))

	source, err := graph.ResolveNode("1")
	require.NoError(t, err)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	var (
		successes int
		fees      lnwire.MilliSatoshi
	)
	for _, scenario := range feeLimitLndBatch() {
		scenario.FeeLimitPPM = ppm

		result, err := runner.RunScenario(&scenario)
		require.NoError(t, err)

		if !result.Success {
			continue
		}
		successes++
		fees += lnwire.MilliSatoshi(result.FeeMsat)

		// Whatever it completed, it completed inside the budget.
		budget := simFeeBudgetMsat(
			lnwire.MilliSatoshi(scenario.AmtMsat), ppm,
		)
		require.LessOrEqual(
			t, lnwire.MilliSatoshi(result.FeeMsat), budget,
			"lnd settled a payment over its budget",
		)
	}

	return successes, fees, runner.FeeLimitStats()
}

// TestSimFeeLimitLndPrunesInsteadOfBeingRefused is the agreement test the
// design spec asks for, and it is the third confirmation of the same lesson:
// a constraint the arms can see binds at PLAN time. lnd's path finding prunes
// on the budget it was handed, so across a ladder of budgets from generous to
// punishing it never offers the runner a route it cannot afford, and the
// backstop reads zero everywhere.
//
// The last rung is the manipulation check. A ladder on which nothing ever
// binds would pass the zero assertion for the wrong reason, so the test also
// requires the tight rung to have cost the arm something real.
func TestSimFeeLimitLndPrunesInsteadOfBeingRefused(t *testing.T) {
	t.Parallel()

	baseSuccesses, baseFees, baseStats := feeLimitLndRun(t, 0)
	require.Equal(t, SimFeeLimitStats{}, baseStats)
	require.Positive(t, baseSuccesses, "the batch completed nothing")

	var tight int
	for _, ppm := range []uint32{100_000, 10_000, 2_000, 200} {
		successes, fees, stats := feeLimitLndRun(t, ppm)

		require.Zero(
			t, stats.Failures,
			"the backstop fired on the lnd arm at %d ppm", ppm,
		)
		require.Equal(t, len(feeLimitLndBatch()), stats.Payments)

		// A budget can only ever cost the arm: it removes routes, it
		// never adds one.
		require.LessOrEqual(t, successes, baseSuccesses)
		require.LessOrEqual(t, fees, baseFees)

		tight = successes
	}

	require.Less(
		t, tight, baseSuccesses,
		"no rung of the ladder bound, so the zeroes above prove "+
			"nothing",
	)
}

// feeLimitShardFee is what the fixture's middle node charges to forward one
// shard of feeLimitShard: its announced base fee plus its rate on the amount.
// Every budget below is quoted against it.
const (
	feeLimitShard    = lnwire.MilliSatoshi(100_000_000)
	feeLimitShardFee = lnwire.MilliSatoshi(11_000)
)

// runFeeLimitPayment sends one scripted route of feeLimitShard over the
// two-hop fixture under the given budget, and reports what the network, the
// router and the counters saw.
func runFeeLimitPayment(t *testing.T, feeLimitPPM uint32) (*SimScenarioResult,
	*scriptedRouter, *SimRunner, *SimGraph) {

	t.Helper()

	graph, nodes := atomicTestGraph(t)
	source, target := nodes[0], nodes[3]

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, feeLimitShard)
	require.Equal(t, feeLimitShardFee, rt.TotalFees())

	router := &scriptedRouter{routes: []*route.Route{rt}}
	runner := atomicRunner(t, graph, source, router)

	result, err := runner.RunScenario(&SimScenario{
		Target:      target.String(),
		AmtMsat:     uint64(feeLimitShard),
		MaxParts:    1,
		FeeLimitPPM: feeLimitPPM,
	})
	require.NoError(t, err)

	return result, router, runner, graph
}

// TestSimFeeLimitRefusesOverBudgetRoute is the load-bearing enforcement claim:
// a route the payment cannot afford is never sent. The htlc does not reach the
// network, no balance moves, the attempt is recorded and named, and the router
// is told why in a form it can switch on.
func TestSimFeeLimitRefusesOverBudgetRoute(t *testing.T) {
	t.Parallel()

	// A hundred ppm of the shard is 10,000 msat against a route fee of
	// 11,000: over budget by a thousand.
	result, router, runner, graph := runFeeLimitPayment(t, 100)

	require.False(t, result.Success)
	require.Zero(t, result.FeeMsat)

	require.Len(t, result.Attempts, 1)
	require.False(t, result.Attempts[0].Success)
	require.Equal(t, simFeeLimitFailureName, result.Attempts[0].Failure)
	require.Zero(
		t, result.Attempts[0].FailureIdx,
		"a refusal is the sender's own, so it is attributed to hop 0",
	)

	require.Len(t, router.results, 1)
	require.IsType(t, SimFeeLimitFailure{}, router.results[0].Failure)

	stats := runner.FeeLimitStats()
	require.Equal(t, 1, stats.Payments)
	require.Equal(t, 1, stats.Failures)

	// Nothing crossed the wire, so nothing moved and nothing was learned.
	require.Equal(
		t, lnwire.NewMSatFromSatoshis(atomicChanCapSat/2),
		atomicBalance(t, graph, 1, SimNodePubKey(1)),
	)
	require.Empty(t, runner.Observations())
	requireNoHolds(t, graph)
}

// TestSimFeeLimitAllowsRouteWithinBudget is the control for the refusal above:
// the same route under a budget that covers it is sent, settles, and trips no
// counter.
func TestSimFeeLimitAllowsRouteWithinBudget(t *testing.T) {
	t.Parallel()

	// Two hundred ppm of the shard is 20,000 msat against a route fee of
	// 11,000.
	result, router, runner, graph := runFeeLimitPayment(t, 200)

	require.True(t, result.Success)
	require.EqualValues(t, feeLimitShardFee, result.FeeMsat)

	require.Len(t, router.results, 1)
	require.Nil(t, router.results[0].Failure)

	require.Zero(t, runner.FeeLimitStats().Failures)
	require.Equal(t, 1, runner.FeeLimitStats().Payments)
	requireNoHolds(t, graph)
}

// TestSimFeeLimitAbsentSendsEverything is the knob-off claim at the wire: with
// no budget named, the very route the tight budget refused is dispatched and
// settles, and no counter reads anything at all.
func TestSimFeeLimitAbsentSendsEverything(t *testing.T) {
	t.Parallel()

	result, _, runner, _ := runFeeLimitPayment(t, 0)

	require.True(t, result.Success)
	require.EqualValues(t, feeLimitShardFee, result.FeeMsat)
	require.Equal(t, SimFeeLimitStats{}, runner.FeeLimitStats())
}

// TestSimFeeLimitSpendsAcrossShards asserts that the budget is a budget for
// the whole PAYMENT and not for each attempt: the fee of a shard that already
// went through is subtracted before the next shard is priced. It runs both
// settlement modes because the fees of a held shard are committed in a
// different place from the fees of a settled one, and a budget that watched
// only one of them would be twice as generous under atomic mpp.
func TestSimFeeLimitSpendsAcrossShards(t *testing.T) {
	t.Parallel()

	// Two shards of half the amount each. The middle node charges its base
	// fee once per shard, so splitting a payment in two costs strictly more
	// in fees than sending it whole.
	const (
		half   = feeLimitShard / 2
		perFee = lnwire.MilliSatoshi(6_000)
	)

	run := func(t *testing.T, atomicMpp bool, ppm uint32) (
		*SimScenarioResult, *SimRunner) {

		graph, nodes := atomicTestGraph(t)
		source, target := nodes[0], nodes[3]

		first := atomicTestRoute(
			t, graph, source, []uint64{1, 2}, half,
		)
		second := atomicTestRoute(
			t, graph, source, []uint64{3, 4}, half,
		)
		require.Equal(t, perFee, first.TotalFees())
		require.Equal(t, perFee, second.TotalFees())

		router := &scriptedRouter{
			routes: []*route.Route{first, second},
		}
		runner := atomicRunner(t, graph, source, router)

		result, err := runner.RunScenario(&SimScenario{
			Target:      target.String(),
			AmtMsat:     uint64(feeLimitShard),
			MaxParts:    2,
			AtomicMpp:   atomicMpp,
			FeeLimitPPM: ppm,
		})
		require.NoError(t, err)
		requireNoHolds(t, graph)

		return result, runner
	}

	for _, atomicMpp := range []bool{false, true} {
		t.Run(fmt.Sprintf("atomic=%v", atomicMpp), func(t *testing.T) {
			t.Parallel()

			// 120 ppm of the payment is 12,000 msat, exactly what
			// the two shards cost together.
			result, runner := run(t, atomicMpp, 120)
			require.True(t, result.Success)
			require.EqualValues(t, 2*perFee, result.FeeMsat)
			require.Zero(t, runner.FeeLimitStats().Failures)

			// 110 ppm is 11,000: enough for the first shard, one
			// thousand short of the second. The payment fails
			// having spent the first shard's fee and nothing more.
			result, runner = run(t, atomicMpp, 110)
			require.False(t, result.Success)
			require.Equal(t, 1, runner.FeeLimitStats().Failures)

			// Under atomic mpp the shard that did go through is
			// released rather than settled, so a failed payment
			// pays nothing at all. Without it the shard is spent.
			spent := lnwire.MilliSatoshi(result.FeeMsat)
			if atomicMpp {
				require.Zero(t, spent)
			} else {
				require.Equal(t, perFee, spent)
			}
		})
	}
}
