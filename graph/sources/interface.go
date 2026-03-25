package sources

import (
	"context"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/routing/route"
)

// RemoteGraph defines the read-only methods that a remote graph source must
// implement. These are the methods called by the Mux when it needs to query
// the remote graph provider for network data.
//
// Cache-related methods (ForEachNodeDirectedChannel, FetchNodeFeatures,
// ForEachNodeCached, GraphSession) and self-node methods (SourceNode,
// ForEachSourceNodeChannel) are intentionally excluded. The Mux handles these
// using the local graph cache and local DB respectively.
type RemoteGraph interface {
	// ForEachNode iterates through all nodes known to the remote source.
	ForEachNode(ctx context.Context,
		cb func(*models.Node) error, reset func()) error

	// ForEachChannel iterates through all channel edges known to the
	// remote source.
	ForEachChannel(ctx context.Context,
		cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error,
		reset func()) error

	// ForEachNodeChannel iterates through all channels of the given node.
	ForEachNodeChannel(ctx context.Context,
		nodePub route.Vertex,
		cb func(*models.ChannelEdgeInfo,
			*models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error,
		reset func()) error

	// FetchChannelEdgesByID looks up directed edges by channel ID.
	FetchChannelEdgesByID(ctx context.Context, chanID uint64) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchChannelEdgesByOutpoint looks up directed edges by funding
	// outpoint.
	FetchChannelEdgesByOutpoint(ctx context.Context,
		op *wire.OutPoint) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchNode looks up a target node by its identity public key.
	FetchNode(ctx context.Context,
		nodePub route.Vertex) (*models.Node, error)

	// IsPublicNode determines whether the given node is seen as a public
	// node in the remote graph.
	IsPublicNode(ctx context.Context, pubKey [33]byte) (bool, error)

	// LookupAlias returns the alias as advertised by the target node.
	LookupAlias(ctx context.Context,
		pub *btcec.PublicKey) (string, error)

	// AddrsForNode returns all known addresses for the target node.
	AddrsForNode(ctx context.Context,
		nodePub *btcec.PublicKey) (bool, []net.Addr, error)
}
