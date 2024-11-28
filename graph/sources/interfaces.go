package sources

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/graph/session"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing/route"
)

// GraphSource defines the read-only graph interface required by LND for graph
// related queries.
//
//nolint:interfacebloat
type GraphSource interface {
	session.ReadOnlyGraph
	invoicesrpc.GraphSource
	netann.ChannelGraph
	channeldb.AddrSource

	// ForEachChannel iterates through all the channel edges stored within
	// the graph and invokes the passed callback for each edge. If the
	// callback returns an error, then the transaction is aborted and the
	// iteration stops early. An edge's policy structs may be nil if the
	// ChannelUpdate in question has not yet been received for the channel.
	// No error is returned if no channels are found.
	ForEachChannel(ctx context.Context, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error

	// ForEachNode iterates through all the stored vertices/nodes in the
	// graph, executing the passed callback with each node encountered. If
	// the callback returns an error, then the transaction is aborted and
	// the iteration stops early. No error is returned if no nodes are
	// found.
	ForEachNode(ctx context.Context,
		cb func(*models.LightningNode) error) error

	// HasLightningNode determines if the graph has a vertex identified by
	// the target node identity public key. If the node exists in the
	// database, a timestamp of when the data for the node was lasted
	// updated is returned along with a true boolean. Otherwise, an empty
	// time.Time is returned with a false boolean and a nil error.
	HasLightningNode(ctx context.Context, nodePub [33]byte) (time.Time,
		bool, error)

	// LookupAlias attempts to return the alias as advertised by the target
	// node. graphdb.ErrNodeAliasNotFound is returned if the alias is not
	// found. No error is returned if the node is not found.
	LookupAlias(ctx context.Context, pub *btcec.PublicKey) (string, error)

	// ForEachNodeChannel iterates through all channels of the given node,
	// executing the passed callback with an edge info structure and the
	// policies of each end of the channel. The first edge policy is the
	// outgoing edge *to* the connecting node, while the second is the
	// incoming edge *from* the connecting node. If the callback returns an
	// error, then the iteration is halted with the error propagated back up
	// to the caller. Unknown policies are passed into the callback as nil
	// values. No error is returned if the node is not found.
	ForEachNodeChannel(ctx context.Context,
		nodePub route.Vertex, cb func(*models.ChannelEdgeInfo,
			*models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error) error

	// FetchLightningNode attempts to look up a target node by its identity
	// public key. If the node isn't found in the database, then
	// graphdb.ErrGraphNodeNotFound is returned.
	FetchLightningNode(ctx context.Context, nodePub route.Vertex) (
		*models.LightningNode, error)

	// GraphBootstrapper returns a network peer bootstrapper that can be
	// used to discover new peers to connect to.
	GraphBootstrapper(ctx context.Context) (
		discovery.NetworkPeerBootstrapper, error)

	// NetworkStats returns statistics concerning the current state of the
	// known channel graph within the network.
	NetworkStats(ctx context.Context) (*models.NetworkStats, error)

	// BetweennessCentrality computes the normalised and non-normalised
	// betweenness centrality for each node in the graph.
	BetweennessCentrality(ctx context.Context) (
		map[autopilot.NodeID]*models.BetweennessCentrality, error)
}
