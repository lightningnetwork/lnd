package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Graph is an abstract interface that provides information about nodes
// and edges to pathfinding.
type Graph interface {
	// forEachNodeChannel calls the callback for every channel of the given node.
	forEachNodeChannel(nodePub route.Vertex,
		cb func(channel *channeldb.DirectedChannel) error) error

	// sourceNode returns the source node of the graph.
	sourceNode() route.Vertex

	// fetchNodeFeatures returns the features of the given node.
	fetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}

// CachedGraph is a Graph implementation that retrieves from the
// database.
type CachedGraph struct {
	graph  *channeldb.ChannelGraph
	source route.Vertex
}

// NewCachedGraph instantiates a new db-connected routing graph. It implictly
// instantiates a new read transaction.
func NewCachedGraph(graph *channeldb.ChannelGraph) (*CachedGraph, error) {
	sourceNode, err := graph.SourceNode()
	if err != nil {
		return nil, err
	}

	return &CachedGraph{
		graph:  graph,
		source: sourceNode.PubKeyBytes,
	}, nil
}

// forEachNodeChannel calls the callback for every channel of the given node.
//
// NOTE: Part of the Graph interface.
func (g *CachedGraph) forEachNodeChannel(nodePub route.Vertex,
	cb func(channel *channeldb.DirectedChannel) error) error {

	return g.graph.ForEachNodeChannel(nodePub, cb)
}

// sourceNode returns the source node of the graph.
//
// NOTE: Part of the Graph interface.
func (g *CachedGraph) sourceNode() route.Vertex {
	return g.source
}

// fetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the Graph interface.
func (g *CachedGraph) fetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	return g.graph.FetchNodeFeatures(nodePub)
}
