package routing

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// warmupChanCapSat is the capacity of every channel in the warmup fixture,
// far larger than the amounts the tests send so that a successful payment
// never exhausts the one working corridor.
const warmupChanCapSat btcutil.Amount = 1_000_000

// warmupNumRelays is how many parallel relays sit between the source and the
// target of the warmup fixture.
const warmupNumRelays = 4

// warmupTestGraph builds the fixture the warmup tests route over: a source
// with several parallel two-hop corridors to a single target, all of them
// drained on the far side except the last.
//
//	S ── 11 ── A1 ── 21 ──┐   (A1 → T drained)
//	├──── 12 ── A2 ── 22 ──┤   (A2 → T drained)
//	├──── 13 ── A3 ── 23 ──┤   (A3 → T drained)
//	└──── 14 ── A4 ── 24 ──┴── T
//
// The relay fees rise with the relay index, so a sender that knows nothing
// walks the corridors cheapest first and only reaches the working one after
// failing on every drained corridor before it. That makes the cost of
// ignorance a fixed, countable number of attempts.
func warmupTestGraph(t *testing.T) (*SimGraph, route.Vertex, route.Vertex) {
	t.Helper()

	graph := NewSimGraph()

	source := SimNodePubKey(1)
	_, err := graph.AddNode(source, "source")
	require.NoError(t, err)

	target := SimNodePubKey(100)
	_, err = graph.AddNode(target, "target")
	require.NoError(t, err)

	// The first hop of every corridor is identical, so the choice between
	// corridors is decided entirely by the relay's forwarding fee.
	firstHop := SimPolicy{
		BaseFeeMsat:   1_000,
		FeeRatePPM:    100,
		TimeLockDelta: 40,
	}

	for i := 1; i <= warmupNumRelays; i++ {
		relay := SimNodePubKey(uint32(i + 1))
		_, err := graph.AddNode(relay, fmt.Sprintf("relay%d", i))
		require.NoError(t, err)

		relayHop := SimPolicy{
			BaseFeeMsat:   1_000,
			FeeRatePPM:    lnwire.MilliSatoshi(1_000 * i),
			TimeLockDelta: 40,
		}

		require.NoError(t, graph.AddChannel(
			uint64(10+i), source, relay, warmupChanCapSat,
			firstHop, firstHop,
		))
		require.NoError(t, graph.AddChannel(
			uint64(20+i), relay, target, warmupChanCapSat,
			relayHop, relayHop,
		))

		// Every corridor but the last is drained on the relay's side,
		// so it looks perfectly routable in gossip and fails the
		// moment an htlc arrives.
		if i < warmupNumRelays {
			atomicSetBalance(t, graph, uint64(20+i), relay, 0)
		}
	}

	return graph, source, target
}

// warmupAddVantage attaches a second sender to every relay of the warmup
// fixture, a node that reaches the target over the very same corridors the
// source does. It stands in for whoever gathered the served weight cache:
// a different vantage onto identical channels.
func warmupAddVantage(t *testing.T, graph *SimGraph) route.Vertex {
	t.Helper()

	vantage := SimNodePubKey(200)
	_, err := graph.AddNode(vantage, "vantage")
	require.NoError(t, err)

	policy := SimPolicy{
		BaseFeeMsat:   1_000,
		FeeRatePPM:    100,
		TimeLockDelta: 40,
	}

	for i := 1; i <= warmupNumRelays; i++ {
		require.NoError(t, graph.AddChannel(
			uint64(30+i), vantage, SimNodePubKey(uint32(i+1)),
			warmupChanCapSat, policy, policy,
		))
	}

	return vantage
}

// warmupFirstHopChan returns the channel the first attempt of a result left
// over, which identifies the node that actually sent the payment.
func warmupFirstHopChan(t *testing.T, result *SimScenarioResult) uint64 {
	t.Helper()

	require.NotEmpty(t, result.Attempts)
	require.NotEmpty(t, result.Attempts[0].Hops)

	return result.Attempts[0].Hops[0].ChanID
}

// warmupTestRunner builds a runner over a fresh copy of the warmup fixture
// with the stock lnd routing stack.
func warmupTestRunner(t *testing.T) (*SimRunner, *SimGraph, route.Vertex) {
	t.Helper()

	graph, source, target := warmupTestGraph(t)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	return runner, graph, target
}

// warmupPay sends one payment to the target and returns the number of htlc
// attempts it took.
func warmupPay(t *testing.T, runner *SimRunner, target route.Vertex) int {
	t.Helper()

	result, err := runner.RunScenario(&SimScenario{
		Target:   target.String(),
		AmtMsat:  1_000_000,
		MaxParts: 1,
	})
	require.NoError(t, err)
	require.True(t, result.Success, "payment failed: %v", result.Error)

	return len(result.Attempts)
}

// TestSimWarmupReducesAttempts asserts that unscored warmup payments really do
// warm the scored batch: the knowledge the first payment buys by probing the
// drained corridors is knowledge the scored payments no longer have to buy.
// This is the whole premise of exp-012's hot-load arm, so it is asserted on
// the lnd stack, whose mission control state is the thing being warmed.
func TestSimWarmupReducesAttempts(t *testing.T) {
	t.Parallel()

	// The scored batch is the same three payments in both runs.
	scoredAttempts := func(runner *SimRunner, target route.Vertex) int {
		var attempts int
		for i := 0; i < 3; i++ {
			attempts += warmupPay(t, runner, target)
		}

		return attempts
	}

	coldRunner, _, coldTarget := warmupTestRunner(t)
	cold := scoredAttempts(coldRunner, coldTarget)

	warmRunner, _, warmTarget := warmupTestRunner(t)
	warmupCost := warmupPay(t, warmRunner, warmTarget)
	warm := scoredAttempts(warmRunner, warmTarget)

	// The warmup payment pays the full price of ignorance: one failed
	// attempt per drained corridor plus the one that works.
	require.Equal(t, warmupNumRelays, warmupCost)

	// A cold batch pays that same price on its first payment, a warmed one
	// pays nothing and spends a single attempt per payment.
	require.Equal(t, 3, warm)
	require.Less(t, warm, cold)

	// Mission control is where the knowledge lives, so it must be
	// non-empty by the time the scored batch starts.
	require.NotEmpty(
		t, warmRunner.mc.GetHistorySnapshot().Pairs,
		"warmup left no mission control history",
	)
}

// TestSimWarmupForeignVantage asserts that a warmup phase can be sent by a
// node other than the one that runs the scored batch, that what it learns
// lands in the shared mission control, and that the scored batch still leaves
// from the file-level source. This is exp-012's third-party arm: the probes
// belong to somebody else, and only knowledge that describes channels rather
// than the observer is of any use to us.
func TestSimWarmupForeignVantage(t *testing.T) {
	t.Parallel()

	graph, source, target := warmupTestGraph(t)
	vantage := warmupAddVantage(t, graph)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	defer runner.Close()

	scenario := SimScenario{
		Target:   target.String(),
		AmtMsat:  1_000_000,
		MaxParts: 1,
	}

	// The foreign node pays the price of ignorance itself, probing the
	// drained corridors from its own channels.
	warmup, err := runner.RunScenarioFrom(vantage, &scenario)
	require.NoError(t, err)
	require.True(t, warmup.Success, "warmup failed: %v", warmup.Error)
	require.Greater(t, len(warmup.Attempts), 1, "warmup never probed")
	require.Contains(
		t, graph.LocalBalances(vantage),
		warmupFirstHopChan(t, warmup),
		"warmup did not leave from the foreign vantage",
	)

	require.NotEmpty(
		t, runner.mc.GetHistorySnapshot().Pairs,
		"foreign warmup left no mission control history",
	)

	// The scored payment is still ours: it leaves over one of the source's
	// own channels, not the vantage's.
	scored, err := runner.RunScenario(&scenario)
	require.NoError(t, err)
	require.True(t, scored.Success, "payment failed: %v", scored.Error)
	require.Contains(
		t, graph.LocalBalances(source),
		warmupFirstHopChan(t, scored),
		"scored payment did not leave from the file-level source",
	)

	// The knowledge did cross the vantage boundary here, and it is worth
	// noting exactly which knowledge: the drained corridors failed at the
	// relays, and a pair failure between two nodes that are neither the
	// observer nor the payer is a fact about the network rather than about
	// whoever saw it. A cold sender pays for the same discovery itself.
	coldRunner, _, coldTarget := warmupTestRunner(t)
	require.Equal(t, warmupNumRelays, warmupPay(t, coldRunner, coldTarget))
	require.Len(t, scored.Attempts, 1)

	// A sender that is not part of the graph at all is an error, not a
	// silently mis-attributed payment.
	_, err = runner.RunScenarioFrom(SimNodePubKey(9_999), &scenario)
	require.ErrorContains(t, err, "not in graph")
}

// TestSimWarmupMovesLiquidity asserts that warmup payments are real payments:
// they move hidden balances, so the scored batch starts from a network the
// warmup actually perturbed rather than from a pristine one.
func TestSimWarmupMovesLiquidity(t *testing.T) {
	t.Parallel()

	runner, graph, target := warmupTestRunner(t)

	before := balanceSnapshot(graph)
	warmupPay(t, runner, target)
	after := balanceSnapshot(graph)

	require.NotEqual(t, before, after, "warmup moved no liquidity")

	// Conservation still holds per channel: an htlc shifts balance across
	// a channel, it never creates or destroys any.
	for id, endsBefore := range before {
		endsAfter := after[id]
		require.Equal(
			t, endsBefore[0]+endsBefore[1],
			endsAfter[0]+endsAfter[1],
			"channel %d total changed", id,
		)
	}
}

// TestSimAdvanceIdle asserts that an idle stretch of virtual time advances the
// clock and runs exactly the background traffic that belongs to it, which is
// what makes warmed knowledge go stale.
func TestSimAdvanceIdle(t *testing.T) {
	t.Parallel()

	const (
		gapSec         = 600.0
		paymentsPerGap = 10
	)

	trafficParams := &SimTrafficParams{
		PaymentsPerGap: paymentsPerGap,
		MinAmtMsat:     100_000,
		MaxAmtMsat:     20_000_000,
		Seed:           7,
	}

	newRunner := func(t *testing.T) *SimRunner {
		graph := trafficTestGraph(t, 3)

		source, err := graph.ResolveNode("1")
		require.NoError(t, err)

		runner, err := NewSimRunner(
			graph, DefaultSimParams(), source, t.TempDir(),
		)
		require.NoError(t, err)
		t.Cleanup(runner.Close)

		runner.SetVirtualClock(&SimClockParams{
			StartUnix:     1_800_000_000,
			PaymentGapSec: gapSec,
			AttemptSec:    1,
		})
		require.NoError(t, runner.SetBackgroundTraffic(trafficParams))

		return runner
	}

	// One gap's worth of idle time runs one gap's worth of traffic, which
	// is exactly what the traffic engine does on its own with the same
	// seed.
	reference, err := newSimTraffic(
		trafficTestGraph(t, 3), trafficParams,
	)
	require.NoError(t, err)
	reference.run()

	runner := newRunner(t)
	start := runner.clk.Now()

	runner.AdvanceIdle(gapSec)

	require.Equal(
		t, start.Add(gapSec*time.Second), runner.clk.Now(),
		"idle time did not advance the clock",
	)

	sent, settled := runner.TrafficStats()
	require.Equal(t, reference.Sent, sent)
	require.Equal(t, reference.Settled, settled)
	require.Positive(t, sent)

	// A zero-length gap is a no-op: no time passes and no traffic runs.
	idle := newRunner(t)
	idleStart := idle.clk.Now()

	idle.AdvanceIdle(0)

	require.Equal(t, idleStart, idle.clk.Now())

	sent, _ = idle.TrafficStats()
	require.Zero(t, sent)
}

// TestSimAdvanceIdleNoModels asserts that an idle stretch is harmless on a
// simulation with neither a virtual clock nor background traffic, the default
// configuration of every batch run to date.
func TestSimAdvanceIdleNoModels(t *testing.T) {
	t.Parallel()

	graph, source, _ := warmupTestGraph(t)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	defer runner.Close()

	before := balanceSnapshot(graph)
	runner.AdvanceIdle(600)

	require.Equal(t, before, balanceSnapshot(graph))

	sent, _ := runner.TrafficStats()
	require.Zero(t, sent)
}
