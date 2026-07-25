package routing

import (
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// corridorsTestSpec returns a corridors spec with the given seed and corridor
// count, sized like the entries of the splitting pressure corpus.
func corridorsTestSpec(seed int64, corridors int) *SimTopologySpec {
	return &SimTopologySpec{
		Type:           "corridors",
		NumNodes:       60,
		ChannelSizeSat: 96_000_000,
		Seed:           seed,
		Corridors:      corridors,
	}
}

// corridorTiersDesc returns the nominal corridor tiers of a spec, largest
// first.
func corridorTiersDesc(spec *SimTopologySpec) []btcutil.Amount {
	tiers := CorridorTierCapacities(spec)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] > tiers[j] })

	return tiers
}

// nodeChannelCaps returns the capacities of all channels of the given
// synthetic node id, largest first.
func nodeChannelCaps(t *testing.T, g *SimGraph, id uint32) []btcutil.Amount {
	t.Helper()

	node := g.Node(SimNodePubKey(id))
	require.NotNil(t, node, "node %d missing", id)

	caps := make([]btcutil.Amount, 0, len(node.channels))
	for _, channel := range node.channels {
		caps = append(caps, channel.Capacity)
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] > caps[j] })

	return caps
}

// TestSimCorridorsDeterminism asserts that a corridors spec generates the same
// graph every time, and that a different seed generates a different one.
func TestSimCorridorsDeterminism(t *testing.T) {
	t.Parallel()

	build := func(seed int64) map[uint64][2]lnwire.MilliSatoshi {
		graph, err := GenerateSimGraph(corridorsTestSpec(seed, 5))
		require.NoError(t, err)

		// Liquidity assignment folds the capacities into the snapshot:
		// the two ends of a channel always sum to its capacity.
		require.NoError(t, graph.AssignLiquidity(LiquidityBimodal, 9))

		return balanceSnapshot(graph)
	}

	require.Equal(t, build(4), build(4), "same seed diverged")
	require.NotEqual(t, build(4), build(5), "different seeds agreed")
}

// TestSimCorridorsTierContract pins the tier ladder that
// simulation/gen_scenarios.py mirrors when it sizes splitting pressure
// payments: the nominal tiers are a fixed fraction of ChannelSizeSat, and the
// generated tier channels never exceed them.
func TestSimCorridorsTierContract(t *testing.T) {
	t.Parallel()

	spec := corridorsTestSpec(7, 6)
	tiers := CorridorTierCapacities(spec)
	require.Len(t, tiers, spec.Corridors)

	// The largest tier is exactly the channel size divided by the
	// bottleneck ratio, and the rest follow the ladder.
	weights := corridorTierWeights(spec.Corridors)
	for i, tier := range tiers {
		want := spec.ChannelSizeSat * int64(weights[i]) /
			(int64(weights[0]) * corridorBottleneckRatio)
		require.EqualValues(t, want, tier, "tier %d", i)
	}

	graph, err := GenerateSimGraph(spec)
	require.NoError(t, err)

	// The target's channels are the tier channels, jittered down from the
	// nominal ladder but never above it.
	got := nodeChannelCaps(t, graph, uint32(spec.NumNodes))
	require.Len(t, got, spec.Corridors, "target degree is not the "+
		"corridor count")

	for i, tier := range corridorTiersDesc(spec) {
		require.LessOrEqual(t, got[i], tier, "tier %d above nominal", i)
		require.GreaterOrEqual(
			t, float64(got[i]),
			float64(tier)*(1-corridorTierJitterFrac),
			"tier %d jittered below the floor", i,
		)
	}
}

// TestSimCorridorsStructure asserts the properties the splitting pressure
// corpus depends on: asymmetric corridor tiers, a target reachable only
// through them, and a source that can fund every corridor.
func TestSimCorridorsStructure(t *testing.T) {
	t.Parallel()

	for _, numCorridors := range []int{3, 6, 12} {
		spec := corridorsTestSpec(int64(numCorridors)*11, numCorridors)

		graph, err := GenerateSimGraph(spec)
		require.NoError(t, err)

		tiers := corridorTiersDesc(spec)
		fillerCap := tiers[len(tiers)-1] / 4

		// The target is only reachable through the corridor tiers, so
		// its inbound capacity is the hard ceiling on the payment and
		// its largest channel the hard ceiling on a single shard.
		inbound := nodeChannelCaps(t, graph, uint32(spec.NumNodes))
		require.Len(t, inbound, numCorridors)

		// The tiering is genuinely asymmetric: the fattest corridor is
		// at least four times the thinnest, so an amount no single
		// corridor can carry never divides evenly across them.
		require.GreaterOrEqual(
			t, float64(inbound[0]),
			4*float64(inbound[len(inbound)-1]),
			"corridor tiers are not asymmetric",
		)

		// A split can still deliver what no single corridor can: the
		// tiers together are well above the largest one.
		var total btcutil.Amount
		for _, cap := range inbound {
			total += cap
		}
		require.Greater(
			t, float64(total), 1.5*float64(inbound[0]),
			"corridors have no headroom over the largest tier",
		)

		// The source funds one fat channel into the head of every
		// corridor, so a failure reflects downstream capacity rather
		// than an underfunded sender. Its remaining channel, if any, is
		// the cheap way into the filler cloud.
		outbound := nodeChannelCaps(t, graph, 1)
		require.GreaterOrEqual(t, len(outbound), numCorridors)

		for i := 0; i < numCorridors; i++ {
			require.Greater(
				t, float64(outbound[i]),
				float64(inbound[0]),
				"source channel %d is not fat", i,
			)
		}
		for i := numCorridors; i < len(outbound); i++ {
			require.Equal(
				t, fillerCap, outbound[i],
				"extra source channel %d is not filler", i,
			)
		}

		// The source is not adjacent to the target: every payment has
		// to cross a corridor.
		source := graph.Node(SimNodePubKey(1))
		target := SimNodePubKey(uint32(spec.NumNodes))
		for _, channel := range source.channels {
			require.NotEqual(
				t, target, channel.otherEnd(source.PubKey).owner,
				"source is adjacent to the target",
			)
		}

		// The filler cloud exists and is made of channels too thin to
		// carry a shard that matters.
		var fillerChannels int
		for _, channel := range graph.channels {
			if channel.Capacity == fillerCap {
				fillerChannels++
			}
		}
		require.Positive(t, fillerChannels, "no filler channels")
	}
}

// TestSimCorridorsRejectsBadSpecs asserts that the generator refuses specs it
// cannot honor rather than silently building a degenerate graph.
func TestSimCorridorsRejectsBadSpecs(t *testing.T) {
	t.Parallel()

	// Fewer than two corridors cannot pose a splitting problem.
	spec := corridorsTestSpec(1, 1)
	_, err := GenerateSimGraph(spec)
	require.ErrorContains(t, err, "at least 2 corridors")

	// Channels too small for the bottleneck ratio leave a thinnest tier
	// below the minimum shard the production splitter will send.
	spec = corridorsTestSpec(1, 6)
	spec.ChannelSizeSat = 1_000_000
	_, err = GenerateSimGraph(spec)
	require.ErrorContains(t, err, "too small")

	// The node budget has to cover the corridor interiors.
	spec = corridorsTestSpec(1, 6)
	spec.NumNodes = 4
	_, err = GenerateSimGraph(spec)
	require.ErrorContains(t, err, "needs at least")
}

// TestSimCorridorsSplitPressure is the behavioral test: on a corridors graph a
// payment larger than the fattest corridor can never complete as a single
// path, but the lnd stack's splitting can carry it across corridors.
func TestSimCorridorsSplitPressure(t *testing.T) {
	t.Parallel()

	// run pays twice the largest tier from a freshly generated corridors
	// network with the given seed. Twice the fattest tier needs three
	// corridors at the very least, since no other corridor reaches half of
	// it.
	run := func(seed int64, maxParts uint32) *SimScenarioResult {
		spec := corridorsTestSpec(seed, 12)

		graph, err := GenerateSimGraph(spec)
		require.NoError(t, err)
		require.NoError(t, graph.AssignLiquidity(
			LiquidityBimodal, seed*7+1,
		))

		source, err := graph.ResolveNode("1")
		require.NoError(t, err)
		require.NoError(t, graph.BalanceNodeChannels(source))

		runner, err := NewSimRunner(
			graph, DefaultSimParams(), source, t.TempDir(),
		)
		require.NoError(t, err)
		defer runner.Close()

		// Freeze the clock so the outcome depends only on the seeds and
		// not on how long mission control's penalties took to decay on
		// the wall clock.
		runner.SetVirtualClock(&SimClockParams{StartUnix: 1_800_000_000})

		tiers := corridorTiersDesc(spec)
		amt := lnwire.NewMSatFromSatoshis(tiers[0]) * 2

		result, err := runner.RunScenario(&SimScenario{
			Target:   "60",
			AmtMsat:  uint64(amt),
			MaxParts: maxParts,
		})
		require.NoError(t, err)

		return result
	}

	var (
		splitSuccesses int
		multiShard     int
	)
	for seed := int64(1); seed <= 8; seed++ {
		// No single shard can exceed the fattest tier, and the target
		// has no other inbound capacity, so an unsplit payment cannot
		// succeed on any seed.
		single := run(seed, 1)
		require.False(
			t, single.Success,
			"seed %d: unsplit payment succeeded", seed,
		)

		split := run(seed, 16)
		if !split.Success {
			continue
		}
		splitSuccesses++

		// Count the settled shards: at twice the fattest tier the
		// payment cannot have been delivered by fewer than three, since
		// no corridor but the fattest reaches half of it.
		var settled int
		for _, attempt := range split.Attempts {
			if attempt.Success {
				settled++
			}
		}
		if settled >= 3 {
			multiShard++
		}
	}

	require.GreaterOrEqual(
		t, splitSuccesses, 3, "splitting rarely succeeded",
	)
	require.Equal(
		t, splitSuccesses, multiShard,
		"a payment at twice the fattest tier settled in under three "+
			"shards",
	)
}
