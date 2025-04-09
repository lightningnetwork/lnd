package autopilot

import (
	"context"
	"runtime"

	"github.com/btcsuite/btcd/btcutil"
)

// TopCentrality is a simple greedy technique to create connections to nodes
// with the top betweenness centrality value. This algorithm is usually
// referred to as TopK in the literature. The idea is that by opening channels
// to nodes with top betweenness centrality we also increase our own betweenness
// centrality (given we already have at least one channel, or create at least
// two new channels).
// A different and much better approach is instead of selecting nodes with top
// centrality value, we extend the graph in a loop by inserting a new non
// existing edge and recalculate the betweenness centrality of each node. This
// technique is usually referred to as "greedy" algorithm and gives better
// results than TopK but is considerably slower too.
type TopCentrality struct {
	centralityMetric *BetweennessCentrality
}

// A compile time assertion to ensure TopCentrality meets the
// AttachmentHeuristic interface.
var _ AttachmentHeuristic = (*TopCentrality)(nil)

// NewTopCentrality constructs and returns a new TopCentrality heuristic.
func NewTopCentrality() *TopCentrality {
	metric, err := NewBetweennessCentralityMetric(
		runtime.NumCPU(),
	)
	if err != nil {
		panic(err)
	}

	return &TopCentrality{
		centralityMetric: metric,
	}
}

// Name returns the name of the heuristic.
func (g *TopCentrality) Name() string {
	return "top_centrality"
}

// NodeScores will return a [0,1] normalized map of scores for the given nodes
// except for the ones we already have channels with. The scores will simply
// be the betweenness centrality values of the nodes.
// As our current implementation of betweenness centrality is non-incremental,
// NodeScores will recalculate the centrality values on every call, which is
// slow for large graphs.
func (g *TopCentrality) NodeScores(ctx context.Context, graph ChannelGraph,
	chans []LocalChannel, chanSize btcutil.Amount,
	nodes map[NodeID]struct{}) (map[NodeID]*NodeScore, error) {

	// Calculate betweenness centrality for the whole graph.
	if err := g.centralityMetric.Refresh(ctx, graph); err != nil {
		return nil, err
	}

	normalize := true
	centrality := g.centralityMetric.GetMetric(normalize)

	// Create a map of the existing peers for faster filtering.
	existingPeers := make(map[NodeID]struct{})
	for _, c := range chans {
		existingPeers[c.Node] = struct{}{}
	}

	result := make(map[NodeID]*NodeScore, len(nodes))
	for nodeID := range nodes {
		// Skip nodes we already have channel with.
		if _, ok := existingPeers[nodeID]; ok {
			continue
		}

		// Skip passed nodes not in the graph. This could happen if
		// the graph changed before computing the centrality values as
		// the nodes we iterate are prefiltered by the autopilot agent.
		score, ok := centrality[nodeID]
		if !ok {
			continue
		}

		result[nodeID] = &NodeScore{
			NodeID: nodeID,
			Score:  score,
		}
	}

	return result, nil
}
