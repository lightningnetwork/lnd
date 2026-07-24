package routing

import (
	"context"
	"testing"

	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/stretchr/testify/require"
)

// TestSimGraphPayment exercises the full simulation loop: generate a
// synthetic topology, assign liquidity, and route payments with the real
// path finding and mission control stack.
func TestSimGraphPayment(t *testing.T) {
	t.Parallel()

	graph, err := GenerateSimGraph(&SimTopologySpec{
		Type:           "smallworld",
		NumNodes:       50,
		ChannelSizeSat: 1_000_000,
		Seed:           42,
		AvgDegree:      6,
	})
	require.NoError(t, err)

	require.NoError(t, graph.AssignLiquidity(LiquidityUniform, 7))

	source, err := graph.ResolveNode("1")
	require.NoError(t, err)

	runner, err := NewSimRunner(
		graph, DefaultSimParams(), source, t.TempDir(),
	)
	require.NoError(t, err)
	defer runner.Close()

	// Pay several targets in sequence; mission control accumulates
	// knowledge across payments.
	var successes int
	for _, target := range []string{"10", "20", "30", "40"} {
		result, err := runner.RunScenario(&SimScenario{
			Target:   target,
			AmtMsat:  50_000_000,
			MaxParts: 4,
		})
		require.NoError(t, err)
		require.NotEmpty(t, result.Attempts)

		if result.Success {
			successes++
		}
	}

	// With 50 well-connected nodes and 1M sat channels, at least one of
	// the four 50k sat payments must complete.
	require.Greater(t, successes, 0)
}

// TestSimViewSealed is the sandbox regression test: a candidate router must
// not be able to recover the concrete *SimGraph (and with it the hidden
// balances and liquidity mutators) from any surface of the SimNetworkView
// it receives.
func TestSimViewSealed(t *testing.T) {
	t.Parallel()

	graph, err := GenerateSimGraph(&SimTopologySpec{
		Type:           "line",
		NumNodes:       3,
		ChannelSizeSat: 100_000,
		Seed:           1,
	})
	require.NoError(t, err)

	view := &simGossipView{g: graph}

	// The view itself must not be the concrete graph.
	var asAny any = view
	_, leaked := asAny.(*SimGraph)
	require.False(t, leaked, "view is the concrete graph")

	// GraphSession must hand the callback the sealed view, never the
	// underlying *SimGraph. This was a live escape: candidates could
	// type-assert the session graph back to *SimGraph and read hidden
	// balances or rewrite liquidity.
	err = view.GraphSession(
		context.Background(),
		func(sessionGraph graphdb.NodeTraverser) error {
			_, leaked := sessionGraph.(*SimGraph)
			require.False(
				t, leaked,
				"GraphSession leaks the concrete graph",
			)
			return nil
		}, func() {},
	)
	require.NoError(t, err)
}

// TestSimGraphDeterminism asserts that the same seed produces identical
// scenario outcomes.
func TestSimGraphDeterminism(t *testing.T) {
	t.Parallel()

	run := func() *SimScenarioResult {
		graph, err := GenerateSimGraph(&SimTopologySpec{
			Type:           "hubspoke",
			NumNodes:       30,
			ChannelSizeSat: 500_000,
			Seed:           1,
		})
		require.NoError(t, err)

		require.NoError(t, graph.AssignLiquidity(LiquidityBimodal, 3))

		source, err := graph.ResolveNode("5")
		require.NoError(t, err)

		runner, err := NewSimRunner(
			graph, DefaultSimParams(), source, t.TempDir(),
		)
		require.NoError(t, err)
		defer runner.Close()

		result, err := runner.RunScenario(&SimScenario{
			Target:   "25",
			AmtMsat:  10_000_000,
			MaxParts: 2,
		})
		require.NoError(t, err)

		return result
	}

	first := run()
	second := run()

	require.Equal(t, first.Success, second.Success)
	require.Equal(t, len(first.Attempts), len(second.Attempts))
	require.Equal(t, first.FeeMsat, second.FeeMsat)
}
