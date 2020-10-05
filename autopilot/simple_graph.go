package autopilot

// SimpleGraph stores a simplifed adj graph of a channel graph to speed
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
func NewSimpleGraph(g ChannelGraph) (*SimpleGraph, error) {
	nodes := make(map[NodeID]int)
	adj := make(map[int][]int)
	nextIndex := 0

	// getNodeIndex returns the integer index of the passed node.
	// The returned index is then used to create a simplifed adjacency list
	// where each node is identified by its index instead of its pubkey, and
	// also to create a mapping from node index to node pubkey.
	getNodeIndex := func(node Node) int {
		key := NodeID(node.PubKey())
		nodeIndex, ok := nodes[key]

		if !ok {
			nodes[key] = nextIndex
			nodeIndex = nextIndex
			nextIndex++
		}

		return nodeIndex
	}

	// Iterate over each node and each channel and update the adj and the node
	// index.
	err := g.ForEachNode(func(node Node) error {
		u := getNodeIndex(node)

		return node.ForEachChannel(func(edge ChannelEdge) error {
			v := getNodeIndex(edge.Peer)

			adj[u] = append(adj[u], v)
			return nil
		})
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

	return graph, nil
}
