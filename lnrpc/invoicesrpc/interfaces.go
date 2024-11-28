package invoicesrpc

import (
	"context"

	"github.com/lightningnetwork/lnd/graph/db/models"
)

// GraphSource defines the graph interface required by the invoice rpc server.
type GraphSource interface {
	// FetchChannelEdgesByID attempts to look up the two directed edges for
	// the channel identified by the channel ID. If the channel can't be
	// found, then graphdb.ErrEdgeNotFound is returned.
	//
	// graphdb.ErrZombieEdge can be returned if the edge is currently marked
	// as a zombie within the database. In this case, the
	// ChannelEdgePolicy's will be nil, and the ChannelEdgeInfo will only
	// include the public keys of each node.
	FetchChannelEdgesByID(ctx context.Context, chanID uint64) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// IsPublicNode is a helper method that determines whether the node with
	// the given public key is seen as a public node in the graph from the
	// graph's source node's point of view. If this node is unknown, then
	// graphdb.ErrGraphNodeNotFound is returned.
	IsPublicNode(ctx context.Context, pubKey [33]byte) (bool, error)
}
