package routing

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Graph is an abstract interface that provides information about nodes and
// edges to pathfinding.
type Graph interface {
	// ForEachNodeDirectedChannel calls the callback for every channel of
	// the given node.
	ForEachNodeDirectedChannel(nodePub route.Vertex,
		cb func(channel *graphdb.DirectedChannel) error,
		reset func()) error

	// FetchNodeFeatures returns the features of the given node.
	FetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}

// GraphSessionFactory can be used to gain access to a graphdb.NodeTraverser
// instance which can then be used for a path-finding session. Depending on the
// implementation, the session will represent a DB connection where a read-lock
// is being held across calls to the backing graph.
type GraphSessionFactory interface {
	// GraphSession will provide the call-back with access to a
	// graphdb.NodeTraverser instance which can be used to perform queries
	// against the channel graph.
	GraphSession(cb func(graph graphdb.NodeTraverser) error,
		reset func()) error
}

// FetchAmountPairCapacity determines the maximal public capacity between two
// nodes depending on the amount we try to send.
func FetchAmountPairCapacity(graph Graph, source, nodeFrom, nodeTo route.Vertex,
	amount lnwire.MilliSatoshi) (btcutil.Amount, error) {

	// Create unified edges for all incoming connections.
	//
	// Note: Inbound fees are not used here because this method is only used
	// by a deprecated router rpc.
	u := newNodeEdgeUnifier(source, nodeTo, false, nil)

	err := u.addGraphPolicies(graph)
	if err != nil {
		return 0, err
	}

	edgeUnifier, ok := u.edgeUnifiers[nodeFrom]
	if !ok {
		return 0, fmt.Errorf("no edge info for node pair %v -> %v",
			nodeFrom, nodeTo)
	}

	edge := edgeUnifier.getEdgeNetwork(amount, 0)
	if edge == nil {
		return 0, fmt.Errorf("no edge for node pair %v -> %v "+
			"(amount %v)", nodeFrom, nodeTo, amount)
	}

	return edge.capacity, nil
}
