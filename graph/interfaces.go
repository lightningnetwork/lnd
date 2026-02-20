package graph

import (
	"context"

	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ChannelGraphSource represents the source of information about the topology
// of the lightning network. It's responsible for the addition of nodes, edges,
// applying edge updates, and returning the current block height with which the
// topology is synchronized.
//
//nolint:interfacebloat
type ChannelGraphSource interface {
	// AddNode is used to add information about a node to the router
	// database. If the node with this pubkey is not present in an existing
	// channel, it will be ignored.
	AddNode(ctx context.Context, node *models.Node,
		op ...batch.SchedulerOption) error

	// AddEdge is used to add edge/channel to the topology of the router,
	// after all information about channel will be gathered this
	// edge/channel might be used in construction of payment path.
	AddEdge(ctx context.Context, edge *models.ChannelEdgeInfo,
		op ...batch.SchedulerOption) error

	// AddProof updates the channel edge info with proof which is needed to
	// properly announce the edge to the rest of the network.
	AddProof(chanID lnwire.ShortChannelID,
		proof *models.ChannelAuthProof) error

	// UpdateEdge is used to update edge information, without this message
	// edge considered as not fully constructed.
	UpdateEdge(ctx context.Context, policy *models.ChannelEdgePolicy,
		op ...batch.SchedulerOption) error

	// IsStaleNode returns true if the graph source has a node announcement
	// for the target node/version that is at least as fresh as the passed
	// announcement. This method will also return true if we don't have an
	// active channel announcement for the target node/version.
	IsStaleNode(ctx context.Context, v lnwire.GossipVersion,
		nodePub route.Vertex, updateTimestamp lnwire.Timestamp) bool

	// IsPublicNode determines whether the given vertex is seen as a public
	// node in the graph from the graph's source node's point of view.
	IsPublicNode(v lnwire.GossipVersion, node route.Vertex) (bool, error)

	// IsKnownEdge returns true if the graph source already knows of the
	// passed channel ID either as a live or zombie edge.
	IsKnownEdge(v lnwire.GossipVersion, chanID lnwire.ShortChannelID) bool

	// IsStaleEdgePolicy returns true if the graph source has a channel
	// edge for the passed policy that has a more recent announcement.
	IsStaleEdgePolicy(policy *models.ChannelEdgePolicy) bool

	// MarkEdgeLive clears an edge from our zombie index, deeming it as
	// live.
	MarkEdgeLive(v lnwire.GossipVersion, chanID lnwire.ShortChannelID) error

	// ForAllOutgoingChannels is used to iterate over all channels
	// emanating from the "source" node which is the center of the
	// star-graph.
	ForAllOutgoingChannels(ctx context.Context,
		cb func(c *models.ChannelEdgeInfo,
			e *models.ChannelEdgePolicy) error, reset func()) error

	// CurrentBlockHeight returns the block height from POV of the router
	// subsystem.
	CurrentBlockHeight() (uint32, error)

	// GetChannelByID return the channel by the channel id.
	GetChannelByID(v lnwire.GossipVersion, chanID lnwire.ShortChannelID) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchNode attempts to look up a target node by its identity
	// public key. channeldb.ErrGraphNodeNotFound is returned if the node
	// doesn't exist within the graph.
	FetchNode(context.Context, route.Vertex) (*models.Node, error)

	// MarkZombieEdge marks the channel with the given ID as a zombie edge.
	MarkZombieEdge(v lnwire.GossipVersion, chanID uint64) error

	// IsZombieEdge returns true if the edge with the given channel ID is
	// currently marked as a zombie edge.
	IsZombieEdge(v lnwire.GossipVersion,
		chanID lnwire.ShortChannelID) (bool, error)
}
