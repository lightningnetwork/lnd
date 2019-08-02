package routing

import (
	"bytes"

	"github.com/lightningnetwork/lnd/routing/route"
)

// DirectedNodePair stores a directed pair of nodes.
type DirectedNodePair struct {
	From, To route.Vertex
}

// NodePair stores an undirected pair of nodes.
type NodePair struct {
	A, B route.Vertex
}

// newNodePair instantiates a new nodePair struct. It makes sure that node a is
// the node with the lower pubkey. A second return parameters indicates whether
// the given node ordering is reversed or not.
func newNodePair(a, b route.Vertex) NodePair {
	if bytes.Compare(a[:], b[:]) == 1 {
		return NodePair{
			A: b,
			B: a,
		}
	}

	return NodePair{
		A: a,
		B: b,
	}
}
