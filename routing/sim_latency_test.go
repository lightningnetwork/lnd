package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSimLatencyValidate pins what a latency section may say. The refusals
// matter as much as the acceptances: a section that charges nothing is not the
// flat tick, and hold_carry false would be a latency knob editing the traffic
// engine.
func TestSimLatencyValidate(t *testing.T) {
	t.Parallel()

	yes, no := true, false

	tests := []struct {
		name   string
		params *SimLatencyParams
		err    string
	}{
		{
			name:   "absent",
			params: nil,
		},
		{
			name: "both set",
			params: &SimLatencyParams{
				PerHopMs:          300,
				AttemptOverheadMs: 250,
			},
		},
		{
			name:   "per hop alone",
			params: &SimLatencyParams{PerHopMs: 300},
		},
		{
			name:   "overhead alone",
			params: &SimLatencyParams{AttemptOverheadMs: 250},
		},
		{
			name: "hold carry true",
			params: &SimLatencyParams{
				PerHopMs:  300,
				HoldCarry: &yes,
			},
		},
		{
			name:   "negative per hop",
			params: &SimLatencyParams{PerHopMs: -1},
			err:    "per_hop_ms must not be negative",
		},
		{
			name:   "negative overhead",
			params: &SimLatencyParams{AttemptOverheadMs: -1},
			err:    "attempt_overhead_ms must not be negative",
		},
		{
			name:   "free attempts",
			params: &SimLatencyParams{},
			err:    "omit the section",
		},
		{
			name: "hold carry false",
			params: &SimLatencyParams{
				PerHopMs:  300,
				HoldCarry: &no,
			},
			err: "hold_carry false is REFUSED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.params.validate()
			if test.err == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.err)
		})
	}
}

// TestSimLatencyReturnTrip pins the arithmetic: a hop is charged twice, out and
// back, and a route nobody crossed is charged nothing.
func TestSimLatencyReturnTrip(t *testing.T) {
	t.Parallel()

	latency, err := newSimLatency(&SimLatencyParams{
		PerHopMs:          300,
		AttemptOverheadMs: 250,
	})
	require.NoError(t, err)

	require.Equal(t, 250*time.Millisecond, latency.overhead)
	require.Equal(t, time.Duration(0), latency.returnTrip(0))
	require.Equal(t, 600*time.Millisecond, latency.returnTrip(1))
	require.Equal(t, 4800*time.Millisecond, latency.returnTrip(8))
}

// TestSimLatencyHops pins the differential structure, which is the whole reason
// this stage is not obviously exp-019's null: a failure is charged for the hops
// the htlc actually crossed, and a settle for the whole route.
func TestSimLatencyHops(t *testing.T) {
	t.Parallel()

	rt := attributionTestRoute(t, 8)

	// A settle traversed the whole route.
	require.Equal(t, 8, simLatencyHops(rt, SimHtlcResult{}))

	// The sender's own first hop costs one round trip, not zero: the spec's
	// own reading, and the one that keeps a first-hop probe from being free
	// in time.
	require.Equal(t, 1, simLatencyHops(rt, attributionTestFailure(rt, 0)))

	// A failure reported by the node at index i is a failure of hop i+1.
	require.Equal(t, 2, simLatencyHops(rt, attributionTestFailure(rt, 1)))
	require.Equal(t, 8, simLatencyHops(rt, attributionTestFailure(rt, 7)))

	// The final node has no further hop to charge for, so the route length
	// is the cap.
	require.Equal(t, 8, simLatencyHops(rt, attributionTestFailure(rt, 8)))

	// A failure from a node that is not on the route at all is charged the
	// whole route rather than credited with stopping early.
	require.Equal(t, 8, simLatencyHops(rt, SimHtlcResult{
		FailureSource: SimNodePubKey(999),
		Failure:       &lnwire.FailTemporaryChannelFailure{},
	}))
}

// latencyRouter is a scripted router that stamps the virtual clock every time
// it is asked for a route, so a test can read the interval an attempt took
// straight off the sender's own view of time.
type latencyRouter struct {
	inner *scriptedRouter
	now   func() time.Time
	asked *[]time.Time
}

// RequestRoute records when the sender was ready to send again.
//
// NOTE: Part of the SimRouter interface.
func (l *latencyRouter) RequestRoute(amt lnwire.MilliSatoshi,
	inFlightHtlcs uint32) (*route.Route, error) {

	*l.asked = append(*l.asked, l.now())

	return l.inner.RequestRoute(amt, inFlightHtlcs)
}

// ReportAttempt passes the outcome through.
//
// NOTE: Part of the SimRouter interface.
func (l *latencyRouter) ReportAttempt(attemptID uint64, rt *route.Route,
	result SimHtlcResult) error {

	return l.inner.ReportAttempt(attemptID, rt, result)
}

// latencyBatch runs a scripted batch on the four node atomic test graph with
// the given latency section, and hands back the results, every instant the
// router was asked for a route, and what the batch reported about its own
// timing.
func latencyBatch(t *testing.T, params *SimLatencyParams,
	build func(*SimGraph, [4]route.Vertex) ([]SimScenario,
		[][]*route.Route)) ([]*SimScenarioResult, []time.Time,
	SimLatencyStats) {

	t.Helper()

	graph, nodes := atomicTestGraph(t)
	scenarios, scripts := build(graph, nodes)

	var (
		asked []time.Time
		built int
	)
	runner := concurrencyRunner(t, graph, nodes[0],
		func(view SimNetworkView, _ route.Vertex,
			_ map[uint64]lnwire.MilliSatoshi,
			_ *SimPaymentSpec) (SimRouter, error) {

			require.Less(t, built, len(scripts))
			router := &latencyRouter{
				inner: &scriptedRouter{routes: scripts[built]},
				now:   view.Now,
				asked: &asked,
			}
			built++

			return router, nil
		},
	)

	runner.SetVirtualClock(&SimClockParams{
		StartUnix:     1_800_000_000,
		PaymentGapSec: 600,
		AttemptSec:    30,
	})

	if params != nil {
		require.NoError(t, runner.SetLatency(params))
	}

	results, err := runner.RunBatch(scenarios, nil)
	require.NoError(t, err)

	return results, asked, runner.LatencyStats()
}

// TestSimLatencyNeedsAClock asserts that a latency section on a scenario with
// no virtual time is refused rather than silently measured nowhere, which is
// the rule stage D arrived at for concurrency and for the same reason.
func TestSimLatencyNeedsAClock(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	runner, err := NewSimRunner(
		graph, DefaultSimParams(), nodes[0], t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	err = runner.SetLatency(&SimLatencyParams{PerHopMs: 300})
	require.ErrorContains(t, err, "needs a clock section")

	// With a clock it is accepted, so the refusal is about virtual time and
	// not about the section.
	runner.SetVirtualClock(&SimClockParams{
		PaymentGapSec: 600, AttemptSec: 30,
	})
	require.NoError(t, runner.SetLatency(&SimLatencyParams{PerHopMs: 300}))
}

// TestSimLatencyAbsentGolden pins the flat tick. With no latency section an
// attempt takes exactly the clock's attempt_sec however long its route was,
// and not one key of timing is emitted anywhere: this is the shape every
// scenario file written before stage E produces and has to keep producing.
func TestSimLatencyAbsentGolden(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(10_000_000)

	results, asked, stats := latencyBatch(t, nil,
		func(g *SimGraph, nodes [4]route.Vertex) ([]SimScenario,
			[][]*route.Route) {

			source, nodeA, target := nodes[0], nodes[1], nodes[3]

			// One hop, then two, and both settle.
			short := atomicTestRoute(t, g, source, []uint64{1}, shard)
			long := atomicTestRoute(
				t, g, source, []uint64{3, 4}, shard,
			)

			return []SimScenario{
					{Target: nodeA.String(),
						AmtMsat:  uint64(shard),
						MaxParts: 1},
					{Target: target.String(),
						AmtMsat:  uint64(shard),
						MaxParts: 1},
				}, [][]*route.Route{
					{short}, {long},
				}
		},
	)

	require.Len(t, results, 2)
	for _, result := range results {
		require.True(t, result.Success)
		require.Nil(t, result.LatencySec)

		require.Len(t, result.Attempts, 1)
		require.Nil(t, result.Attempts[0].LatencySec)
	}

	// One hop and two hops cost the same flat tick, so the second payment
	// starts one attempt plus one gap after the first one did.
	require.Len(t, asked, 2)
	require.Equal(t, (30+600)*time.Second, asked[1].Sub(asked[0]))

	// The stats are computed either way; only the reporting is gated.
	require.InDelta(t, 30, stats.MeanAttemptLatencySec, 1e-9)
}

// TestSimLatencyLongerRoutesResolveLater is the mechanism: an attempt costs its
// overhead plus a round trip to the hop that resolved it, so a two hop route
// takes longer than a one hop route and the sender waits through the
// difference.
func TestSimLatencyLongerRoutesResolveLater(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(10_000_000)

	params := &SimLatencyParams{PerHopMs: 300, AttemptOverheadMs: 250}

	results, asked, stats := latencyBatch(t, params,
		func(g *SimGraph, nodes [4]route.Vertex) ([]SimScenario,
			[][]*route.Route) {

			source, nodeA, target := nodes[0], nodes[1], nodes[3]

			short := atomicTestRoute(t, g, source, []uint64{1}, shard)
			long := atomicTestRoute(
				t, g, source, []uint64{3, 4}, shard,
			)

			return []SimScenario{
					{Target: nodeA.String(),
						AmtMsat:  uint64(shard),
						MaxParts: 1},
					{Target: target.String(),
						AmtMsat:  uint64(shard),
						MaxParts: 1},
				}, [][]*route.Route{
					{short}, {long},
				}
		},
	)

	require.Len(t, results, 2)

	// 0.25 + 2 * 0.3 * 1 hop, and 0.25 + 2 * 0.3 * 2 hops.
	require.True(t, results[0].Success)
	require.InDelta(t, 0.85, *results[0].Attempts[0].LatencySec, 1e-9)
	require.InDelta(t, 0.85, *results[0].LatencySec, 1e-9)

	require.True(t, results[1].Success)
	require.InDelta(t, 1.45, *results[1].Attempts[0].LatencySec, 1e-9)
	require.InDelta(t, 1.45, *results[1].LatencySec, 1e-9)

	// The clock the sender reads moved by the short payment's own attempt
	// and the payment gap, and by nothing else.
	require.Len(t, asked, 2)
	require.Equal(
		t, 850*time.Millisecond+600*time.Second,
		asked[1].Sub(asked[0]),
	)

	require.InDelta(t, 1.15, stats.MeanAttemptLatencySec, 1e-9)
	require.InDelta(t, 1.15, stats.MeanPaymentLatencySec, 1e-9)
}

// TestSimLatencyFailureCostsTheHopsCrossed is the asymmetry the stage exists
// for. A failure at the sender's own first hop comes back in one round trip
// while a settle over the same length of route pays for the whole thing, so
// probing near really is cheaper in time than probing far.
func TestSimLatencyFailureCostsTheHopsCrossed(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(10_000_000)

	params := &SimLatencyParams{PerHopMs: 300, AttemptOverheadMs: 250}

	results, asked, _ := latencyBatch(t, params,
		func(g *SimGraph, nodes [4]route.Vertex) ([]SimScenario,
			[][]*route.Route) {

			source, target := nodes[0], nodes[3]

			// The sender's own end of channel 1 carries nothing, so
			// the first route fails at hop one and never leaves.
			atomicSetBalance(t, g, 1, source, 0)

			dead := atomicTestRoute(
				t, g, source, []uint64{1, 2}, shard,
			)
			live := atomicTestRoute(
				t, g, source, []uint64{3, 4}, shard,
			)

			return []SimScenario{{
					Target:   target.String(),
					AmtMsat:  uint64(shard),
					MaxParts: 1,
				}}, [][]*route.Route{
					{dead, live},
				}
		},
	)

	require.Len(t, results, 1)
	require.True(t, results[0].Success)
	require.Len(t, results[0].Attempts, 2)

	// The failed attempt crossed one hop of a two hop route and is charged
	// for one, not two.
	require.False(t, results[0].Attempts[0].Success)
	require.InDelta(t, 0.85, *results[0].Attempts[0].LatencySec, 1e-9)

	require.True(t, results[0].Attempts[1].Success)
	require.InDelta(t, 1.45, *results[0].Attempts[1].LatencySec, 1e-9)

	// The sender was ready to try again a first-hop round trip later, which
	// is the whole claim: it learned sooner because it probed nearer.
	require.Len(t, asked, 2)
	require.Equal(t, 850*time.Millisecond, asked[1].Sub(asked[0]))

	require.InDelta(t, 2.3, *results[0].LatencySec, 1e-9)
}

// latencyTrafficBatch runs the same atomic batch over the same seeded network
// with and without a latency section, and reports the background payments the
// run elapsed and the virtual time it took.
func latencyTrafficBatch(t *testing.T,
	params *SimLatencyParams) (int, float64) {

	t.Helper()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), nodes[0], t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	runner.SetVirtualClock(&SimClockParams{
		StartUnix:     1_800_000_000,
		PaymentGapSec: 600,
		AttemptSec:    30,
	})
	require.NoError(t, runner.SetBackgroundTraffic(&SimTrafficParams{
		PaymentsPerGap: 20,
		MinAmtMsat:     1_000,
		MaxAmtMsat:     1_000_000,
		Seed:           11,
	}))

	if params != nil {
		require.NoError(t, runner.SetLatency(params))
	}

	scenarios := make([]SimScenario, 0, 6)
	for i := 1; i <= 6; i++ {
		scenarios = append(scenarios, SimScenario{
			Target:    nodes[i*3].String(),
			AmtMsat:   50_000_000,
			MaxParts:  4,
			AtomicMpp: true,
		})
	}

	_, err = runner.RunBatch(scenarios, nil)
	require.NoError(t, err)

	sent, _ := runner.TrafficStats()

	return sent, runner.ConcurrencyStats().MakespanSec
}

// TestSimLatencySlowAttemptsChurnMore is the indirect cost channel, and it is
// the one exp-019's uniform delay could reach too. Time on the clock is time
// the rest of the network keeps moving in, so an attempt that takes longer
// leaves the sender's knowledge staler when the answer finally arrives.
//
// What the delay knob could NOT do is make that cost depend on the route,
// which is what the direct tests above pin. This one only asserts the channel
// is live: a batch whose attempts are slower elapses more of the exogenous
// process, through the same prorating every other advance uses.
func TestSimLatencySlowAttemptsChurnMore(t *testing.T) {
	t.Parallel()

	flat, flatSpan := latencyTrafficBatch(t, nil)

	// Twenty seconds a hop, each way, against a flat tick of thirty for the
	// whole attempt: a multi-hop attempt now takes minutes.
	slow, slowSpan := latencyTrafficBatch(t, &SimLatencyParams{
		PerHopMs:          20_000,
		AttemptOverheadMs: 1_000,
	})

	require.Greater(t, slowSpan, flatSpan,
		"slower attempts did not take longer")
	require.Greater(t, slow, flat,
		"a longer batch did not churn the network for longer")
}

// TestSimLatencyDeterminism asserts that latency keeps the property every
// sealed tier depends on: the same batch run twice produces the same traces.
// The round trip is charged off the true result, so it moves the clock by the
// same amount on both runs and every downstream draw stays in order.
func TestSimLatencyDeterminism(t *testing.T) {
	t.Parallel()

	params := &SimLatencyParams{PerHopMs: 300, AttemptOverheadMs: 250}

	run := func() []*SimScenarioResult {
		graph := trafficTestGraph(t, 3)
		nodes := sortedNodes(graph)

		runner, err := NewSimRunner(
			graph, DefaultSimParams(), nodes[0], t.TempDir(),
		)
		require.NoError(t, err)
		t.Cleanup(runner.Close)

		runner.SetVirtualClock(&SimClockParams{
			StartUnix:     1_800_000_000,
			PaymentGapSec: 600,
			AttemptSec:    30,
		})
		require.NoError(t, runner.SetBackgroundTraffic(
			&SimTrafficParams{
				PaymentsPerGap: 20,
				MinAmtMsat:     1_000,
				MaxAmtMsat:     1_000_000,
				Seed:           11,
			},
		))
		require.NoError(t, runner.SetLatency(params))

		scenarios := make([]SimScenario, 0, 6)
		for i := 1; i <= 6; i++ {
			scenarios = append(scenarios, SimScenario{
				Target:    nodes[i*3].String(),
				AmtMsat:   50_000_000,
				MaxParts:  4,
				AtomicMpp: true,
			})
		}

		results, err := runner.RunBatch(scenarios,
			&SimConcurrencyParams{MaxInFlight: 3})
		require.NoError(t, err)

		return results
	}

	require.Equal(t, run(), run())
}
