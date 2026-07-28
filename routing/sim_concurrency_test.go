package routing

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSimConcurrencyParamsValidate asserts that the section rejects what the
// scheduler cannot honor, and that the deferred arrival process says so by
// name rather than quietly running the one that is implemented.
func TestSimConcurrencyParamsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params *SimConcurrencyParams
		errStr string
	}{
		{
			name:   "absent section",
			params: nil,
		},
		{
			name:   "sequential",
			params: &SimConcurrencyParams{MaxInFlight: 1},
		},
		{
			name: "window named",
			params: &SimConcurrencyParams{
				MaxInFlight:     4,
				Arrival:         "window",
				InterArrivalSec: 30,
			},
		},
		{
			name:   "zero window",
			params: &SimConcurrencyParams{MaxInFlight: 0},
			errStr: "max_in_flight must be positive",
		},
		{
			name:   "negative window",
			params: &SimConcurrencyParams{MaxInFlight: -1},
			errStr: "max_in_flight must be positive",
		},
		{
			name: "poisson is deferred by name",
			params: &SimConcurrencyParams{
				MaxInFlight: 4,
				Arrival:     "poisson",
			},
			errStr: "DEFERRED",
		},
		{
			name: "unknown arrival",
			params: &SimConcurrencyParams{
				MaxInFlight: 4,
				Arrival:     "batch",
			},
			errStr: "unknown arrival",
		},
		{
			name: "negative inter arrival",
			params: &SimConcurrencyParams{
				MaxInFlight:     2,
				InterArrivalSec: -1,
			},
			errStr: "must not be negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.params.validate()
			if test.errStr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, test.errStr)
		})
	}
}

// TestSimConcurrencyMaxInFlightDefault asserts that an absent section is the
// sequential batch, which is what every scenario file written before stage D
// says by omission.
func TestSimConcurrencyMaxInFlightDefault(t *testing.T) {
	t.Parallel()

	var absent *SimConcurrencyParams
	require.Equal(t, 1, absent.maxInFlight())

	present := &SimConcurrencyParams{MaxInFlight: 4}
	require.Equal(t, 4, present.maxInFlight())
}

// concurrencyRunner builds a runner over the given graph whose router factory
// is called once per payment, handing each payment its own router. That is the
// contract SimRouterFactory has always had and the one this stage keeps: a
// shared instance would need RequestRoute and ReportAttempt to carry a payment
// identifier, which would break every router ever evolved.
func concurrencyRunner(t *testing.T, g *SimGraph, source route.Vertex,
	factory SimRouterFactory) *SimRunner {

	t.Helper()

	runner, err := NewSimRunner(g, DefaultSimParams(), source, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	runner.SetRouterFactory(factory)

	return runner
}

// scriptedFactory hands each payment a fresh scripted router built from the
// per-payment route lists, in scenario order.
func scriptedFactory(t *testing.T,
	scripts [][]*route.Route) (SimRouterFactory, *[]*scriptedRouter) {

	t.Helper()

	built := make([]*scriptedRouter, 0, len(scripts))

	return func(_ SimNetworkView, _ route.Vertex,
		_ map[uint64]lnwire.MilliSatoshi,
		_ *SimPaymentSpec) (SimRouter, error) {

		require.Less(t, len(built), len(scripts),
			"factory called more times than there are scripts")

		router := &scriptedRouter{routes: scripts[len(built)]}
		built = append(built, router)

		return router, nil
	}, &built
}

// TestSimSchedulerSequentialTimeline pins the timeline the sequential batch
// has always produced, now that it comes out of the event loop: a payment
// starts one payment gap after the previous one resolved, each of its attempts
// takes one attempt step, and nothing ever overlaps.
func TestSimSchedulerSequentialTimeline(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(10_000_000)

	graph, nodes := atomicTestGraph(t)
	source, target := nodes[0], nodes[3]

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)

	// Two payments, the first taking two attempts and the second one. The
	// first route is deliberately repeated so the payment needs a second
	// attempt to deliver its full amount.
	factory, _ := scriptedFactory(t, [][]*route.Route{{rt, rt}, {rt}})

	var starts []time.Time
	runner := concurrencyRunner(t, graph, source,
		func(view SimNetworkView, src route.Vertex,
			balances map[uint64]lnwire.MilliSatoshi,
			spec *SimPaymentSpec) (SimRouter, error) {

			starts = append(starts, view.Now())

			return factory(view, src, balances, spec)
		},
	)

	const start = int64(1_800_000_000)
	runner.SetVirtualClock(&SimClockParams{
		StartUnix:     start,
		PaymentGapSec: 600,
		AttemptSec:    30,
	})

	results, err := runner.RunBatch([]SimScenario{
		{Target: target.String(), AmtMsat: uint64(2 * shard),
			MaxParts: 2},
		{Target: target.String(), AmtMsat: uint64(shard),
			MaxParts: 1},
	}, nil)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.True(t, results[0].Success)
	require.True(t, results[1].Success)

	epoch := time.Unix(start, 0)
	require.Len(t, starts, 2)

	// The first payment starts one gap after the epoch.
	require.Equal(t, 600*time.Second, starts[0].Sub(epoch))

	// Its two attempts take one step each, and the second payment starts a
	// gap after the first resolved.
	require.Equal(
		t, (2*30+600)*time.Second, starts[1].Sub(starts[0]),
	)

	// A sequential batch never overlaps, and the mean is taken over the
	// virtual time in which anything was live, so it reads exactly one.
	stats := runner.ConcurrencyStats()
	require.Equal(t, 1, stats.MaxConcurrent)
	require.InDelta(t, 1.0, stats.MeanConcurrent, 1e-9)
	require.Zero(t, stats.SelfContentionFailures)

	// The makespan runs from the scheduler starting to the last payment
	// resolving: two gaps plus three attempts.
	require.InDelta(t, 2*600+3*30, stats.MakespanSec, 1e-9)
}

// TestSimSchedulerTrafficIsPerInterval asserts that the background traffic a
// batch runs is a function of the virtual time that elapsed, not of the number
// of payments or attempts that elapsed it.
//
// The carry is what makes that true: the per-gap volume is pro-rated by
// duration and the fractional remainder is kept rather than rounded away, so
// however finely the scheduler slices a window, the window's total is the
// same. Slicing is exactly what a concurrent batch does differently, which is
// why this is the invariant the stage has to hold.
func TestSimSchedulerTrafficCarryIsSliceInvariant(t *testing.T) {
	t.Parallel()

	// owed replays the prorating path over a window divided into the given
	// number of equal slices, and reports the total it dispatched.
	owed := func(slices int) int {
		runner := &SimRunner{
			traffic: &simTraffic{
				params: SimTrafficParams{PaymentsPerGap: 8},
			},
			clockParams: SimClockParams{PaymentGapSec: 600},
		}

		var total int
		for i := 0; i < slices; i++ {
			total += runner.trafficPaymentsFor(600 / float64(slices))
		}

		return total
	}

	// One gap's worth, however the gap is cut up. The guarantee is within
	// one payment rather than exact: a remainder that should land on an
	// integer boundary can land a hair under it in floating point, which
	// pushes one payment into the next slice and no further.
	for _, slices := range []int{1, 2, 3, 7, 20, 600} {
		require.InDelta(t, 8, owed(slices), 1,
			"a window cut into %d slices moved a different "+
				"amount of liquidity", slices)
	}

	// A whole gap in one piece is exact, which is what makes the sequential
	// batch's churn per payment exactly payments_per_gap and what lets the
	// scheduler's admission reproduce it byte for byte.
	require.Equal(t, 8, owed(1))
}

// TestSimSchedulerTrafficTracksVirtualTime asserts that a concurrent batch
// clears in less virtual time than the sequential one and therefore elapses
// less of the exogenous process.
//
// That is the honest consequence of running the traffic per interval rather
// than per payment: concurrency compresses the clock, and the network churns
// for as long as the clock says and no longer. A concurrency tier is therefore
// a slightly quieter world than its sequential control, and a sweep that
// compares the two is reading both effects at once.
func TestSimSchedulerTrafficTracksVirtualTime(t *testing.T) {
	t.Parallel()

	run := func(inFlight int) (int, float64) {
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

		scenarios := make([]SimScenario, 0, 6)
		for i := 1; i <= 6; i++ {
			scenarios = append(scenarios, SimScenario{
				Target:    nodes[i*3].String(),
				AmtMsat:   50_000_000,
				MaxParts:  4,
				AtomicMpp: true,
			})
		}

		var params *SimConcurrencyParams
		if inFlight > 1 {
			params = &SimConcurrencyParams{MaxInFlight: inFlight}
		}

		_, err = runner.RunBatch(scenarios, params)
		require.NoError(t, err)

		sent, _ := runner.TrafficStats()

		return sent, runner.ConcurrencyStats().MakespanSec
	}

	sequential, seqSpan := run(1)
	concurrent, conSpan := run(3)

	require.Less(t, conSpan, seqSpan, "the window did not clear faster")
	require.Less(t, concurrent, sequential,
		"a shorter batch churned the network for just as long")
}

// TestSimSchedulerDeterminism asserts that the same batch run twice against
// the same seeds produces the same traces, which is the property every sealed
// tier depends on and the one real parallelism would have destroyed.
func TestSimSchedulerDeterminism(t *testing.T) {
	t.Parallel()

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

// TestSimSchedulerOverlapLadder asserts that raising the window actually makes
// the sender's payments overlap. This is the manipulation check: a concurrency
// tier whose payments never run at the same time is testing nothing, and the
// score would say nothing about that.
func TestSimSchedulerOverlapLadder(t *testing.T) {
	t.Parallel()

	run := func(inFlight int) SimConcurrencyStats {
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

		scenarios := make([]SimScenario, 0, 8)
		for i := 1; i <= 8; i++ {
			scenarios = append(scenarios, SimScenario{
				Target:    nodes[i*2].String(),
				AmtMsat:   200_000_000,
				MaxParts:  4,
				AtomicMpp: true,
			})
		}

		var params *SimConcurrencyParams
		if inFlight > 1 {
			params = &SimConcurrencyParams{MaxInFlight: inFlight}
		}

		_, err = runner.RunBatch(scenarios, params)
		require.NoError(t, err)

		return runner.ConcurrencyStats()
	}

	one := run(1)
	require.Equal(t, 1, one.MaxConcurrent)
	require.InDelta(t, 1.0, one.MeanConcurrent, 1e-9)

	for _, inFlight := range []int{2, 4} {
		stats := run(inFlight)
		require.Equal(t, inFlight, stats.MaxConcurrent,
			"window %d never filled", inFlight)
		require.Greater(t, stats.MeanConcurrent, 1.0,
			"window %d never overlapped", inFlight)
		require.Less(t, stats.MakespanSec, one.MakespanSec,
			"window %d did not clear faster", inFlight)
	}
}

// TestSimSchedulerNeedsAClock asserts that a concurrency section on a file
// with no virtual time is refused rather than measured. With no clock every
// event ties at the same instant, the loop degenerates to running the payments
// in index order, and the scheduling counters would report a window that never
// opened.
func TestSimSchedulerNeedsAClock(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, target := nodes[0], nodes[3]

	factory, _ := scriptedFactory(t, [][]*route.Route{})
	runner := concurrencyRunner(t, graph, source, factory)

	_, err := runner.RunBatch([]SimScenario{
		{Target: target.String(), AmtMsat: 1_000_000},
	}, &SimConcurrencyParams{MaxInFlight: 2})
	require.ErrorContains(t, err, "needs a clock section")

	// The sequential batch is unaffected: it is what every scenario file
	// with no clock has always run.
	_, err = runner.RunBatch([]SimScenario{}, nil)
	require.NoError(t, err)
}

// refreshRouter is a scripted router that also takes balance refreshes, and
// records every map it was handed.
type refreshRouter struct {
	scriptedRouter

	refreshed []map[uint64]lnwire.MilliSatoshi
}

// RefreshLocalBalances records the refreshed view of the sender's own
// liquidity.
//
// NOTE: Part of the SimBalanceRefresher interface.
func (r *refreshRouter) RefreshLocalBalances(
	balances map[uint64]lnwire.MilliSatoshi) {

	r.refreshed = append(r.refreshed, balances)
}

// TestSimSchedulerSelfContention asserts that an attempt that fails because
// ANOTHER of the sender's own payments is holding the liquidity is counted,
// and that the same attempt against the same shortfall with no sibling holding
// anything is not.
//
// This is the number the whole stage exists to produce. Under atomic mpp a
// shard that reaches the destination reserves every hop it crossed, so a
// second payment finds the sender's own first channel short of liquidity that
// nothing in its gossip view can explain.
func TestSimSchedulerSelfContention(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	// run sends two payments over the sender's channel 1, which funds one
	// and a half shards. The first payment is an mpp that puts one shard
	// over channel 1 and the rest over channel 3, so it is still holding
	// channel 1 when the second payment tries to use it.
	run := func(inFlight int, atomic bool) SimConcurrencyStats {
		graph, nodes := atomicTestGraph(t)
		source, target := nodes[0], nodes[3]

		atomicSetBalance(t, graph, 1, source, shard+shard/2)

		viaA := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)
		viaB := atomicTestRoute(t, graph, source, []uint64{3, 4}, shard)
		factory, _ := scriptedFactory(t, [][]*route.Route{
			{viaA, viaB}, {viaA},
		})

		runner := concurrencyRunner(t, graph, source, factory)
		runner.SetVirtualClock(&SimClockParams{
			StartUnix:     1_800_000_000,
			PaymentGapSec: 600,
			AttemptSec:    30,
		})

		var params *SimConcurrencyParams
		if inFlight > 1 {
			params = &SimConcurrencyParams{MaxInFlight: inFlight}
		}

		_, err := runner.RunBatch([]SimScenario{
			{Target: target.String(), AmtMsat: uint64(2 * shard),
				MaxParts: 2, AtomicMpp: atomic},
			{Target: target.String(), AmtMsat: uint64(shard),
				MaxParts: 1, AtomicMpp: atomic},
		}, params)
		require.NoError(t, err)
		requireNoHolds(t, graph)

		return runner.ConcurrencyStats()
	}

	// Sequential: the first payment has settled and moved the liquidity
	// before the second one is even built, so the second one's shortfall is
	// a fact about the network rather than about its sibling.
	sequential := run(1, true)
	require.Equal(t, 1, sequential.MaxConcurrent)
	require.Zero(t, sequential.SelfContentionFailures)

	// Concurrent and atomic: the first payment's shard is still held on
	// channel 1, and it is the reason the second one fails there.
	contended := run(2, true)
	require.Equal(t, 2, contended.MaxConcurrent)
	require.Equal(t, 1, contended.SelfContentionFailures)

	// Concurrent without holds is the free control. A shard that settles
	// the instant it arrives reserves nothing, so the counter is
	// structurally zero however heavily the payments overlap.
	noHolds := run(2, false)
	require.Equal(t, 2, noHolds.MaxConcurrent)
	require.Zero(t, noHolds.SelfContentionFailures)
}

// TestRouterAcceptsBalanceRefreshFalseForPlainRouter asserts that a router
// without the optional refresh half is reported as such, so a sweep can tell
// an ineffective refresh from an undelivered one.
func TestRouterAcceptsBalanceRefreshFalseForPlainRouter(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, target := nodes[0], nodes[3]

	rt := atomicTestRoute(
		t, graph, source, []uint64{1, 2}, lnwire.MilliSatoshi(10_000),
	)
	factory, _ := scriptedFactory(t, [][]*route.Route{{rt}})

	runner := concurrencyRunner(t, graph, source, factory)
	require.False(t, runner.RouterAcceptsBalanceRefresh())

	_, err := runner.RunBatch([]SimScenario{
		{Target: target.String(), AmtMsat: 10_000, MaxParts: 1},
	}, nil)
	require.NoError(t, err)

	require.False(t, runner.RouterAcceptsBalanceRefresh())
	require.False(
		t, runner.ConcurrencyStats().RouterAcceptsBalanceRefresh,
	)
}

// TestSimSchedulerBalanceRefreshIsDelivered asserts that the optional half is
// not dead plumbing: a router that implements it is told, before every route
// request, what its own outbound liquidity is now, and the number it is told is
// net of what is currently held.
//
// The single-payment case is the exact one, and it is the same staleness the
// concurrent case has: what a router was handed at construction stops being
// true the moment any shard reserves liquidity, whether that shard is its own
// or a sibling payment's.
func TestSimSchedulerBalanceRefreshIsDelivered(t *testing.T) {
	t.Parallel()

	const shard = lnwire.MilliSatoshi(100_000_000)

	graph, nodes := atomicTestGraph(t)
	source, target := nodes[0], nodes[3]

	atomicSetBalance(t, graph, 1, source, 3*shard)

	viaA := atomicTestRoute(t, graph, source, []uint64{1, 2}, shard)
	viaB := atomicTestRoute(t, graph, source, []uint64{3, 4}, shard)

	built := make([]*refreshRouter, 0, 1)
	runner := concurrencyRunner(t, graph, source,
		func(_ SimNetworkView, _ route.Vertex,
			_ map[uint64]lnwire.MilliSatoshi,
			_ *SimPaymentSpec) (SimRouter, error) {

			router := &refreshRouter{
				scriptedRouter: scriptedRouter{
					routes: []*route.Route{viaA, viaB},
				},
			}
			built = append(built, router)

			return router, nil
		},
	)
	runner.SetVirtualClock(&SimClockParams{
		StartUnix:     1_800_000_000,
		PaymentGapSec: 600,
		AttemptSec:    30,
	})

	require.False(t, runner.RouterAcceptsBalanceRefresh(),
		"the capability is latched at the first router, not before")

	results, err := runner.RunBatch([]SimScenario{{
		Target:    target.String(),
		AmtMsat:   uint64(2 * shard),
		MaxParts:  2,
		AtomicMpp: true,
	}}, nil)
	require.NoError(t, err)
	require.True(t, results[0].Success)

	require.True(t, runner.RouterAcceptsBalanceRefresh())
	require.True(
		t, runner.ConcurrencyStats().RouterAcceptsBalanceRefresh,
	)

	require.Len(t, built, 1)
	require.Len(t, built[0].refreshed, 2,
		"the router was not told once per route request")

	// The first request saw the whole balance of channel 1; the second was
	// made while the first shard was held on it, so what it was told is
	// short by exactly what that shard reserved.
	require.Equal(t, 3*shard, built[0].refreshed[0][1])
	require.Equal(
		t, 3*shard-viaA.TotalAmount, built[0].refreshed[1][1],
	)
}
