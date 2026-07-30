package routing

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// liquidityTestChain builds a path of numChannels channels over numChannels+1
// nodes, with the fixed capacities the golden test below was captured
// against.
func liquidityTestChain(t *testing.T, caps []btcutil.Amount) *SimGraph {
	t.Helper()

	g := NewSimGraph()
	for i := uint32(1); i <= uint32(len(caps))+1; i++ {
		_, err := g.AddNode(SimNodePubKey(i), fmt.Sprintf("n%d", i))
		require.NoError(t, err)
	}

	for i := range caps {
		id := uint64(i + 1)
		require.NoError(t, g.AddChannel(
			id, SimNodePubKey(uint32(i+1)),
			SimNodePubKey(uint32(i+2)), caps[i],
			SimPolicy{}, SimPolicy{},
		))
	}

	return g
}

// liquidityTestStars builds numHubs stars of numLeaves leaves each. Every
// channel therefore joins a node of degree numLeaves to a node of degree one,
// which is the topology the hubdrain model is defined against.
func liquidityTestStars(t *testing.T, numHubs, numLeaves int,
	capacity btcutil.Amount) (*SimGraph, []route.Vertex) {

	t.Helper()

	g := NewSimGraph()

	var (
		hubs   []route.Vertex
		nextID uint32 = 1
		chanID uint64
	)
	for h := 0; h < numHubs; h++ {
		hub := SimNodePubKey(nextID)
		nextID++
		_, err := g.AddNode(hub, fmt.Sprintf("hub%d", h))
		require.NoError(t, err)
		hubs = append(hubs, hub)

		for l := 0; l < numLeaves; l++ {
			leaf := SimNodePubKey(nextID)
			nextID++
			_, err := g.AddNode(leaf, fmt.Sprintf("leaf%d", nextID))
			require.NoError(t, err)

			chanID++
			require.NoError(t, g.AddChannel(
				chanID, hub, leaf, capacity, SimPolicy{},
				SimPolicy{},
			))
		}
	}

	return g, hubs
}

// node1Fractions returns the share of each channel's capacity that sits on
// its node1 end, in channel id order.
func node1Fractions(g *SimGraph) []float64 {
	fracs := make([]float64, 0, len(g.channels))
	for id := uint64(1); id <= uint64(len(g.channels)); id++ {
		channel := g.channels[id]
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)
		fracs = append(fracs, float64(channel.ends[0].balance)/
			float64(capacityMsat))
	}

	return fracs
}

// allBalances returns both ends of every channel in channel id order, the
// value a determinism check compares.
func allBalances(g *SimGraph) [][2]lnwire.MilliSatoshi {
	out := make([][2]lnwire.MilliSatoshi, 0, len(g.channels))
	for id := uint64(1); id <= uint64(len(g.channels)); id++ {
		channel := g.channels[id]
		out = append(out, [2]lnwire.MilliSatoshi{
			channel.ends[0].balance, channel.ends[1].balance,
		})
	}

	return out
}

// TestAssignLiquidityLegacyGolden pins the exact balances the three legacy
// models produce. Every scenario corpus regenerates from a fixed seed, so a
// change to these numbers silently moves every published result: the goldens
// were captured from the pre-parameterization code and must survive it.
func TestAssignLiquidityLegacyGolden(t *testing.T) {
	t.Parallel()

	caps := []btcutil.Amount{1_000_000, 2_000_000, 500_000, 750_000}

	golden := map[LiquidityModel][][2]lnwire.MilliSatoshi{
		LiquidityHalf: {
			{500_000_000, 500_000_000},
			{1_000_000_000, 1_000_000_000},
			{250_000_000, 250_000_000},
			{375_000_000, 375_000_000},
		},
		LiquidityUniform: {
			{790_699_325, 209_300_675},
			{239_482_843, 1_760_517_157},
			{458_314_107, 41_685_893},
			{458_607_231, 291_392_769},
		},
		LiquidityBimodal: {
			{24_786_920, 975_213_080},
			{1_984_676_655, 15_323_345},
			{2_899_105, 497_100_895},
			{717_786_929, 32_213_071},
		},
	}

	for model, want := range golden {
		g := liquidityTestChain(t, caps)
		require.NoError(t, g.AssignLiquidity(model, 42))
		require.Equal(t, want, allBalances(g), "model %v", model)
	}

	// "bimodal:0.05" spells out the legacy scale, so it must draw exactly
	// what "bimodal" draws.
	g := liquidityTestChain(t, caps)
	require.NoError(t, g.AssignLiquidity("bimodal:0.05", 42))
	require.Equal(t, golden[LiquidityBimodal], allBalances(g))
}

// TestAssignLiquidityDeterminism checks that every parameterized family is a
// pure function of (model string, seed): a corpus that names one of them has
// to regenerate identically, and two seeds have to disagree.
func TestAssignLiquidityDeterminism(t *testing.T) {
	t.Parallel()

	caps := make([]btcutil.Amount, 40)
	for i := range caps {
		caps[i] = btcutil.Amount(1_000_000 + 10_000*i)
	}

	models := []LiquidityModel{
		"bimodal:0.2", "beta:0.3:0.3", "beta:2:2", "hubdrain:0.05",
	}

	for _, model := range models {
		first := liquidityTestChain(t, caps)
		require.NoError(t, first.AssignLiquidity(model, 7))

		second := liquidityTestChain(t, caps)
		require.NoError(t, second.AssignLiquidity(model, 7))

		require.Equal(t, allBalances(first), allBalances(second),
			"model %v is not deterministic", model)

		other := liquidityTestChain(t, caps)
		require.NoError(t, other.AssignLiquidity(model, 8))

		require.NotEqual(t, allBalances(first), allBalances(other),
			"model %v ignores the seed", model)
	}
}

// TestAssignLiquidityBetaShape checks that the two beta families the
// robustness sweep uses have the shapes their parameters promise: Beta(2,2)
// is centered and tight, Beta(0.3,0.3) puts most of its mass at the ends.
func TestAssignLiquidityBetaShape(t *testing.T) {
	t.Parallel()

	caps := make([]btcutil.Amount, 2_000)
	for i := range caps {
		caps[i] = 10_000_000
	}

	stats := func(model LiquidityModel) (float64, float64, float64) {
		g := liquidityTestChain(t, caps)
		require.NoError(t, g.AssignLiquidity(model, 11))

		fracs := node1Fractions(g)

		var sum, extremes float64
		for _, f := range fracs {
			sum += f
			if f < 0.1 || f > 0.9 {
				extremes++
			}
		}
		mean := sum / float64(len(fracs))

		var variance float64
		for _, f := range fracs {
			variance += (f - mean) * (f - mean)
		}
		variance /= float64(len(fracs))

		return mean, variance, extremes / float64(len(fracs))
	}

	// Beta(2,2): mean 0.5, variance 1/20, almost nothing at the ends.
	centeredMean, centeredVar, centeredEnds := stats("beta:2:2")
	require.InDelta(t, 0.5, centeredMean, 0.03)
	require.InDelta(t, 0.05, centeredVar, 0.01)
	require.Less(t, centeredEnds, 0.1)

	// Beta(0.3,0.3): same mean, but the mass is at the two ends, so both
	// the variance and the tail count are far larger.
	uMean, uVar, uEnds := stats("beta:0.3:0.3")
	require.InDelta(t, 0.5, uMean, 0.05)
	require.Greater(t, uVar, 3*centeredVar)
	require.Greater(t, uEnds, 0.5)
}

// TestAssignLiquidityBimodalScale checks that widening the exponential scale
// really does fatten the middle of the distribution, which is the whole point
// of exposing the constant: an evolved router tuned to the 0.05 generator
// meets channels it has no prior for.
func TestAssignLiquidityBimodalScale(t *testing.T) {
	t.Parallel()

	caps := make([]btcutil.Amount, 2_000)
	for i := range caps {
		caps[i] = 10_000_000
	}

	midRange := func(model LiquidityModel) float64 {
		g := liquidityTestChain(t, caps)
		require.NoError(t, g.AssignLiquidity(model, 5))

		var mid float64
		for _, f := range node1Fractions(g) {
			if f > 0.1 && f < 0.9 {
				mid++
			}
		}

		return mid / float64(len(g.channels))
	}

	// A 0.05 scale leaves the middle at exp(-2) of the draws; a 0.2 scale
	// puts most of them there.
	narrow := midRange(LiquidityBimodal)
	wide := midRange("bimodal:0.2")

	require.InDelta(t, math.Exp(-2), narrow, 0.05)
	require.Greater(t, wide, 0.5)
	require.Greater(t, wide, 3*narrow)
}

// TestAssignLiquidityHubDrain checks the one generator that is correlated
// with topology: over channels that each join a hub to a leaf, the depleted
// end should be the hub's own side about hubDrainProb of the time.
func TestAssignLiquidityHubDrain(t *testing.T) {
	t.Parallel()

	g, hubs := liquidityTestStars(t, 20, 10, 10_000_000)
	require.NoError(t, g.AssignLiquidity("hubdrain:0.05", 3))

	isHub := make(map[route.Vertex]struct{}, len(hubs))
	for _, hub := range hubs {
		isHub[hub] = struct{}{}
	}

	var hubDepleted, total float64
	for _, channel := range g.channels {
		var hub route.Vertex
		if _, ok := isHub[channel.ends[0].owner]; ok {
			hub = channel.ends[0].owner
		} else {
			hub = channel.ends[1].owner
		}

		total++
		if channel.end(hub).balance < channel.otherEnd(hub).balance {
			hubDepleted++
		}
	}

	require.InDelta(t, hubDrainProb, hubDepleted/total, 0.05)

	// The fair coin of the plain bimodal model gives no such correlation,
	// which is what makes this family a different regime rather than a
	// relabeling of the old one.
	require.NoError(t, g.AssignLiquidity(LiquidityBimodal, 3))

	var coinDepleted float64
	for _, channel := range g.channels {
		hub := channel.ends[0].owner
		if _, ok := isHub[hub]; !ok {
			hub = channel.ends[1].owner
		}

		if channel.end(hub).balance < channel.otherEnd(hub).balance {
			coinDepleted++
		}
	}

	require.InDelta(t, 0.5, coinDepleted/total, 0.06)
}

// TestAssignLiquidityParseErrors checks that a malformed model string is a
// descriptive error and leaves the graph untouched, rather than a panic or a
// half assigned network.
func TestAssignLiquidityParseErrors(t *testing.T) {
	t.Parallel()

	caps := []btcutil.Amount{1_000_000, 2_000_000}

	bad := []LiquidityModel{
		"bimodal:x", "bimodal:", "bimodal:0", "bimodal:-1",
		"bimodal:0.1:0.2", "beta", "beta:2", "beta:-1:2",
		"beta:2:0", "beta:a:2", "beta:2:2:2", "hubdrain",
		"hubdrain:x", "hubdrain:0", "", "gaussian:1",
	}

	for _, model := range bad {
		g := liquidityTestChain(t, caps)
		before := allBalances(g)

		err := g.AssignLiquidity(model, 1)
		require.Error(t, err, "model %q was accepted", model)
		require.Contains(t, err.Error(), "liquidity model")
		require.Equal(t, before, allBalances(g),
			"model %q mutated balances", model)
	}
}

// TestBetaSampleFallback exercises the rejection loop's escape hatch: shape
// parameters large enough that Jöhnk's algorithm effectively never accepts
// must still return a fraction in the unit interval instead of spinning.
func TestBetaSampleFallback(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		frac := betaSample(rng, 400, 400)
		require.GreaterOrEqual(t, frac, 0.0)
		require.LessOrEqual(t, frac, 1.0)
	}
}

// TestAssignLiquidityFromGraph checks the one model that draws nothing: every
// channel must end up holding exactly what the graph file said it holds, on
// the side the file said it, whichever way round the file named the ends.
// This is the whole point of the model, since a balance that lands on the
// wrong end is still a plausible looking network and would fail no aggregate.
func TestAssignLiquidityFromGraph(t *testing.T) {
	t.Parallel()

	g := writeSimGraphFixture(t, describeGraphBalanceFixture)
	require.NoError(t, g.AssignLiquidity(LiquidityFromGraph, 7))

	first := g.channels[1234]
	require.EqualValues(t, 250_000_000, first.ends[0].balance)
	require.EqualValues(t, 750_000_000, first.ends[1].balance)

	second := g.channels[5678]
	require.EqualValues(t, 300_000_000, second.ends[0].balance)
	require.EqualValues(t, 700_000_000, second.ends[1].balance)

	// Every end still adds up to its capacity, and the seed buys nothing:
	// the model consumes no randomness, so two different seeds are the
	// same network.
	for _, channel := range g.channels {
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)
		require.Equal(t, capacityMsat,
			channel.ends[0].balance+channel.ends[1].balance)
	}

	seeded := fixtureBalances(g)
	require.NoError(t, g.AssignLiquidity(LiquidityFromGraph, 99))
	require.Equal(t, seeded, fixtureBalances(g))
}

// fixtureBalances returns both ends of every channel keyed by channel id, for
// graphs whose ids are whatever a file happened to carry rather than the 1..N
// that allBalances walks.
func fixtureBalances(g *SimGraph) map[uint64][2]lnwire.MilliSatoshi {
	out := make(map[uint64][2]lnwire.MilliSatoshi, len(g.channels))
	for id, channel := range g.channels {
		out[id] = [2]lnwire.MilliSatoshi{
			channel.ends[0].balance, channel.ends[1].balance,
		}
	}

	return out
}

// TestAssignLiquidityFromGraphMissing checks that a graph which does not
// carry the balances the model needs is a loud failure with a count, not a
// quiet fallback. A silent fallback here would report a synthetic liquidity
// family under the name of a modelled one, which is exactly the confusion the
// model exists to remove.
func TestAssignLiquidityFromGraphMissing(t *testing.T) {
	t.Parallel()

	// A synthetic topology carries no balances at all.
	g := liquidityTestChain(t, []btcutil.Amount{1_000_000, 2_000_000})
	before := allBalances(g)

	err := g.AssignLiquidity(LiquidityFromGraph, 1)
	require.ErrorContains(t, err, "2 of 2 channels carry no balance")
	require.Equal(t, before, allBalances(g))

	// A partly modelled graph is refused on the same terms: the whole
	// network is scored or none of it is.
	require.NoError(t, g.setGraphBalance(
		1, SimNodePubKey(1), lnwire.MilliSatoshi(400_000_000), 0.5,
	))

	err = g.AssignLiquidity(LiquidityFromGraph, 1)
	require.ErrorContains(t, err, "1 of 2 channels carry no balance")
	require.Equal(t, before, allBalances(g))
}

// TestSetGraphBalanceErrors checks the two ways a caller can misname a
// channel it is recording balances for.
func TestSetGraphBalanceErrors(t *testing.T) {
	t.Parallel()

	g := liquidityTestChain(t, []btcutil.Amount{1_000_000})

	err := g.setGraphBalance(7, SimNodePubKey(1), 1, 0)
	require.ErrorContains(t, err, "unknown channel")

	err = g.setGraphBalance(1, SimNodePubKey(3), 1, 0)
	require.ErrorContains(t, err, "not a party to channel")

	err = g.setGraphBalance(
		1, SimNodePubKey(1), lnwire.MilliSatoshi(1_000_000_001), 0,
	)
	require.ErrorContains(t, err, "outside its")
}
