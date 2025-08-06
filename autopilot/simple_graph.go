package autopilot

import (
	"context"

	"github.com/lightningnetwork/lnd/routing/route"
)

// diameterCutoff is used to discard nodes in the diameter calculation.
// It is the multiplier for the eccentricity of the highest-degree node,
// serving as a cutoff to discard all nodes with a smaller hop distance. This
// number should not be set close to 1 and is a tradeoff for computation cost,
// where 0 is maximally costly.
const diameterCutoff = 0.75

// SimpleGraph stores a simplified adj graph of a channel graph to speed
// up graph processing by eliminating all unnecessary hashing and map access.
type SimpleGraph struct {
	// Nodes is a map from node index to NodeID.
	Nodes []NodeID

	// Adj stores nodes and neighbors in an adjacency list.
	Adj [][]int
}

// NewSimpleGraph creates a simplified graph from the current channel graph.
// Returns an error if the channel graph iteration fails due to underlying
// failure.
func NewSimpleGraph(ctx context.Context, g ChannelGraph) (*SimpleGraph, error) {
	nodes := make(map[NodeID]int)
	adj := make(map[int][]int)
	nextIndex := 0

	// getNodeIndex returns the integer index of the passed node.
	// The returned index is then used to create a simplified adjacency list
	// where each node is identified by its index instead of its pubkey, and
	// also to create a mapping from node index to node pubkey.
	getNodeIndex := func(node route.Vertex) int {
		key := NodeID(node)
		nodeIndex, ok := nodes[key]

		if !ok {
			nodes[key] = nextIndex
			nodeIndex = nextIndex
			nextIndex++
		}

		return nodeIndex
	}

	// Iterate over each node and each channel and update the adj and the
	// node index.
	err := g.ForEachNodesChannels(ctx, func(_ context.Context,
		node Node, channels []*ChannelEdge) error {

		u := getNodeIndex(node.PubKey())

		for _, edge := range channels {
			v := getNodeIndex(edge.Peer)
			adj[u] = append(adj[u], v)
		}

		return nil
	}, func() {
		clear(adj)
		clear(nodes)
		nextIndex = 0
	})
	if err != nil {
		return nil, err
	}

	graph := &SimpleGraph{
		Nodes: make([]NodeID, len(nodes)),
		Adj:   make([][]int, len(nodes)),
	}

	// Fill the adj and the node index to node pubkey mapping.
	for nodeID, nodeIndex := range nodes {
		graph.Adj[nodeIndex] = adj[nodeIndex]
		graph.Nodes[nodeIndex] = nodeID
	}

	// We prepare to give some debug output about the size of the graph.
	totalChannels := 0
	for _, channels := range graph.Adj {
		totalChannels += len(channels)
	}

	// The number of channels is double counted, so divide by two.
	log.Debugf("Initialized simple graph with %d nodes and %d "+
		"channels", len(graph.Adj), totalChannels/2)
	return graph, nil
}

// maxVal is a helper function to get the maximal value of all values of a map.
func maxVal(mapping map[int]uint32) uint32 {
	maxValue := uint32(0)
	for _, value := range mapping {
		maxValue = max(maxValue, value)
	}
	return maxValue
}

// degree determines the number of edges for a node in the graph.
func (graph *SimpleGraph) degree(node int) int {
	return len(graph.Adj[node])
}

// nodeMaxDegree determines the node with the max degree and its degree.
func (graph *SimpleGraph) nodeMaxDegree() (int, int) {
	var maxNode, maxDegree int
	for node := range graph.Adj {
		degree := graph.degree(node)
		if degree > maxDegree {
			maxNode = node
			maxDegree = degree
		}
	}
	return maxNode, maxDegree
}

// shortestPathLengths performs a breadth-first-search from a node to all other
// nodes, returning the lengths of the paths.
func (graph *SimpleGraph) shortestPathLengths(node int) map[int]uint32 {
	// level indicates the shell of the search around the root node.
	var level uint32
	graphOrder := len(graph.Adj)

	// nextLevel tracks which nodes should be visited in the next round.
	nextLevel := make([]int, 0, graphOrder)

	// The root node is put as a starting point for the exploration.
	nextLevel = append(nextLevel, node)

	// Seen tracks already visited nodes and tracks how far away they are.
	seen := make(map[int]uint32, graphOrder)

	// Mark the root node as seen.
	seen[node] = level

	// thisLevel contains the nodes that are explored in the round.
	thisLevel := make([]int, 0, graphOrder)

	// Abort if we have an empty graph.
	if len(graph.Adj) == 0 {
		return seen
	}

	// We discover other nodes in a ring-like structure as long as we don't
	// have more nodes to explore.
	for len(nextLevel) > 0 {
		level++

		// We swap the queues for efficient memory management.
		thisLevel, nextLevel = nextLevel, thisLevel

		// Visit all neighboring nodes of the level and mark them as
		// seen if they were not discovered before.
		for _, thisNode := range thisLevel {
			for _, neighbor := range graph.Adj[thisNode] {
				_, ok := seen[neighbor]
				if !ok {
					nextLevel = append(nextLevel, neighbor)
					seen[neighbor] = level
				}

				// If we have seen all nodes, we return early.
				if len(seen) == graphOrder {
					return seen
				}
			}
		}

		// Empty the queue to be used in the next level.
		thisLevel = thisLevel[:0:cap(thisLevel)]
	}

	return seen
}

// nodeEccentricity calculates the eccentricity (longest shortest path to all
// other nodes) of a node.
func (graph *SimpleGraph) nodeEccentricity(node int) uint32 {
	pathLengths := graph.shortestPathLengths(node)
	return maxVal(pathLengths)
}

// nodeEccentricities calculates the eccentricities for the given nodes.
func (graph *SimpleGraph) nodeEccentricities(nodes []int) map[int]uint32 {
	eccentricities := make(map[int]uint32, len(graph.Adj))
	for _, node := range nodes {
		eccentricities[node] = graph.nodeEccentricity(node)
	}
	return eccentricities
}

// Diameter returns the maximal eccentricity (longest shortest path
// between any node pair) in the graph.
//
// Note: This method is exact but expensive, use DiameterRadialCutoff instead.
func (graph *SimpleGraph) Diameter() uint32 {
	nodes := make([]int, len(graph.Adj))
	for a := range nodes {
		nodes[a] = a
	}
	eccentricities := graph.nodeEccentricities(nodes)
	return maxVal(eccentricities)
}

// DiameterRadialCutoff is a method to efficiently evaluate the diameter of a
// graph. The highest-degree node is usually central in the graph. We can
// determine its eccentricity (shortest-longest path length to any other node)
// and use it as an approximation for the radius of the network. We then
// use this radius to compute a cutoff. All the nodes within a distance of the
// cutoff are discarded, as they represent the inside of the graph. We then
// loop over all outer nodes and determine their eccentricities, from which we
// get the diameter.
func (graph *SimpleGraph) DiameterRadialCutoff() uint32 {
	// Determine the reference node as the node with the highest degree.
	nodeMaxDegree, _ := graph.nodeMaxDegree()

	distances := graph.shortestPathLengths(nodeMaxDegree)
	eccentricityMaxDegreeNode := maxVal(distances)

	// We use the eccentricity to define a cutoff for the interior of the
	// graph from the reference node.
	cutoff := uint32(float32(eccentricityMaxDegreeNode) * diameterCutoff)
	log.Debugf("Cutoff radius is %d hops (max-degree node's "+
		"eccentricity is %d)", cutoff, eccentricityMaxDegreeNode)

	// Remove the nodes that are close to the reference node.
	var nodes []int
	for node, distance := range distances {
		if distance > cutoff {
			nodes = append(nodes, node)
		}
	}
	log.Debugf("Evaluated nodes: %d, discarded nodes %d",
		len(nodes), len(graph.Adj)-len(nodes))

	// Compute the diameter of the remaining nodes.
	eccentricities := graph.nodeEccentricities(nodes)
	return maxVal(eccentricities)
}
