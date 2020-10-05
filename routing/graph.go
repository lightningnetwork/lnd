package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// routingGraph is an abstract interface that provides information about nodes
// and edges to pathfinding.
type routingGraph interface {
	// forEachNodeChannel calls the callback for every channel of the given node.
	forEachNodeChannel(nodePub route.Vertex,
		cb func(*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
			*channeldb.ChannelEdgePolicy) error) error

	// sourceNode returns the source node of the graph.
	sourceNode() route.Vertex

	// fetchNodeFeatures returns the features of the given node.
	fetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}

// dbRoutingTx is a routingGraph implementation that retrieves from the
// database.
type dbRoutingTx struct {
	graph  *channeldb.ChannelGraph
	tx     kvdb.RTx
	source route.Vertex
}

// newDbRoutingTx instantiates a new db-connected routing graph. It implictly
// instantiates a new read transaction.
func newDbRoutingTx(graph *channeldb.ChannelGraph) (*dbRoutingTx, error) {
	sourceNode, err := graph.SourceNode()
	if err != nil {
		return nil, err
	}

	tx, err := graph.Database().BeginReadTx()
	if err != nil {
		return nil, err
	}

	return &dbRoutingTx{
		graph:  graph,
		tx:     tx,
		source: sourceNode.PubKeyBytes,
	}, nil
}

// close closes the underlying db transaction.
func (g *dbRoutingTx) close() error {
	return g.tx.Rollback()
}

// forEachNodeChannel calls the callback for every channel of the given node.
//
// NOTE: Part of the routingGraph interface.
func (g *dbRoutingTx) forEachNodeChannel(nodePub route.Vertex,
	cb func(*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
		*channeldb.ChannelEdgePolicy) error) error {

	txCb := func(_ kvdb.RTx, info *channeldb.ChannelEdgeInfo,
		p1, p2 *channeldb.ChannelEdgePolicy) error {

		return cb(info, p1, p2)
	}

	return g.graph.ForEachNodeChannel(g.tx, nodePub[:], txCb)
}

// sourceNode returns the source node of the graph.
//
// NOTE: Part of the routingGraph interface.
func (g *dbRoutingTx) sourceNode() route.Vertex {
	return g.source
}

// fetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the routingGraph interface.
func (g *dbRoutingTx) fetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	targetNode, err := g.graph.FetchLightningNode(g.tx, nodePub)
	switch err {

	// If the node exists and has features, return them directly.
	case nil:
		return targetNode.Features, nil

	// If we couldn't find a node announcement, populate a blank feature
	// vector.
	case channeldb.ErrGraphNodeNotFound:
		return lnwire.EmptyFeatureVector(), nil

	// Otherwise bubble the error up.
	default:
		return nil, err
	}
}
