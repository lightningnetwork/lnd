package routing

import (
	"fmt"

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

// String converts a node pair to its human readable representation.
func (d DirectedNodePair) String() string {
	return fmt.Sprintf("%v -> %v", d.From, d.To)
}

// Reverse returns a reversed copy of the pair.
func (d DirectedNodePair) Reverse() DirectedNodePair {
	return DirectedNodePair{From: d.To, To: d.From}
}
