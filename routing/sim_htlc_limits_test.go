package routing

import (
	"fmt"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// htlcLimitsTestSpec is the fixed topology the golden below was captured
// against. Nothing about it may change: the golden is a claim about what the
// policy generator produces, and a different spec would be a different claim.
var htlcLimitsTestSpec = SimTopologySpec{
	Type:           "smallworld",
	NumNodes:       8,
	ChannelSizeSat: 1_000_000,
	Seed:           42,
	AvgDegree:      4,
}

// goldenPolicy is one directed policy of the golden table: the channel it
// belongs to, which end announced it, and the four fields the generator sets.
type goldenPolicy struct {
	chanID  uint64
	end     int
	baseFee lnwire.MilliSatoshi
	feeRate lnwire.MilliSatoshi
	minHTLC lnwire.MilliSatoshi
	maxHTLC lnwire.MilliSatoshi
}

// allPolicies returns every directed policy of the graph, in channel id order
// and node1 end first. That is the same order ApplyHtlcLimits walks, so a
// comparison of two of these slices is a comparison of the whole network's
// announced policy.
func allPolicies(g *SimGraph) []goldenPolicy {
	ids := make([]uint64, 0, len(g.channels))
	for id := range g.channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]goldenPolicy, 0, 2*len(ids))
	for _, id := range ids {
		channel := g.channels[id]
		for i := range channel.ends {
			policy := channel.ends[i].policy
			out = append(out, goldenPolicy{
				chanID:  id,
				end:     i,
				baseFee: policy.BaseFeeMsat,
				feeRate: policy.FeeRatePPM,
				minHTLC: policy.MinHTLCMsat,
				maxHTLC: policy.MaxHTLCMsat,
			})
		}
	}

	return out
}

// htlcLimitsGolden is every directed policy the topology generator produced
// before the htlc_limits section existed. Every scenario corpus regenerates
// from a fixed seed, so a change to these numbers silently moves every
// published result: min htlc has been a flat 1000 msat and max htlc a flat
// zero (meaning no maximum) on every synthetic tier the program has ever run.
var htlcLimitsGolden = []goldenPolicy{
	{1, 0, 416, 101, 1000, 0},
	{1, 1, 862, 414, 1000, 0},
	{2, 0, 658, 675, 1000, 0},
	{2, 1, 351, 900, 1000, 0},
	{3, 0, 273, 138, 1000, 0},
	{3, 1, 86, 675, 1000, 0},
	{4, 0, 858, 395, 1000, 0},
	{4, 1, 135, 375, 1000, 0},
	{5, 0, 865, 464, 1000, 0},
	{5, 1, 579, 716, 1000, 0},
	{6, 0, 614, 654, 1000, 0},
	{6, 1, 124, 345, 1000, 0},
	{7, 0, 92, 639, 1000, 0},
	{7, 1, 827, 789, 1000, 0},
	{8, 0, 74, 213, 1000, 0},
	{8, 1, 554, 97, 1000, 0},
	{9, 0, 678, 596, 1000, 0},
	{9, 1, 288, 357, 1000, 0},
	{10, 0, 314, 246, 1000, 0},
	{10, 1, 779, 940, 1000, 0},
	{11, 0, 866, 56, 1000, 0},
	{11, 1, 149, 542, 1000, 0},
	{12, 0, 819, 219, 1000, 0},
	{12, 1, 703, 970, 1000, 0},
	{13, 0, 103, 394, 1000, 0},
	{13, 1, 637, 837, 1000, 0},
	{14, 0, 462, 14, 1000, 0},
	{14, 1, 898, 974, 1000, 0},
	{15, 0, 976, 612, 1000, 0},
	{15, 1, 918, 45, 1000, 0},
	{16, 0, 935, 134, 1000, 0},
	{16, 1, 187, 305, 1000, 0},
}

// htlcLimitsTestGraph builds the golden topology.
func htlcLimitsTestGraph(t *testing.T) *SimGraph {
	t.Helper()

	spec := htlcLimitsTestSpec
	g, err := GenerateSimGraph(&spec)
	require.NoError(t, err)

	return g
}

// htlcLimitsStarGraph builds a graph of numChannels independent two-node
// channels of the given capacity, which is the shape a marginal check wants:
// one directed policy per end, no topology to interact with.
func htlcLimitsStarGraph(t *testing.T, numChannels int,
	capacity btcutil.Amount) *SimGraph {

	t.Helper()

	g := NewSimGraph()
	var next uint32 = 1
	for i := 0; i < numChannels; i++ {
		a, b := SimNodePubKey(next), SimNodePubKey(next+1)
		next += 2

		_, err := g.AddNode(a, fmt.Sprintf("a%d", i))
		require.NoError(t, err)
		_, err = g.AddNode(b, fmt.Sprintf("b%d", i))
		require.NoError(t, err)

		require.NoError(t, g.AddChannel(
			uint64(i+1), a, b, capacity, SimPolicy{}, SimPolicy{},
		))
	}

	return g
}

// maxHtlcFractions returns each directed policy's announced maximum as a
// fraction of its channel's capacity.
func maxHtlcFractions(g *SimGraph) []float64 {
	var fracs []float64
	for _, channel := range g.channels {
		capacityMsat := lnwire.NewMSatFromSatoshis(channel.Capacity)
		for i := range channel.ends {
			fracs = append(fracs, float64(
				channel.ends[i].policy.MaxHTLCMsat,
			)/float64(capacityMsat))
		}
	}

	return fracs
}

// minHtlcValues returns every directed policy's announced minimum.
func minHtlcValues(g *SimGraph) []lnwire.MilliSatoshi {
	var mins []lnwire.MilliSatoshi
	for _, channel := range g.channels {
		for i := range channel.ends {
			mins = append(mins, channel.ends[i].policy.MinHTLCMsat)
		}
	}

	return mins
}

// TestHtlcLimitsAbsentGolden pins the announced policy of every generated
// channel with no htlc_limits section in play. The section is the whole of
// stage A, and its off state has to reproduce the constants every published
// result was measured against: a flat 1000 msat floor and no ceiling at all.
func TestHtlcLimitsAbsentGolden(t *testing.T) {
	t.Parallel()

	g := htlcLimitsTestGraph(t)
	require.Equal(t, htlcLimitsGolden, allPolicies(g))

	// A nil section, an empty section and a section that names no family
	// are all the same no-op, since a corpus reaches the simulator through
	// any of the three depending on how it was written.
	sections := []*SimHtlcLimitsParams{
		nil,
		{},
		{Seed: 99},
	}
	for _, section := range sections {
		g := htlcLimitsTestGraph(t)
		require.NoError(t, g.ApplyHtlcLimits(section, 7))
		require.Equal(t, htlcLimitsGolden, allPolicies(g))
	}

	// With no section the network announces nothing that can bind, which
	// is the finding stage A exists to fix.
	require.Equal(t, SimHtlcLimitStats{Policies: 32}, g.HtlcLimitStats())
}

// TestApplyHtlcLimitsDeterminism checks that the redraw is a pure function of
// (families, seed): a corpus that names the section has to regenerate
// identically, and two seeds have to disagree.
func TestApplyHtlcLimitsDeterminism(t *testing.T) {
	t.Parallel()

	families := []SimHtlcLimitsParams{
		{
			MaxHtlcFracFamily: HtlcLimitFamilyMainnet,
			MinHtlcFamily:     HtlcLimitFamilyMainnet,
		},
		{MaxHtlcFracFamily: HtlcLimitFamilyTight},
		{MinHtlcFamily: HtlcLimitFamilyMainnet},
	}

	for _, params := range families {
		first := htlcLimitsTestGraph(t)
		require.NoError(t, first.ApplyHtlcLimits(&params, 11))

		second := htlcLimitsTestGraph(t)
		require.NoError(t, second.ApplyHtlcLimits(&params, 11))

		require.Equal(t, allPolicies(first), allPolicies(second),
			"section %+v is not deterministic", params)

		// The default seed is derived from the liquidity seed, so a
		// corpus that varies only its liquidity seed still varies its
		// limits.
		other := htlcLimitsTestGraph(t)
		require.NoError(t, other.ApplyHtlcLimits(&params, 12))
		require.NotEqual(t, allPolicies(first), allPolicies(other),
			"section %+v ignores the default seed", params)

		// An explicit seed overrides the derived one.
		pinned := params
		pinned.Seed = 11
		explicit := htlcLimitsTestGraph(t)
		require.NoError(t, explicit.ApplyHtlcLimits(&pinned, 12))

		repeat := htlcLimitsTestGraph(t)
		require.NoError(t, repeat.ApplyHtlcLimits(&pinned, 999))
		require.Equal(t, allPolicies(explicit), allPolicies(repeat),
			"section %+v ignores its pinned seed", params)
	}
}

// TestApplyHtlcLimitsIndependentStreams checks that each directed policy burns
// the same number of draws whichever families are named, so that moving the
// maximum family does not silently move every minimum as well. Without that,
// a paired tier that changes one knob would change both.
func TestApplyHtlcLimitsIndependentStreams(t *testing.T) {
	t.Parallel()

	both := htlcLimitsTestGraph(t)
	require.NoError(t, both.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: HtlcLimitFamilyMainnet,
		MinHtlcFamily:     HtlcLimitFamilyMainnet,
	}, 5))

	minOnly := htlcLimitsTestGraph(t)
	require.NoError(t, minOnly.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MinHtlcFamily: HtlcLimitFamilyMainnet,
	}, 5))

	// The minimums have to match even though one graph also drew a
	// ceiling, except where the ceiling clamped the floor under it.
	bothPolicies := allPolicies(both)
	minPolicies := allPolicies(minOnly)
	require.Len(t, minPolicies, len(bothPolicies))

	for i := range bothPolicies {
		clamped := bothPolicies[i].maxHTLC != 0 &&
			minPolicies[i].minHTLC > bothPolicies[i].maxHTLC
		if clamped {
			continue
		}

		require.Equal(t, minPolicies[i].minHTLC,
			bothPolicies[i].minHTLC, "policy %d", i)
	}

	// The tight family shares the minimum ladder, so it must agree too.
	tight := htlcLimitsTestGraph(t)
	require.NoError(t, tight.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: HtlcLimitFamilyTight,
		MinHtlcFamily:     HtlcLimitFamilyMainnet,
	}, 5))

	tightPolicies := allPolicies(tight)
	for i := range tightPolicies {
		clamped := tightPolicies[i].maxHTLC != 0 &&
			minPolicies[i].minHTLC > tightPolicies[i].maxHTLC
		if clamped {
			continue
		}

		require.Equal(t, minPolicies[i].minHTLC,
			tightPolicies[i].minHTLC, "policy %d", i)
	}
}

// TestApplyHtlcLimitsMainnetMarginals checks the empirical family against the
// survey it was measured from. The tolerances are wide enough for sampling
// noise at four thousand policies and tight enough that an authored shape
// could not pass: what is being asserted is that the draws reproduce the real
// graph's marginals, which is the entire justification for the family.
func TestApplyHtlcLimitsMainnetMarginals(t *testing.T) {
	t.Parallel()

	g := htlcLimitsStarGraph(t, 2_000, 1_000_000)
	require.NoError(t, g.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: HtlcLimitFamilyMainnet,
		MinHtlcFamily:     HtlcLimitFamilyMainnet,
	}, 3))

	fracs := maxHtlcFractions(g)
	require.Len(t, fracs, 4_000)

	sorted := append([]float64(nil), fracs...)
	sort.Float64s(sorted)

	// Median max_htlc/capacity is 0.99 on the real graph, and it is a
	// point mass rather than a crossing: three quarters of the policies
	// sit exactly there.
	require.InDelta(t, 0.99, sorted[len(sorted)/2], 0.01)

	// The fifth percentile is 0.20 and one directed policy in eight
	// announces a ceiling below half its capacity. That 13% is the whole
	// pressure of the stage.
	require.InDelta(t, 0.20, sorted[len(sorted)/20], 0.05)

	var belowHalf float64
	for _, frac := range fracs {
		if frac < 0.5 {
			belowHalf++
		}
	}
	require.InDelta(t, 0.13, belowHalf/float64(len(fracs)), 0.03)

	// Announced minimums: 78% at the 1000 msat mode, 5.9% at or above the
	// 100 sat floor that can actually refuse a shard.
	mins := minHtlcValues(g)
	var (
		mode  float64
		floor float64
	)
	for _, value := range mins {
		if value == 1_000 {
			mode++
		}
		if value >= simHtlcFloorMsat {
			floor++
		}
	}
	require.InDelta(t, 0.78, mode/float64(len(mins)), 0.03)
	require.InDelta(t, 0.059, floor/float64(len(mins)), 0.02)

	// The manipulation check the tier ships with has to see the same
	// thing: nearly every policy now announces a bounded ceiling, since
	// the empirical family gives 99% of them a maximum at 0.99 of
	// capacity, and the floors are the 5.9% tail.
	stats := g.HtlcLimitStats()
	require.Equal(t, 4_000, stats.Policies)
	require.Greater(t, stats.Bounded, 3_500)
	require.InDelta(t, 0.059, float64(stats.Floors)/4_000, 0.02)
}

// TestApplyHtlcLimitsTight checks the authored stress rung: every ceiling
// lands inside [0.1, 0.4] of capacity, so every channel binds.
func TestApplyHtlcLimitsTight(t *testing.T) {
	t.Parallel()

	g := htlcLimitsStarGraph(t, 500, 1_000_000)
	require.NoError(t, g.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: HtlcLimitFamilyTight,
		MinHtlcFamily:     HtlcLimitFamilyMainnet,
	}, 4))

	fracs := maxHtlcFractions(g)
	require.Len(t, fracs, 1_000)

	var sum float64
	for _, frac := range fracs {
		require.GreaterOrEqual(t, frac, tightMaxFracLow-0.001)
		require.LessOrEqual(t, frac, tightMaxFracHigh+0.001)
		sum += frac
	}

	// Uniform on the interval, so the mean sits at its midpoint.
	require.InDelta(t, 0.25, sum/float64(len(fracs)), 0.02)

	// Every directed policy binds, which is what makes this the stress
	// rung rather than a second empirical family.
	require.Equal(t, 1_000, g.HtlcLimitStats().Bounded)
}

// TestApplyHtlcLimitsWellFormed checks the two invariants a drawn policy has
// to satisfy: a ceiling of zero would mean "no maximum" rather than "almost
// none", and a floor above the ceiling would be a direction nothing at all can
// cross. Neither shape exists on the real graph.
func TestApplyHtlcLimitsWellFormed(t *testing.T) {
	t.Parallel()

	// Tiny channels are the adversarial case: a 0.000005 fraction of a
	// small capacity rounds to zero, and the ladder's 10 sat floor sits
	// above a large share of the drawn ceilings.
	g := htlcLimitsStarGraph(t, 2_000, 20)
	require.NoError(t, g.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: HtlcLimitFamilyMainnet,
		MinHtlcFamily:     HtlcLimitFamilyMainnet,
	}, 6))

	for _, channel := range g.channels {
		for i := range channel.ends {
			policy := channel.ends[i].policy
			require.NotZero(t, policy.MaxHTLCMsat)
			require.LessOrEqual(t, policy.MinHTLCMsat,
				policy.MaxHTLCMsat)
		}
	}
}

// TestApplyHtlcLimitsErrors checks that an unknown family is a descriptive
// error that leaves every policy untouched, rather than a panic or a half
// redrawn network.
func TestApplyHtlcLimitsErrors(t *testing.T) {
	t.Parallel()

	bad := []SimHtlcLimitsParams{
		{MaxHtlcFracFamily: "gaussian"},
		{MinHtlcFamily: "gaussian"},
		{
			MaxHtlcFracFamily: HtlcLimitFamilyMainnet,
			MinHtlcFamily:     "empirical",
		},
		{
			MaxHtlcFracFamily: "MAINNET_EMPIRICAL",
			MinHtlcFamily:     HtlcLimitFamilyMainnet,
		},
	}

	for _, params := range bad {
		g := htlcLimitsTestGraph(t)

		err := g.ApplyHtlcLimits(&params, 1)
		require.Error(t, err, "section %+v was accepted", params)
		require.Contains(t, err.Error(), "htlc limit family")
		require.Equal(t, htlcLimitsGolden, allPolicies(g),
			"section %+v mutated policies", params)
	}
}

// TestSampleMaxHtlcFracLadder checks the interpolation itself: the ladder has
// to be monotone in the draw and has to reproduce its own knots, since every
// marginal claim above rests on it.
func TestSampleMaxHtlcFracLadder(t *testing.T) {
	t.Parallel()

	var previous float64
	for i := 0; i <= 1_000; i++ {
		u := float64(i) / 1_000
		frac := sampleMaxHtlcFrac(htlcLimitFamilyEmpirical, u)

		require.GreaterOrEqual(t, frac, previous)
		require.LessOrEqual(t, frac, 1.0)
		previous = frac
	}

	for _, knot := range mainnetMaxHtlcFrac {
		require.InDelta(t, knot.frac, sampleMaxHtlcFrac(
			htlcLimitFamilyEmpirical, knot.quantile,
		), 1e-9, "knot %v", knot.quantile)
	}
}
