package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// setPolicyLimits pins the announced htlc limits of one end of a channel,
// leaving its fees and delta alone.
func setPolicyLimits(t *testing.T, g *SimGraph, chanID uint64,
	owner route.Vertex, minHTLC, maxHTLC lnwire.MilliSatoshi) {

	t.Helper()

	channel, ok := g.channels[chanID]
	require.True(t, ok, "unknown channel %d", chanID)

	end := channel.end(owner)
	require.NotNil(t, end, "node is not a party to channel %d", chanID)

	end.policy.MinHTLCMsat = minHTLC
	end.policy.MaxHTLCMsat = maxHTLC
}

// TestPolicyRefusalCounters checks that the forwarding refusals an announced
// limit causes are counted, split by which limit caused them, and that
// nothing else is counted with them. A ceiling violation is returned as a
// plain temporary channel failure, indistinguishable on the wire from a
// depleted channel, so the counter is the only way a sweep can tell that a
// tier's ceilings ever bound.
func TestPolicyRefusalCounters(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source, nodeA := nodes[0], nodes[1]

	// A ceiling on the far side of the route: node A refuses to forward
	// onto channel 2 anything above 10k msat.
	setPolicyLimits(t, graph, 2, nodeA, 0, 10_000)

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, 50_000)
	result, err := graph.SendHtlc(rt)
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.Equal(t, nodeA, result.FailureSource)
	require.Equal(t, lnwire.CodeTemporaryChannelFailure,
		result.Failure.Code())

	require.Equal(t, SimPolicyStats{MaxHtlcRefusals: 1},
		graph.PolicyStats())

	// An amount under the ceiling clears it and is not counted.
	rt = atomicTestRoute(t, graph, source, []uint64{1, 2}, 5_000)
	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{MaxHtlcRefusals: 1},
		graph.PolicyStats())

	// A floor on the same end: anything under 100k msat is refused, and
	// this one says on the wire what it is.
	setPolicyLimits(t, graph, 2, nodeA, 100_000, 0)

	rt = atomicTestRoute(t, graph, source, []uint64{1, 2}, 5_000)
	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeAmountBelowMinimum, result.Failure.Code())

	require.Equal(t, SimPolicyStats{
		MinHtlcRefusals: 1,
		MaxHtlcRefusals: 1,
	}, graph.PolicyStats())

	// A depleted channel fails with the same code the ceiling used, and
	// must not be counted as a policy refusal: telling the two apart is
	// the entire purpose of these counters.
	setPolicyLimits(t, graph, 2, nodeA, 0, 0)
	atomicSetBalance(t, graph, 2, nodeA, 0)

	rt = atomicTestRoute(t, graph, source, []uint64{1, 2}, 5_000)
	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeTemporaryChannelFailure,
		result.Failure.Code())

	require.Equal(t, SimPolicyStats{
		MinHtlcRefusals: 1,
		MaxHtlcRefusals: 1,
	}, graph.PolicyStats())

	// Nor is a disabled direction, which is a different refusal entirely.
	atomicSetBalance(t, graph, 2, nodeA, lnwire.NewMSatFromSatoshis(
		atomicChanCapSat/2,
	))
	graph.channels[2].end(nodeA).policy.Disabled = true

	rt = atomicTestRoute(t, graph, source, []uint64{1, 2}, 5_000)
	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeChannelDisabled, result.Failure.Code())

	require.Equal(t, SimPolicyStats{
		MinHtlcRefusals: 1,
		MaxHtlcRefusals: 1,
	}, graph.PolicyStats())

	// Nothing above happened at the sender's own first hop, which is the
	// one refusal site the stage A flag opens.
	require.Zero(t, graph.PolicyStats().SourceRefusals)
}

// TestPolicyRefusalCountersQuietByDefault checks that a network with the
// limits every generated tier has always carried refuses nothing, so the
// counters stay at zero and the aggregate they feed keeps omitting them.
func TestPolicyRefusalCountersQuietByDefault(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source := nodes[0]

	// The generator's constants: a 1000 msat floor and no ceiling.
	for _, chanID := range []uint64{1, 2, 3, 4} {
		channel := graph.channels[chanID]
		for i := range channel.ends {
			channel.ends[i].policy.MinHTLCMsat = 1_000
			channel.ends[i].policy.MaxHTLCMsat = 0
		}
	}

	for _, amt := range []lnwire.MilliSatoshi{
		1_000, 50_000, 1_000_000, 100_000_000,
	} {

		rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, amt)
		_, err := graph.SendHtlc(rt)
		require.NoError(t, err)
	}

	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())
}

// TestSourceLimitEnforcement pins both sides of stage A's lead decision: with
// the flag off the sender is exempt from its own announced limits, which is
// how every result before stage A was measured, and with it on those limits
// bind on the first hop exactly like every other hop's.
func TestSourceLimitEnforcement(t *testing.T) {
	t.Parallel()

	// send builds a route over the sender's own channel 1 and reports what
	// the network did with it.
	send := func(t *testing.T, enforce bool, minHTLC,
		maxHTLC lnwire.MilliSatoshi,
		amt lnwire.MilliSatoshi) (*SimGraph, SimHtlcResult) {

		t.Helper()

		graph, nodes := atomicTestGraph(t)
		source := nodes[0]

		setPolicyLimits(t, graph, 1, source, minHTLC, maxHTLC)
		graph.enforceSourceLimits = enforce

		rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, amt)
		result, err := graph.SendHtlc(rt)
		require.NoError(t, err)

		return graph, result
	}

	// A ceiling on the sender's own channel, with the flag off: carried,
	// which is the behavior every published number was measured on.
	graph, result := send(t, false, 0, 10_000, 50_000)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())

	// The same ceiling with the flag on: refused, blamed on the sender
	// itself, and counted as a source refusal.
	graph, result = send(t, true, 0, 10_000, 50_000)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeTemporaryChannelFailure,
		result.Failure.Code())

	sender := SimNodePubKey(1)
	require.Equal(t, sender, result.FailureSource)
	require.Equal(t, SimPolicyStats{
		MaxHtlcRefusals: 1,
		SourceRefusals:  1,
	}, graph.PolicyStats())

	// A floor behaves the same way, and says on the wire what it is.
	graph, result = send(t, false, 100_000, 0, 5_000)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())

	graph, result = send(t, true, 100_000, 0, 5_000)
	require.NotNil(t, result.Failure)
	require.Equal(t, lnwire.CodeAmountBelowMinimum, result.Failure.Code())
	require.Equal(t, SimPolicyStats{
		MinHtlcRefusals: 1,
		SourceRefusals:  1,
	}, graph.PolicyStats())

	// An amount inside both limits clears the first hop with the flag on,
	// so the rule binds at the margin rather than everywhere.
	graph, result = send(t, true, 1_000, 100_000, 50_000)
	require.Nil(t, result.Failure)
	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())
}

// TestSourceLimitEnforcementLimitsOnly checks the scope of the uniform rule:
// it is the announced htlc LIMITS that bind on the first hop, not the whole
// policy. A sender neither pays itself a fee nor grants itself a timelock
// delta, so those two checks are unsatisfiable at hop zero by construction,
// and lnd's own local edge selection ignores the disabled flag as well.
func TestSourceLimitEnforcementLimitsOnly(t *testing.T) {
	t.Parallel()

	graph, nodes := atomicTestGraph(t)
	source := nodes[0]

	graph.enforceSourceLimits = true

	// atomicTestPolicy charges a base fee, a rate and a 40 block delta on
	// every end, including the sender's. If the whole policy bound at hop
	// zero, no route the sender ever builds could clear it.
	require.NotZero(t, graph.channels[1].end(source).policy.BaseFeeMsat)
	require.NotZero(t, graph.channels[1].end(source).policy.TimeLockDelta)

	rt := atomicTestRoute(t, graph, source, []uint64{1, 2}, 50_000)
	result, err := graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)

	// A disabled own channel is likewise not the sender's problem: lnd's
	// getEdgeLocal says so in as many words.
	graph.channels[1].end(source).policy.Disabled = true

	rt = atomicTestRoute(t, graph, source, []uint64{1, 2}, 50_000)
	result, err = graph.SendHtlc(rt)
	require.NoError(t, err)
	require.Nil(t, result.Failure)

	require.Equal(t, SimPolicyStats{}, graph.PolicyStats())
}

// TestApplyHtlcLimitsEnablesSourceEnforcement checks that the flag rides with
// the section: a scenario file that asks for drawn limits gets the uniform
// rule, and one that omits the section keeps the source exemption every
// earlier corpus was measured with.
func TestApplyHtlcLimitsEnablesSourceEnforcement(t *testing.T) {
	t.Parallel()

	graph, _ := atomicTestGraph(t)
	require.False(t, graph.enforceSourceLimits)

	require.NoError(t, graph.ApplyHtlcLimits(nil, 1))
	require.False(t, graph.enforceSourceLimits)

	require.NoError(t, graph.ApplyHtlcLimits(&SimHtlcLimitsParams{}, 1))
	require.False(t, graph.enforceSourceLimits)

	// A section that names a family the simulator does not know leaves the
	// network entirely alone, enforcement included.
	err := graph.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: "gaussian",
	}, 1)
	require.Error(t, err)
	require.False(t, graph.enforceSourceLimits)

	require.NoError(t, graph.ApplyHtlcLimits(&SimHtlcLimitsParams{
		MaxHtlcFracFamily: HtlcLimitFamilyMainnet,
		MinHtlcFamily:     HtlcLimitFamilyMainnet,
	}, 1))
	require.True(t, graph.enforceSourceLimits)
}
