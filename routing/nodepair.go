package routing

import (
	"github.com/lightningnetwork/lnd/routing/route"
)

// DirectedNodePair stores a directed pair of nodes.
type DirectedNodePair struct {
	From, To route.Vertex
}

// NewDirectedNodePair instantiates a new DirectedNodePair struct.
func NewDirectedNodePair(from, to route.Vertex) DirectedNodePair {
	return DirectedNodePair{
		From: from,
		To:   to,
	}
}
