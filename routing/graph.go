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

	// sourceNode returns the source node of the graph.
	sourceNode() route.Vertex

	// fetchNodeFeatures returns the features of the given node.
	fetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)

	// FetchAmountPairCapacity determines the maximal capacity between two
	// pairs of nodes.
	FetchAmountPairCapacity(nodeFrom, nodeTo route.Vertex,
		amount lnwire.MilliSatoshi) (btcutil.Amount, error)
}

// CachedGraph is a routingGraph implementation that retrieves from the
// database.
type CachedGraph struct {
	graph  *channeldb.ChannelGraph
	tx     kvdb.RTx
	source route.Vertex
}

// A compile time assertion to make sure CachedGraph implements the routingGraph
// interface.
var _ routingGraph = (*CachedGraph)(nil)

// NewCachedGraph instantiates a new db-connected routing graph. It implicitly
// instantiates a new read transaction.
func NewCachedGraph(sourceNode *channeldb.LightningNode,
	graph *channeldb.ChannelGraph) (*CachedGraph, error) {

	tx, err := graph.NewPathFindTx()
	if err != nil {
		return nil, err
	}

	return &CachedGraph{
		graph:  graph,
		tx:     tx,
		source: sourceNode.PubKeyBytes,
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

	return g.graph.ForEachNodeChannel(g.tx, nodePub, cb)
}

// sourceNode returns the source node of the graph.
//
// NOTE: Part of the routingGraph interface.
func (g *CachedGraph) sourceNode() route.Vertex {
	return g.source
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
//
// NOTE: Part of the routingGraph interface.
func (g *CachedGraph) FetchAmountPairCapacity(nodeFrom, nodeTo route.Vertex,
	amount lnwire.MilliSatoshi) (btcutil.Amount, error) {

	// Create unified edges for all incoming connections.
	u := newNodeEdgeUnifier(g.sourceNode(), nodeTo, nil)

	err := u.addGraphPolicies(g)
	if err != nil {
		return 0, err
	}

	edgeUnifier, ok := u.edgeUnifiers[nodeFrom]
	if !ok {
		return 0, fmt.Errorf("no edge info for node pair %v -> %v",
			nodeFrom, nodeTo)
	}

	edge := edgeUnifier.getEdgeNetwork(amount)
	if edge == nil {
		return 0, fmt.Errorf("no edge for node pair %v -> %v "+
			"(amount %v)", nodeFrom, nodeTo, amount)
	}

	return edge.capacity, nil
}
