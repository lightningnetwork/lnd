package graphdb

import (
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/routing/route"
)

// NodeTx represents transaction object with an underlying node associated that
// can be used to make further queries to the graph under the same transaction.
// This is useful for consistency during graph traversal and queries.
type NodeTx interface {
	// Node returns the raw information of the node.
	Node() *models.LightningNode

	// ForEachChannel can be used to iterate over the node's channels under
	// the same transaction used to fetch the node.
	ForEachChannel(func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error

	// FetchNode fetches the node with the given pub key under the same
	// transaction used to fetch the current node. The returned node is also
	// a NodeTx and any operations on that NodeTx will also be done under
	// the same transaction.
	FetchNode(node route.Vertex) (NodeTx, error)
}
