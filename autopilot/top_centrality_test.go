package autopilot

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// testTopCentrality is subtest helper to which given the passed graph and
// channels creates the expected centrality score set and checks that the
// calculated score set matches it.
func testTopCentrality(t *testing.T, graph testGraph,
	graphNodes map[int]*btcec.PublicKey, channelsWith []int) {

	topCentrality := NewTopCentrality()

	var channels []LocalChannel
	for _, ch := range channelsWith {
		channels = append(channels, LocalChannel{
			Node: NewNodeID(graphNodes[ch]),
		})
	}

	// Start iteration from -1 to also test the case where the node set
	// is empty.
	for i := -1; i < len(graphNodes); i++ {
		nodes := make(map[NodeID]struct{})
		expected := make(map[NodeID]*NodeScore)

		for j := 0; j <= i; j++ {
			// Add node to the interest set.
			nodeID := NewNodeID(graphNodes[j])
			nodes[nodeID] = struct{}{}

			// Add to the expected set unless it's a node we have
			// a channel with.
			haveChannel := false
			for _, ch := range channels {
				if nodeID == ch.Node {
					haveChannel = true
					break
				}
			}

			if !haveChannel {
				score := normalizedTestGraphCentrality[j]
				expected[nodeID] = &NodeScore{
					NodeID: nodeID,
					Score:  score,
				}
			}
		}

		const chanSize = btcutil.SatoshiPerBitcoin

		// Attempt to get centrality scores and expect
		// that the result equals with the expected set.
		scores, err := topCentrality.NodeScores(
			t.Context(), graph, channels, chanSize, nodes,
		)

		require.NoError(t, err)
		require.Equal(t, expected, scores)
	}
}

// TestTopCentrality tests that we return the correct normalized centrality
// values given a non empty graph, and given our node has an increasing amount
// of channels from 0 to N-1 simulating the whole range from non-connected to
// fully connected.
func TestTopCentrality(t *testing.T) {
	// Generate channels: {}, {0}, {0, 1}, ... {0, 1, ..., N-1}
	channelsWith := [][]int{nil}

	for i := 0; i < centralityTestGraph.nodes; i++ {
		channels := make([]int, i+1)
		for j := 0; j <= i; j++ {
			channels[j] = j
		}
		channelsWith = append(channelsWith, channels)
	}

	for _, chanGraph := range chanGraphs {
		chanGraph := chanGraph

		success := t.Run(chanGraph.name, func(t1 *testing.T) {
			t1.Parallel()

			graph, err := chanGraph.genFunc(t1)
			require.NoError(t1, err, "unable to create graph")

			// Build the test graph.
			graphNodes := buildTestGraph(
				t1, graph, centralityTestGraph,
			)

			for _, chans := range channelsWith {
				testTopCentrality(t1, graph, graphNodes, chans)
			}
		})

		require.True(t, success)
	}
}
