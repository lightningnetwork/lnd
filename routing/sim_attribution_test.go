package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// attributionTestRoute builds a synthetic route of the given hop count,
// sourced at node 0 and running through nodes 1..numHops.
func attributionTestRoute(t *testing.T, numHops int) *route.Route {
	t.Helper()

	hops := make([]*route.Hop, numHops)
	for i := range hops {
		hops[i] = &route.Hop{
			PubKeyBytes:  SimNodePubKey(uint32(i + 1)),
			ChannelID:    uint64(i + 1),
			AmtToForward: lnwire.MilliSatoshi(1_000 - i),
		}
	}

	rt, err := route.NewRouteFromHops(
		1_000, 100, SimNodePubKey(0), hops,
	)
	require.NoError(t, err)

	return rt
}

// attributionTestFailure is a truthful failure at the given route index.
func attributionTestFailure(rt *route.Route, idx int) SimHtlcResult {
	source, _ := simRouteNodeAt(rt, idx)

	return SimHtlcResult{
		FailureSource: source,
		Failure:       &lnwire.FailTemporaryChannelFailure{},
	}
}

// recordingRouter wraps a router and keeps every result it was told about, so
// a test can inspect the channel from the consumer's side rather than the
// simulator's.
type recordingRouter struct {
	inner SimRouter
	seen  []SimHtlcResult
	route []*route.Route
}

func (r *recordingRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	return r.inner.RequestRoute(amt, inFlightHtlcs)
}

func (r *recordingRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result SimHtlcResult) error {

	r.seen = append(r.seen, result)
	r.route = append(r.route, rt)

	return r.inner.ReportAttempt(attemptID, rt, result)
}

// attributionRunner builds a runner over a hard little network, recording
// everything the router under test is told.
func attributionRunner(t *testing.T,
	params *SimAttributionParams) (*SimRunner, *recordingRouter) {

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

	recorder := &recordingRouter{}
	runner.SetRouterFactory(func(view SimNetworkView, src route.Vertex,
		balances map[uint64]lnwire.MilliSatoshi,
		spec *SimPaymentSpec) (SimRouter, error) {

		inner, err := newLndStackRouter(
			view, runner.mc, runner.params, src, balances, spec,
		)
		if err != nil {
			return nil, err
		}
		recorder.inner = inner

		return recorder, nil
	})

	if params != nil {
		require.NoError(t, runner.SetAttribution(params, 7))
	}

	return runner, recorder
}

// attributionScenarios is a batch big enough to produce failures on the test
// network.
func attributionScenarios() []SimScenario {
	return []SimScenario{
		{Target: "20", AmtMsat: 400_000_000, MaxParts: 4},
		{Target: "31", AmtMsat: 600_000_000, MaxParts: 4},
		{Target: "12", AmtMsat: 500_000_000, MaxParts: 4},
	}
}

// runAttributionBatch runs the standard batch and returns the results.
func runAttributionBatch(t *testing.T,
	runner *SimRunner) []*SimScenarioResult {

	t.Helper()

	var results []*SimScenarioResult
	for _, scenario := range attributionScenarios() {
		result, err := runner.RunScenario(&scenario)
		require.NoError(t, err)
		results = append(results, result)
	}

	return results
}

// TestSimAttributionOffUnchanged asserts the non-interference property the
// whole instrument rests on: a configured but zeroed degradation produces
// exactly the run an unconfigured one does, attempt for attempt.
func TestSimAttributionOffUnchanged(t *testing.T) {
	t.Parallel()

	clean, _ := attributionRunner(t, nil)
	cleanResults := runAttributionBatch(t, clean)

	zeroed, _ := attributionRunner(t, &SimAttributionParams{})
	zeroedResults := runAttributionBatch(t, zeroed)

	require.Equal(t, cleanResults, zeroedResults)

	// The counters still see every attempt go past, which is what makes
	// the realized rates checkable.
	stats := zeroed.AttributionStats()
	require.Positive(t, stats.Attempts)
	require.Zero(t, stats.Unknown)
	require.Zero(t, stats.Shifted)
	require.Zero(t, stats.Delayed)
	require.Zero(t, clean.AttributionStats().Attempts)
}

// TestSimAttributionUnknownStrips asserts that an unattributed failure reaches
// the consumer with no source and no readable code, while a settled attempt is
// left alone.
func TestSimAttributionUnknownStrips(t *testing.T) {
	t.Parallel()

	runner, recorder := attributionRunner(t, &SimAttributionParams{
		UnknownProb: 1,
	})
	runAttributionBatch(t, runner)

	require.NotEmpty(t, recorder.seen)

	var failures int
	for i, result := range recorder.seen {
		if result.Failure == nil {
			continue
		}
		failures++

		// The source is gone: no index lookup on the route can find
		// it, which is the exact condition both consumer paths key
		// their no-information handling on.
		require.Equal(t, simUnknownSource, result.FailureSource)
		require.Nil(
			t, getNodeIndexSim(
				recorder.route[i], result.FailureSource,
			),
		)

		// And so is the code.
		require.IsType(t, SimUnknownFailure{}, result.Failure)
		require.Equal(t, lnwire.CodeNone, result.Failure.Code())
	}

	require.Positive(t, failures, "no failure to degrade")
	require.Equal(t, failures, runner.AttributionStats().Unknown)
}

// TestSimAttributionUnknownLndPath asserts that the lnd consumer answers a
// stripped failure with its own production logic for an unreadable onion
// error: processPaymentOutcomeUnknown, which penalizes every pair of the route
// because any of them could be responsible. The truthful failure penalizes one
// pair and credits the ones before it, so the two are easy to tell apart in
// mission control's own state.
func TestSimAttributionUnknownLndPath(t *testing.T) {
	t.Parallel()

	graph, err := GenerateSimGraph(&SimTopologySpec{
		Type:           "line",
		NumNodes:       5,
		ChannelSizeSat: 1_000_000,
		Seed:           3,
	})
	require.NoError(t, err)
	require.NoError(t, graph.AssignLiquidity(LiquidityUniform, 5))

	source, err := graph.ResolveNode("1")
	require.NoError(t, err)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	defer runner.Close()

	target, err := graph.ResolveNode("5")
	require.NoError(t, err)

	spec := &SimPaymentSpec{
		Target: target, Amount: 10_000, MaxParts: 1,
	}
	router, err := newLndStackRouter(
		&simGossipView{g: graph}, runner.mc, runner.params, source,
		graph.LocalBalances(source), spec,
	)
	require.NoError(t, err)

	rt, err := router.RequestRoute(10_000, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rt.Hops), 3)

	// Count the pairs mission control considers failed.
	failedPairs := func() int {
		var failed int
		for _, pair := range runner.mc.GetHistorySnapshot().Pairs {
			if !pair.FailTime.IsZero() {
				failed++
			}
		}

		return failed
	}

	// A truthful failure at the second hop blames exactly one pair.
	truthful := attributionTestFailure(rt, 2)
	require.NoError(t, router.ReportAttempt(0, rt, truthful))
	require.Equal(t, 1, failedPairs())

	require.NoError(t, runner.mc.ResetHistory())
	require.Zero(t, failedPairs())

	// The degraded version of that same failure blames all of them.
	attribution, err := newSimAttribution(
		&SimAttributionParams{UnknownProb: 1}, 1,
	)
	require.NoError(t, err)

	// processPaymentOutcomeUnknown penalizes every pair of the route, and
	// failPair marks a pair in both directions, so a route of n hops
	// leaves 2n failed pairs behind where the truthful failure left one.
	degraded := attribution.degrade(rt, truthful)
	require.NoError(t, router.ReportAttempt(1, rt, degraded))
	require.Equal(t, 2*len(rt.Hops), failedPairs())
}

// TestSimAttributionShiftAdjacent asserts that a shifted failure lands on a
// neighbour of the node that really failed, never anywhere else, and that the
// failure code survives the move untouched.
func TestSimAttributionShiftAdjacent(t *testing.T) {
	t.Parallel()

	rt := attributionTestRoute(t, 4)

	attribution, err := newSimAttribution(
		&SimAttributionParams{ShiftProb: 1}, 42,
	)
	require.NoError(t, err)

	// Every position on the route, including both ends where one of the
	// two neighbours does not exist.
	var moved int
	for idx := 0; idx <= len(rt.Hops); idx++ {
		for i := 0; i < 50; i++ {
			truthful := attributionTestFailure(rt, idx)

			degraded := attribution.degrade(rt, truthful)
			require.Equal(
				t, truthful.Failure.Code(),
				degraded.Failure.Code(),
			)

			got := getNodeIndexSim(rt, degraded.FailureSource)
			require.NotNil(t, got)
			require.Equal(t, 1, abs(*got-idx),
				"blamed index %v is not adjacent to %v",
				*got, idx)
			moved++
		}
	}

	require.Equal(t, moved, attribution.stats.Shifted)
}

// abs returns the absolute value of an int.
func abs(v int) int {
	if v < 0 {
		return -v
	}

	return v
}

// TestSimAttributionDeterminism asserts that the degradation sequence is a
// function of the seed and the attempt index alone: the same seed replays
// exactly, a derived seed replays too, and a different seed does not.
func TestSimAttributionDeterminism(t *testing.T) {
	t.Parallel()

	rt := attributionTestRoute(t, 4)

	sequence := func(params *SimAttributionParams,
		defaultSeed int64) []route.Vertex {

		attribution, err := newSimAttribution(params, defaultSeed)
		require.NoError(t, err)

		var sources []route.Vertex
		for i := 0; i < 200; i++ {
			result := attribution.degrade(
				rt, attributionTestFailure(rt, 2),
			)
			sources = append(sources, result.FailureSource)
		}

		return sources
	}

	params := &SimAttributionParams{UnknownProb: 0.3, ShiftProb: 0.3}

	// A pinned seed replays.
	pinned := &SimAttributionParams{
		UnknownProb: 0.3, ShiftProb: 0.3, Seed: 99,
	}
	require.Equal(t, sequence(pinned, 1), sequence(pinned, 2),
		"a pinned seed must ignore the derived one")

	// An omitted seed is derived from the scenario's liquidity seed, so
	// it replays too, and two liquidity seeds diverge.
	require.Equal(t, sequence(params, 7), sequence(params, 7))
	require.NotEqual(t, sequence(params, 7), sequence(params, 8))

	// The draw sequence must not depend on the outcomes it is applied to:
	// a run of settled attempts consumes the same stream as a run of
	// failed ones, so two routers that fail at different rates still face
	// the same degradation at the same attempt index.
	interleaved, err := newSimAttribution(params, 7)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		interleaved.degrade(rt, SimHtlcResult{})
	}
	var tail []route.Vertex
	for i := 0; i < 100; i++ {
		result := interleaved.degrade(
			rt, attributionTestFailure(rt, 2),
		)
		tail = append(tail, result.FailureSource)
	}
	require.Equal(t, sequence(params, 7)[100:], tail)
}

// TestSimAttributionRates asserts the realized degradation matches the
// configured probabilities, including the conditioning: a shift is only drawn
// when the unknown draw did not fire.
func TestSimAttributionRates(t *testing.T) {
	t.Parallel()

	const (
		trials      = 20_000
		unknownProb = 0.3
		shiftProb   = 0.2
		tolerance   = 0.02
	)

	rt := attributionTestRoute(t, 4)

	attribution, err := newSimAttribution(&SimAttributionParams{
		UnknownProb: unknownProb,
		ShiftProb:   shiftProb,
	}, 11)
	require.NoError(t, err)

	for i := 0; i < trials; i++ {
		attribution.degrade(rt, attributionTestFailure(rt, 2))
	}

	stats := attribution.stats
	require.Equal(t, trials, stats.Attempts)
	require.InDelta(
		t, unknownProb, float64(stats.Unknown)/trials, tolerance,
	)
	require.InDelta(
		t, (1-unknownProb)*shiftProb,
		float64(stats.Shifted)/trials, tolerance,
	)
}

// TestSimAttributionDelay asserts that a delayed delivery ages the network
// only where there is time to age in: under a virtual clock with traffic it
// advances the clock and moves liquidity, and on a static tier it is a no-op
// that reports itself as one.
func TestSimAttributionDelay(t *testing.T) {
	t.Parallel()

	const slices = 3

	// Static tier: no clock, so nothing to delay.
	static, _ := attributionRunner(t, &SimAttributionParams{
		DelaySlices: slices,
	})
	runAttributionBatch(t, static)
	require.Positive(t, static.AttributionStats().Attempts)
	require.Zero(t, static.AttributionStats().Delayed)

	// Under a clock with traffic, every attempt ages the network by its
	// slices and the counter says so.
	timed, _ := attributionRunner(t, nil)
	timed.SetVirtualClock(&SimClockParams{
		PaymentGapSec: 60, AttemptSec: 10,
	})
	require.NoError(t, timed.SetBackgroundTraffic(&SimTrafficParams{
		PaymentsPerGap: 30,
		MinAmtMsat:     100_000,
		MaxAmtMsat:     50_000_000,
		Seed:           7,
	}))
	require.NoError(t, timed.SetAttribution(&SimAttributionParams{
		DelaySlices: slices,
	}, 7))

	start := timed.clk.Now()
	results := runAttributionBatch(t, timed)

	var attempts int
	for _, result := range results {
		attempts += len(result.Attempts)
	}
	require.Positive(t, attempts)

	stats := timed.AttributionStats()
	require.Equal(t, attempts, stats.Delayed)

	// The clock moved by the payment gaps, the attempts themselves, and
	// the delay slices on top.
	elapsed := timed.clk.Now().Sub(start).Seconds()
	require.GreaterOrEqual(t, elapsed, float64(slices*10*attempts))

	// The delay ran the background engine, which is what makes evidence
	// stale rather than merely late.
	sent, _ := timed.TrafficStats()
	require.Positive(t, sent)
}

// TestSimAttributionValidation rejects the configurations that would silently
// mean something other than what they say.
func TestSimAttributionValidation(t *testing.T) {
	t.Parallel()

	runner, _ := attributionRunner(t, nil)

	for _, params := range []SimAttributionParams{
		{UnknownProb: -0.1},
		{UnknownProb: 1.5},
		{ShiftProb: -1},
		{ShiftProb: 2},
		{DelaySlices: -1},
	} {
		require.Error(t, runner.SetAttribution(&params, 1))
	}
}
