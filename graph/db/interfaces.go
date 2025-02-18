package graphdb

import (
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// NodeRTx represents transaction object with an underlying node associated that
// can be used to make further queries to the graph under the same transaction.
// This is useful for consistency during graph traversal and queries.
type NodeRTx interface {
	// Node returns the raw information of the node.
	Node() *models.LightningNode

	// ForEachChannel can be used to iterate over the node's channels under
	// the same transaction used to fetch the node.
	ForEachChannel(func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error

	// FetchNode fetches the node with the given pub key under the same
	// transaction used to fetch the current node. The returned node is also
	// a NodeRTx and any operations on that NodeRTx will also be done under
	// the same transaction.
	FetchNode(node route.Vertex) (NodeRTx, error)
}

// NodeTraverser is an abstract read only interface that provides information
// about nodes and their edges. The interface is about providing fast read-only
// access to the graph and so if a cache is available, it should be used.
type NodeTraverser interface {
	// ForEachNodeDirectedChannel calls the callback for every channel of
	// the given node.
	ForEachNodeDirectedChannel(nodePub route.Vertex,
		cb func(channel *DirectedChannel) error) error

	// FetchNodeFeatures returns the features of the given node.
	FetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}
