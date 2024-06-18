package routing

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// routingGraph is an abstract interface that provides information about nodes
// and edges to pathfinding.
type routingGraph interface {
	// forEachNodeChannel calls the callback for every channel of the given
	// node.
	forEachNodeChannel(nodePub route.Vertex,
		cb func(channel *channeldb.DirectedChannel) error) error

	// fetchNodeFeatures returns the features of the given node.
	fetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}

// GraphWithReadLock is a Graph extended with a call to create a new read-only
// transaction that can then be used to make further queries to the Graph.
type GraphWithReadLock interface {
	// NewPathFindTx returns a new read transaction that can be used for a
	// single path finding session. Will return nil if the graph cache is
	// enabled.
	NewPathFindTx() (kvdb.RTx, error)

	Graph
}

// Graph describes the API necessary for a graph source to have in order to be
// used by the Router for pathfinding.
type Graph interface {
	// ForEachNodeDirectedChannel iterates through all channels of a given
	// node, executing the passed callback on the directed edge representing
	// the channel and its incoming policy. If the callback returns an
	// error, then the iteration is halted with the error propagated back
	// up to the caller.
	//
	// Unknown policies are passed into the callback as nil values.
	ForEachNodeDirectedChannel(tx kvdb.RTx, node route.Vertex,
		cb func(channel *channeldb.DirectedChannel) error) error

	// FetchNodeFeatures returns the features of a given node. If no
	// features are known for the node, an empty feature vector is returned.
	FetchNodeFeatures(node route.Vertex) (*lnwire.FeatureVector, error)
}

// CachedGraph is a routingGraph implementation that retrieves from the
// database.
type CachedGraph struct {
	graph  Graph
	tx     kvdb.RTx
	source route.Vertex
}

// A compile time assertion to make sure CachedGraph implements the routingGraph
// interface.
var _ routingGraph = (*CachedGraph)(nil)

// NewCachedGraph instantiates a new db-connected routing graph. It implicitly
// instantiates a new read transaction.
func NewCachedGraph(sourceNodePubKey route.Vertex,
	graph GraphWithReadLock) (*CachedGraph, error) {

	tx, err := graph.NewPathFindTx()
	if err != nil {
		return nil, err
	}

	return &CachedGraph{
		graph:  graph,
		tx:     tx,
		source: sourceNodePubKey,
	}, nil
}

// Close attempts to close the underlying db transaction. This is a no-op in
// case the underlying graph uses an in-memory cache.
func (g *CachedGraph) Close() error {
	if g.tx == nil {
		return nil
	}

	return g.tx.Rollback()
}

// forEachNodeChannel calls the callback for every channel of the given node.
//
// NOTE: Part of the routingGraph interface.
func (g *CachedGraph) forEachNodeChannel(nodePub route.Vertex,
	cb func(channel *channeldb.DirectedChannel) error) error {

	return g.graph.ForEachNodeDirectedChannel(g.tx, nodePub, cb)
}

// fetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the routingGraph interface.
func (g *CachedGraph) fetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	return g.graph.FetchNodeFeatures(nodePub)
}

// FetchAmountPairCapacity determines the maximal public capacity between two
// nodes depending on the amount we try to send.
func (g *CachedGraph) FetchAmountPairCapacity(nodeFrom, nodeTo route.Vertex,
	amount lnwire.MilliSatoshi) (btcutil.Amount, error) {

	// Create unified edges for all incoming connections.
	//
	// Note: Inbound fees are not used here because this method is only used
	// by a deprecated router rpc.
	u := newNodeEdgeUnifier(g.source, nodeTo, false, nil)

	err := u.addGraphPolicies(g)
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
