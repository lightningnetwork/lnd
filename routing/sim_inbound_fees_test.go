package routing

import (
	"context"
	"sort"
	"testing"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// setInboundFee pins the inbound fee one end of a channel announces, which is
// the fee its owner charges for htlcs ARRIVING over that channel.
func setInboundFee(t *testing.T, g *SimGraph, chanID uint64,
	owner route.Vertex, base, rate int32) {

	t.Helper()

	channel, ok := g.channels[chanID]
	require.True(t, ok, "unknown channel %d", chanID)

	end := channel.end(owner)
	require.NotNil(t, end, "node is not a party to channel %d", chanID)

	end.policy.InboundBaseMsat = base
	end.policy.InboundRatePPM = rate
}

// inboundTestRoute builds a route that pays every forwarding node its full
// node fee, inbound component included. It is the transcription of lnd's own
// newRoute (routing/pathfind.go): walk backward, and at each forwarding node
// add its outbound fee and then the inbound fee it charges on the sum, with
// the total floored at zero.
//
// atomicTestRoute is deliberately kept as the fee-blind builder: the contrast
// between the two is what the refusal counter measures.
func inboundTestRoute(t *testing.T, g *SimGraph, source route.Vertex,
	chanIDs []uint64, amt lnwire.MilliSatoshi) *route.Route {

	t.Helper()

	require.NotEmpty(t, chanIDs, "route needs at least one channel")

	nodes := []route.Vertex{source}
	for _, id := range chanIDs {
		channel, ok := g.channels[id]
		require.True(t, ok, "unknown channel %d", id)

		next := channel.otherEnd(nodes[len(nodes)-1])
		require.NotNil(t, next, "channel %d does not extend the "+
			"route", id)

		nodes = append(nodes, next.owner)
	}

	last := len(chanIDs) - 1
	amts := make([]lnwire.MilliSatoshi, len(chanIDs))
	expiries := make([]uint32, len(chanIDs))
	amts[last] = amt
	expiries[last] = atomicFinalCltv

	for k := last - 1; k >= 0; k-- {
		// The node forwarding onto channel k+1 sends out over its own
		// end of that channel and received over its own end of channel
		// k, so those are the two policies its fee is built from.
		outPolicy := &g.channels[chanIDs[k+1]].end(nodes[k+1]).policy
		inPolicy := &g.channels[chanIDs[k]].end(nodes[k+1]).policy

		fee, _ := nodeFee(outPolicy, inPolicy, amts[k+1])

		amts[k] = amts[k+1] + fee
		expiries[k] = expiries[k+1] + uint32(outPolicy.TimeLockDelta)
	}

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

// TestCheckPolicyLegacyGolden is the load-bearing identity test of stage B.
// checkPolicy sits on the hot path of every payment this program has ever run,
// and the stage moved its signature and rewrote its fee line. The table below
// is what it accepted and refused before that rewrite; a change to any entry
// silently moves every published result.
//
// Each case is run three ways that must agree: with no inbound policy at all,
// with an inbound policy announcing nothing, and through the whole walk with
// the mechanism switched off.
func TestCheckPolicyLegacyGolden(t *testing.T) {
	t.Parallel()

	// The policy under test charges a 1,000 msat base plus 100 ppm, floors
	// htlcs at 1,000 msat, ceilings them at 100,000 and wants 40 blocks.
	policy := SimPolicy{
		BaseFeeMsat:   1_000,
		FeeRatePPM:    100,
		TimeLockDelta: 40,
		MinHTLCMsat:   1_000,
		MaxHTLCMsat:   100_000,
	}

	cases := []struct {
		name     string
		amtIn    lnwire.MilliSatoshi
		amtOut   lnwire.MilliSatoshi
		expiryIn uint32
		expected lnwire.FailCode
	}{{
		// 50,000 out costs 1,000 + 5 = 1,005 in fee.
		name:     "exact fee",
		amtIn:    51_005,
		amtOut:   50_000,
		expiryIn: 100,
	}, {
		name:     "fee overpaid",
		amtIn:    52_000,
		amtOut:   50_000,
		expiryIn: 100,
	}, {
		name:     "fee one short",
		amtIn:    51_004,
		amtOut:   50_000,
		expiryIn: 100,
		expected: lnwire.CodeFeeInsufficient,
	}, {
		name:     "below floor",
		amtIn:    2_000,
		amtOut:   999,
		expiryIn: 100,
		expected: lnwire.CodeAmountBelowMinimum,
	}, {
		name:     "on the floor",
		amtIn:    3_000,
		amtOut:   1_000,
		expiryIn: 100,
	}, {
		name:     "above ceiling",
		amtIn:    200_000,
		amtOut:   100_001,
		expiryIn: 100,
		expected: lnwire.CodeTemporaryChannelFailure,
	}, {
		name:     "on the ceiling",
		amtIn:    200_000,
		amtOut:   100_000,
		expiryIn: 100,
	}, {
		name:     "expiry one short",
		amtIn:    51_005,
		amtOut:   50_000,
		expiryIn: 99,
		expected: lnwire.CodeIncorrectCltvExpiry,
	}}

	// The outgoing expiry every case is measured against.
	const expiryOut = 60

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			check := func(inPolicy *SimPolicy) {
				failure := checkPolicy(
					&policy, inPolicy, testCase.amtIn,
					testCase.amtOut, testCase.expiryIn,
					expiryOut,
				)

				if testCase.expected == 0 {
					require.Nil(t, failure)
					return
				}

				require.NotNil(t, failure)
				require.Equal(
					t, testCase.expected, failure.Code(),
				)
			}

			// No inbound policy, which is what every hop gets while
			// the mechanism is off.
			check(nil)

			// An inbound policy that announces no inbound fee,
			// which is what 88% of the real graph announces and
			// what every generated policy announces.
			check(&SimPolicy{
				BaseFeeMsat: 5_000,
				FeeRatePPM:  9_000,
			})
		})
	}
}

// TestInboundFeeOffByDefault checks that a network carrying inbound fees but
// no inbound_fees section behaves exactly as it did before stage B: the fees
// are dead data, nothing is charged, and nothing is counted. This is the case
// the mainnet tier runs in, where the loader now preserves 4,783 real inbound
// fees that no published number has ever priced.
func TestInboundFeeOffByDefault(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, nodeA := nodes[0], nodes[1]

	// Node A announces a surcharge on the channel it receives over, big
	// enough that a fee-blind sender could not possibly cover it.
	setInboundFee(t, graph, 1, nodeA, 10_000, 100_000)

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, 50_000)
	result, err := graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())

	// The same htlc with the mechanism on is refused, and the refusal is
	// attributed to the inbound fee rather than to the outbound one.
	graph.inboundFees = true

	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeFeeInsufficient, result.Failure.Code())
	require.Equal(t, nodeA, result.FailureSource)
	require.Equal(t, SimPolicyStats{
		InboundFeeCharged:  1,
		InboundFeeRefusals: 1,
	}, graph.PolicyStats())
}

// TestInboundFeeForwarding transcribes the inbound fee pairs of lnd's own
// TestChannelLinkInboundFee onto the simulator's three hop fixture, so that
// the sim's arithmetic is checked against lnd's rather than against itself.
// In every case the middle node forwards 1,000,000 msat and charges a 1,100
// msat outbound fee (the fixture's 1,000 base plus 100 ppm, where lnd's test
// uses a flat 1,000), so its inbound fee is computed on 1,001,100.
func TestInboundFeeForwarding(t *testing.T) {
	t.Parallel()

	const (
		deliver     = lnwire.MilliSatoshi(1_000_000)
		outboundFee = lnwire.MilliSatoshi(1_100)
	)

	cases := []struct {
		name string
		base int32
		rate int32

		// aware is what a sender that prices the inbound fee pays on
		// top of the delivered amount, and blind is what a sender that
		// prices only the outbound fee pays.
		aware lnwire.MilliSatoshi
		blind lnwire.MilliSatoshi

		// blindRefused says whether the fee-blind sender's htlc is
		// turned away, which only a surcharge can do.
		blindRefused bool
	}{{
		// -500 base and -100 ppm on 1,001,100 is -600, the rate
		// component rounding up exactly as lnd's case does. The aware
		// sender pays 500 where the blind one pays the full 1,100 and
		// is simply overpaying.
		name:  "discount",
		base:  -500,
		rate:  -100,
		aware: 500,
		blind: outboundFee,
	}, {
		// A discount larger than the outbound fee it nets against.
		// The node forwards for free rather than paying to forward, so
		// the aware sender pays nothing and cannot pay less.
		name:  "discount past zero",
		base:  -5_000,
		aware: 0,
		blind: outboundFee,
	}, {
		// 1,000 base and 100,000 ppm on 1,001,100 is 101,110, on top
		// of the 1,100 outbound fee.
		name:         "surcharge",
		base:         1_000,
		rate:         100_000,
		aware:        102_210,
		blind:        outboundFee,
		blindRefused: true,
	}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			graph, nodes := atomicTestGraph(t)
			source, nodeA := nodes[0], nodes[1]
			graph.inboundFees = true

			require.EqualValues(t, outboundFee, graph.channels[2].
				end(nodeA).policy.fee(deliver))

			setInboundFee(
				t, graph, 1, nodeA, testCase.base,
				testCase.rate,
			)

			// The fee-aware sender's route pays exactly what the
			// node requires, and the route builder agrees with the
			// hand-computed figure from lnd's test.
			rt := inboundTestRoute(
				t, graph, source, []uint64{1, 2}, deliver,
			)
			require.Equal(t, deliver+testCase.aware, rt.TotalAmount)

			result, err := graph.SendHtlc(rt)
			require.NoError(t, err)
			require.Nil(t, result.Failure)

			// One msat less is refused whenever the node charges
			// anything at all.
			if testCase.aware > 0 {
				rt.TotalAmount--

				result, err = graph.SendHtlc(rt)
				require.NoError(t, err)
				require.NotNil(t, result.Failure)
				require.Equal(t, lnwire.CodeFeeInsufficient,
					result.Failure.Code())
			}

			// The fee-blind sender pays the outbound fee alone. A
			// discount means it overpays and clears; a surcharge
			// means it underpays and is refused, and the refusal
			// is attributed to the inbound fee.
			before := graph.PolicyStats().InboundFeeRefusals

			blind := atomicTestRoute(
				t, graph, source, []uint64{1, 2}, deliver,
			)
			require.Equal(t, deliver+testCase.blind,
				blind.TotalAmount)

			result, err = graph.SendHtlc(blind)
			require.NoError(t, err)

			if !testCase.blindRefused {
				require.Nil(t, result.Failure)
				return
			}

			require.NotNil(t, result.Failure)
			require.Equal(t, lnwire.CodeFeeInsufficient,
				result.Failure.Code())
			require.Equal(t, nodeA, result.FailureSource)
			require.Equal(t, before+1,
				graph.PolicyStats().InboundFeeRefusals)
		})
	}
}

// TestInboundFeeEndpointsExempt checks the two hops that charge no inbound fee
// no matter what they announce: the sender, which does not charge itself, and
// the destination, which is the exit hop lnd's path finding zeroes out
// explicitly. Both are announced here at rates that would be impossible to
// miss if they were ever charged.
func TestInboundFeeEndpointsExempt(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, target := nodes[0], nodes[3]
	graph.inboundFees = true

	// The sender announces a surcharge on the channel it sends out over,
	// which is also the channel it would receive over. It never receives
	// on this route, so nothing is charged.
	setInboundFee(t, graph, 1, source, 50_000, 500_000)

	// The destination announces one on the channel it receives the payment
	// over, which is the exit hop.
	setInboundFee(t, graph, 2, target, 50_000, 500_000)

	rt := inboundTestRoute(t, graph, source, []uint64{1, 2}, 50_000)

	// The route is priced as if neither fee existed: the only fee on it is
	// the middle node's outbound one.
	outFee := graph.channels[2].end(nodes[1]).policy.fee(50_000)
	require.Equal(t, 50_000+outFee, rt.TotalAmount)

	result, err := graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())
}

// TestInboundFeeCounters checks that the two wire counters count what their
// declarations promise and nothing else: a priced inbound fee is counted even
// when the htlc clears, and a fee insufficiency the outbound fee alone
// explains is not attributed to the inbound one.
func TestInboundFeeCounters(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, nodeA := nodes[0], nodes[1]
	graph.inboundFees = true

	// A discount, priced and cleared. The mechanism reached the wire and
	// refused nothing, which is the honest steady state.
	setInboundFee(t, graph, 1, nodeA, -500, -100)

	rt := inboundTestRoute(t, graph, source, []uint64{1, 2}, 50_000)
	result, err := graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{InboundFeeCharged: 1},
		graph.PolicyStats())

	// An htlc that underpays the OUTBOUND fee is not the inbound fee's
	// doing, even though an inbound fee was priced on the same hop.
	rt.TotalAmount = 50_000
	rt.Hops[0].AmtToForward = 50_000

	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeFeeInsufficient, result.Failure.Code())
	require.Equal(t, SimPolicyStats{InboundFeeCharged: 2},
		graph.PolicyStats())

	// A node that announces no inbound fee is never counted, however many
	// htlcs cross it.
	setInboundFee(t, graph, 1, nodeA, 0, 0)

	rt = inboundTestRoute(t, graph, source, []uint64{1, 2}, 50_000)
	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{InboundFeeCharged: 2},
		graph.PolicyStats())
}

// TestInboundFeeGossipExposure pins lead decision 2. The sealed view exposes
// an inbound fee in exactly one place, on the channel of the node that charges
// it, and the option lnd's own cache carries on the incoming policy stays
// empty because there it describes the other node entirely.
func TestInboundFeeGossipExposure(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, nodeA := nodes[0], nodes[1]
	graph.inboundFees = true

	// Both parties to channel 1 announce an inbound fee, so a view that
	// picked the wrong end would still find a number and look right.
	setInboundFee(t, graph, 1, source, -11, -22)
	setInboundFee(t, graph, 1, nodeA, -33, -44)

	// look returns the directed channel the given node sees for channel 1.
	look := func(node route.Vertex) *graphdb.DirectedChannel {
		var found *graphdb.DirectedChannel

		require.NoError(t, graph.ForEachNodeDirectedChannel(
			context.Background(), node,
			func(ch *graphdb.DirectedChannel) error {
				if ch.ChannelID == 1 {
					found = ch
				}

				return nil
			}, func() {},
		))
		require.NotNil(t, found)

		return found
	}

	fromSource := look(source)
	require.Equal(t, lnwire.Fee{BaseFee: -11, FeeRate: -22},
		fromSource.InboundFee)
	require.True(t, fromSource.InPolicy.InboundFee.IsNone())

	fromNodeA := look(nodeA)
	require.Equal(t, lnwire.Fee{BaseFee: -33, FeeRate: -44},
		fromNodeA.InboundFee)
	require.True(t, fromNodeA.InPolicy.InboundFee.IsNone())

	// With the mechanism off the view is the one every published number
	// was measured against, whatever the policies carry.
	graph.inboundFees = false
	require.Equal(t, lnwire.Fee{}, look(source).InboundFee)
	require.Equal(t, lnwire.Fee{}, look(nodeA).InboundFee)
}

// TestInboundFeeReachesLndPathfinding checks that the arm this stage was half
// written for actually consumes the data. lnd's path finding reads inbound
// fees off the directed channel through nodeEdgeUnifier.addGraphPolicies, and
// that code has been running against a hardcoded zero for the entire program
// because the sim never set the field.
func TestInboundFeeReachesLndPathfinding(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, nodeA := nodes[0], nodes[1]
	graph.inboundFees = true

	setInboundFee(t, graph, 1, nodeA, -500, -100)

	// The unifier is built for node A the way pathfinding builds it for a
	// pivot that is not the exit hop, and fed from the sealed view.
	unifier := newNodeEdgeUnifier(source, nodeA, true, nil)
	require.NoError(t, unifier.addGraphPolicies(graph))

	edges := unifier.edgeUnifiers[source]
	require.NotNil(t, edges)
	require.Len(t, edges.edges, 1)
	require.Equal(t, models.InboundFee{Base: -500, Rate: -100},
		edges.edges[0].inboundFees)

	// The exit hop is the case lnd zeroes out explicitly, and it still
	// does so here.
	exit := newNodeEdgeUnifier(source, nodeA, false, nil)
	require.NoError(t, exit.addGraphPolicies(graph))
	require.Equal(t, models.InboundFee{},
		exit.edgeUnifiers[source].edges[0].inboundFees)
}

// inboundPair is one directed policy's announced inbound fee.
type inboundPair struct {
	base int32
	rate int32
}

// inboundPairs returns every directed policy's inbound fee, in channel id
// order and node1 end first, which is the order ApplyInboundFees walks.
func inboundPairs(g *SimGraph) []inboundPair {
	ids := make([]uint64, 0, len(g.channels))
	for id := range g.channels {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]inboundPair, 0, 2*len(ids))
	for _, id := range ids {
		channel := g.channels[id]
		for i := range channel.ends {
			policy := channel.ends[i].policy
			out = append(out, inboundPair{
				base: policy.InboundBaseMsat,
				rate: policy.InboundRatePPM,
			})
		}
	}

	return out
}

// TestInboundFeesAbsentGolden is stage B's off-state proof at the generator.
// Every synthetic tier the program has run announces no inbound fee anywhere,
// and a scenario file that omits the section has to keep it that way whichever
// of the three shapes it omits it in.
func TestInboundFeesAbsentGolden(t *testing.T) {
	t.Parallel()

	golden := make([]inboundPair, 32)

	sections := []*SimInboundFeeParams{
		nil,
		{},
		{Seed: 99},
	}
	for _, section := range sections {
		g := htlcLimitsTestGraph(t)
		require.Equal(t, golden, inboundPairs(g))

		require.NoError(t, g.ApplyInboundFees(section, 7))
		require.Equal(t, golden, inboundPairs(g))

		// The mechanism stays off, so nothing is charged at forwarding
		// time and nothing is shown in gossip.
		require.False(t, g.inboundFees)
		require.Equal(t, SimInboundFeeStats{Policies: 32},
			g.InboundFeeStats())
	}

	// A section naming a family the simulator does not know leaves the
	// network entirely alone, mechanism included.
	g := htlcLimitsTestGraph(t)
	err := g.ApplyInboundFees(&SimInboundFeeParams{Family: "lognormal"}, 7)
	require.Error(t, err)
	require.False(t, g.inboundFees)
	require.Equal(t, golden, inboundPairs(g))
}

// TestApplyInboundFeesDeterminism checks that the redraw is a pure function of
// family and seed, and that it does not depend on map iteration order.
func TestApplyInboundFeesDeterminism(t *testing.T) {
	t.Parallel()

	draw := func(family string, seed int64) []inboundPair {
		g := htlcLimitsTestGraph(t)
		require.NoError(t, g.ApplyInboundFees(&SimInboundFeeParams{
			Family: family,
			Seed:   seed,
		}, 0))

		return inboundPairs(g)
	}

	for _, family := range []string{
		InboundFeeFamilyMainnet, InboundFeeFamilyHeavy,
	} {

		first := draw(family, 7)
		require.Equal(t, first, draw(family, 7))
		require.NotEqual(t, first, draw(family, 8))
	}

	// An omitted seed derives one from the scenario's liquidity seed, so a
	// file that pins neither is still reproducible and two files with
	// different liquidity seeds still differ.
	derived := func(liquiditySeed int64) []inboundPair {
		g := htlcLimitsTestGraph(t)
		require.NoError(t, g.ApplyInboundFees(&SimInboundFeeParams{
			Family: InboundFeeFamilyMainnet,
		}, liquiditySeed))

		return inboundPairs(g)
	}
	require.Equal(t, derived(3), derived(3))
	require.NotEqual(t, derived(3), derived(4))
}

// TestApplyInboundFeesLoaded checks the family the mainnet tier runs: the
// mechanism comes on and not one announced value moves, because on a loaded
// snapshot the announced values are the measurement.
func TestApplyInboundFeesLoaded(t *testing.T) {
	t.Parallel()

	g := htlcLimitsTestGraph(t)

	// Stand in for a loaded snapshot by pinning a fee the generator would
	// never produce.
	g.channels[1].ends[0].policy.InboundBaseMsat = -1_000
	g.channels[1].ends[0].policy.InboundRatePPM = -1_006
	before := inboundPairs(g)

	require.NoError(t, g.ApplyInboundFees(&SimInboundFeeParams{
		Family: InboundFeeFamilyLoaded,
		Seed:   7,
	}, 0))

	require.True(t, g.inboundFees)
	require.Equal(t, before, inboundPairs(g))
	require.Equal(t, SimInboundFeeStats{
		Policies:  32,
		Charging:  1,
		Discounts: 1,
	}, g.InboundFeeStats())
}

// TestInboundFeeFamilyMarginals checks that the empirical family reproduces
// the shares it was fitted to. The point of drawing from measured marginals
// rather than an authored shape is that the resulting world is one nobody
// chose, so the numbers below are the whole claim the family makes.
func TestInboundFeeFamilyMarginals(t *testing.T) {
	t.Parallel()

	const numChannels = 20_000

	g := htlcLimitsStarGraph(t, numChannels, 1_000_000)
	require.NoError(t, g.ApplyInboundFees(&SimInboundFeeParams{
		Family: InboundFeeFamilyMainnet,
		Seed:   11,
	}, 0))

	stats := g.InboundFeeStats()
	require.Equal(t, 2*numChannels, stats.Policies)

	// 7.6% of the real graph's directed policies carry an inbound fee.
	share := float64(stats.Charging) / float64(stats.Policies)
	require.InDelta(t, 0.0762, share, 0.004)

	// 97.4% of those are discounts, the rest surcharges. lnd will not even
	// set a positive inbound fee without an explicit opt-in, so a family
	// that got this backwards would be modelling a different network.
	discounts := float64(stats.Discounts) / float64(stats.Charging)
	require.InDelta(t, 0.974, discounts, 0.01)

	// The median discount rate is -200 ppm and the 5th percentile is
	// -2,000, both measured over the policies that announce a rate.
	var rates []int32
	for _, channel := range g.channels {
		for i := range channel.ends {
			rate := channel.ends[i].policy.InboundRatePPM
			if rate < 0 {
				rates = append(rates, rate)
			}
		}
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i] < rates[j] })

	median := rates[len(rates)/2]
	require.InDelta(t, -235, median, 60)

	fifth := rates[len(rates)/20]
	require.InDelta(t, -2000, fifth, 500)
}

// TestInboundFeeFamilyHeavy checks the authored stress rung on the two dials
// it turns and on the one it deliberately leaves alone. Every policy carries a
// fee and every magnitude is multiplied, but the sign split is the measured
// one, so it stays a world of discounts rather than becoming a world of
// surcharges no real sender would meet.
func TestInboundFeeFamilyHeavy(t *testing.T) {
	t.Parallel()

	const numChannels = 5_000

	g := htlcLimitsStarGraph(t, numChannels, 1_000_000)
	require.NoError(t, g.ApplyInboundFees(&SimInboundFeeParams{
		Family: InboundFeeFamilyHeavy,
		Seed:   11,
	}, 0))

	stats := g.InboundFeeStats()
	require.Equal(t, 2*numChannels, stats.Policies)
	require.Equal(t, stats.Policies, stats.Charging)

	discounts := float64(stats.Discounts) / float64(stats.Charging)
	require.InDelta(t, 0.974, discounts, 0.01)

	var rates []int32
	for _, channel := range g.channels {
		for i := range channel.ends {
			rate := channel.ends[i].policy.InboundRatePPM
			if rate < 0 {
				rates = append(rates, rate)
			}
		}
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i] < rates[j] })

	// The median discount lands in the range of the simulator's own
	// synthetic outbound rates, which run 0 to 1000 ppm, so ignoring an
	// inbound fee costs about what ignoring an outbound one would.
	median := rates[len(rates)/2]
	require.InDelta(t, -1175, median, 300)
}
