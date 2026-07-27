package routing

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// importingRouter wraps a scripted router with the optional importer half of
// the contract, standing in for a candidate that can consume served weights.
type importingRouter struct {
	scriptedRouter

	imported []SimObservation
}

// ImportObservations records what was served.
//
// NOTE: Part of the SimObservationImporter interface.
func (i *importingRouter) ImportObservations(obs []SimObservation) error {
	i.imported = append(i.imported, obs...)

	return nil
}

// weightsTestRunner builds a runner over a well-connected graph with the given
// routing strategy.
func weightsTestRunner(t *testing.T, graph *SimGraph, source route.Vertex,
	factory SimRouterFactory) *SimRunner {

	t.Helper()

	runner, err := NewSimRunner(graph, DefaultSimParams(), source, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(runner.Close)

	if factory != nil {
		runner.SetRouterFactory(factory)
	}

	return runner
}

// sortedNodes returns the graph's nodes in a deterministic order.
func sortedNodes(g *SimGraph) []route.Vertex {
	nodes := make([]route.Vertex, 0, len(g.nodes))
	for v := range g.nodes {
		nodes = append(nodes, v)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].String() < nodes[j].String()
	})

	return nodes
}

// TestObservationsFromAttempt asserts the asymmetry that makes an attempt
// informative: every hop before the failure is proven to have carried its
// amount, and only the failing hop is proven to have refused.
func TestObservationsFromAttempt(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)
	source := nodes[0]

	runner := weightsTestRunner(t, graph, source, nil)

	// Drive a real payment so the observations come from a genuine route
	// rather than a hand-built one.
	_, err := runner.RunScenario(&SimScenario{
		Target:  nodes[len(nodes)-1].String(),
		AmtMsat: 1_000_000,
	})
	require.NoError(t, err)

	obs := runner.Observations()
	require.NotEmpty(t, obs, "a completed payment recorded nothing")

	for _, o := range obs {
		require.NotEqual(t, o.From, o.To)
		require.Positive(t, o.AmtMsat)
		require.Positive(t, o.TimeUnix)
	}
}

// TestObservationsRoundTrip asserts that the served format survives a write
// and read unchanged, since the file is the API surface.
func TestObservationsRoundTrip(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)
	runner := weightsTestRunner(t, graph, nodes[0], nil)

	_, err := runner.RunScenario(&SimScenario{
		Target:  nodes[len(nodes)-1].String(),
		AmtMsat: 1_000_000,
	})
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "weights.json")
	require.NoError(t, WriteObservations(path, runner.Observations()))

	loaded, err := ReadObservations(path)
	require.NoError(t, err)
	require.Equal(t, runner.Observations(), loaded)
}

// TestImportExcludesLocalChannels asserts the policy exp-012 part 4 measured:
// remote-pair observations transfer, and observations about the consumer's
// own channels must not be imported.
func TestImportExcludesLocalChannels(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)
	source := nodes[0]

	// Build one observation about a channel the source owns, and one
	// about a channel it does not.
	var localObs, remoteObs *SimObservation
	for id, channel := range graph.channels {
		o := SimObservation{
			From:     channel.ends[0].owner,
			To:       channel.ends[1].owner,
			ChanID:   id,
			AmtMsat:  500_000,
			Success:  true,
			TimeUnix: 1000,
		}

		isLocal := channel.ends[0].owner == source ||
			channel.ends[1].owner == source

		if isLocal && localObs == nil {
			copied := o
			localObs = &copied
		}
		if !isLocal && remoteObs == nil {
			copied := o
			remoteObs = &copied
		}
	}
	require.NotNil(t, localObs, "graph has no local channel")
	require.NotNil(t, remoteObs, "graph has no remote channel")

	runner := weightsTestRunner(t, graph, source, nil)

	stats, err := runner.importWithStats(
		[]SimObservation{*localObs, *remoteObs},
		SimImportPolicy{ExcludeLocal: true},
	)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Offered)
	require.Equal(t, 1, stats.Accepted)
	require.Equal(t, 1, stats.DroppedLocal)

	// With the policy off, both land — the arm that reproduces the
	// measured failure deliberately.
	runner2 := weightsTestRunner(t, graph, source, nil)
	stats2, err := runner2.importWithStats(
		[]SimObservation{*localObs, *remoteObs},
		SimImportPolicy{ExcludeLocal: false},
	)
	require.NoError(t, err)
	require.Equal(t, 2, stats2.Accepted)
	require.Zero(t, stats2.DroppedLocal)
}

// TestImportReachesMissionControl asserts that served observations become real
// lnd history without a single payment being sent, which is the whole point:
// knowledge separated from the cost of acquiring it.
func TestImportReachesMissionControl(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)
	source := nodes[0]

	// Pick a remote pair so the default policy keeps it.
	var obs SimObservation
	for id, channel := range graph.channels {
		a, b := channel.ends[0].owner, channel.ends[1].owner
		if a == source || b == source {
			continue
		}

		obs = SimObservation{
			From: a, To: b, ChanID: id,
			AmtMsat: 750_000, Success: false, TimeUnix: 1000,
		}

		break
	}
	require.NotZero(t, obs.ChanID)

	runner := weightsTestRunner(t, graph, source, nil)

	before := runner.mc.GetHistorySnapshot()
	require.Empty(t, before.Pairs, "history was not clean")

	require.NoError(t, runner.ImportObservations([]SimObservation{obs}))

	after := runner.mc.GetHistorySnapshot()
	require.Len(t, after.Pairs, 1, "served observation did not reach "+
		"mission control")
	require.Equal(t, lnwire.MilliSatoshi(obs.AmtMsat),
		after.Pairs[0].FailAmt)
}

// TestImportReachesCandidateRouter asserts the candidate half of the contract:
// a router that implements the optional importer is handed the observations
// before its first route request, and only once.
func TestImportReachesCandidateRouter(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)
	source := nodes[0]

	router := &importingRouter{}
	runner := weightsTestRunner(t, graph, source,
		func(_ SimNetworkView, _ route.Vertex,
			_ map[uint64]lnwire.MilliSatoshi,
			_ *SimPaymentSpec) (SimRouter, error) {

			return router, nil
		},
	)

	require.True(t, runner.RouterAcceptsImports())

	var obs SimObservation
	for id, channel := range graph.channels {
		a, b := channel.ends[0].owner, channel.ends[1].owner
		if a == source || b == source {
			continue
		}
		obs = SimObservation{
			From: a, To: b, ChanID: id,
			AmtMsat: 750_000, Success: true, TimeUnix: 1000,
		}

		break
	}

	require.NoError(t, runner.ImportObservations([]SimObservation{obs}))

	// Two payments: the evidence must be delivered once, not once per
	// payment, or an interval router would double-count it.
	for i := 0; i < 2; i++ {
		_, err := runner.RunScenario(&SimScenario{
			Target:  nodes[len(nodes)-1].String(),
			AmtMsat: 1_000_000,
		})
		require.NoError(t, err)
	}

	require.Len(t, router.imported, 1,
		"served evidence was delivered %d times, not once",
		len(router.imported))
	require.Equal(t, obs, router.imported[0])
}

// TestRouterAcceptsImportsFalseForPlainRouter asserts that a router without
// the optional half is reported as such, so a sweep can tell an ineffective
// import from an undelivered one.
func TestRouterAcceptsImportsFalseForPlainRouter(t *testing.T) {
	t.Parallel()

	graph := trafficTestGraph(t, 3)
	nodes := sortedNodes(graph)

	runner := weightsTestRunner(t, graph, nodes[0],
		func(_ SimNetworkView, _ route.Vertex,
			_ map[uint64]lnwire.MilliSatoshi,
			_ *SimPaymentSpec) (SimRouter, error) {

			return &scriptedRouter{}, nil
		},
	)

	require.False(t, runner.RouterAcceptsImports())
}
