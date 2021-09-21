package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// routingGraph is an abstract interface that provides information about nodes
// and edges to pathfinding.
type routingGraph interface {
	// forEachNodeChannel calls the callback for every channel of the given node.
	forEachNodeChannel(nodePub route.Vertex,
		cb func(channel *channeldb.DirectedChannel) error) error

	// sourceNode returns the source node of the graph.
	sourceNode() route.Vertex

	// fetchNodeFeatures returns the features of the given node.
	fetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}

// dbRoutingTx is a routingGraph implementation that retrieves from the
// database.
type dbRoutingTx struct {
	graph  *channeldb.ChannelGraph
	source route.Vertex
}

// newDbRoutingTx instantiates a new db-connected routing graph. It implictly
// instantiates a new read transaction.
func newDbRoutingTx(graph *channeldb.ChannelGraph) (*dbRoutingTx, error) {
	sourceNode, err := graph.SourceNode()
	if err != nil {
		return nil, err
	}

	return &dbRoutingTx{
		graph:  graph,
		source: sourceNode.PubKeyBytes,
	}, nil
}

// forEachNodeChannel calls the callback for every channel of the given node.
//
// NOTE: Part of the routingGraph interface.
func (g *dbRoutingTx) forEachNodeChannel(nodePub route.Vertex,
	cb func(channel *channeldb.DirectedChannel) error) error {

	return g.graph.ForEachNodeChannel(nodePub, cb)
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

	return g.graph.FetchNodeFeatures(nodePub)
}
