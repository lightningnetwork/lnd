package graph

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/batch"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
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
	// for the target node with a more recent timestamp. This method will
	// also return true if we don't have an active channel announcement for
	// the target node.
	IsStaleNode(ctx context.Context, node route.Vertex,
		timestamp time.Time) bool

	// IsPublicNode determines whether the given vertex is seen as a public
	// node in the graph from the graph's source node's point of view.
	IsPublicNode(node route.Vertex) (bool, error)

	// IsKnownEdge returns true if the graph source already knows of the
	// passed channel ID either as a live or zombie edge.
	IsKnownEdge(chanID lnwire.ShortChannelID) bool

	// IsStaleEdgePolicy returns true if the graph source has a channel
	// edge for the passed channel ID (and flags) that have a more recent
	// timestamp.
	IsStaleEdgePolicy(chanID lnwire.ShortChannelID, timestamp time.Time,
		flags lnwire.ChanUpdateChanFlags) bool

	// MarkEdgeLive clears an edge from our zombie index, deeming it as
	// live.
	MarkEdgeLive(chanID lnwire.ShortChannelID) error

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
	GetChannelByID(chanID lnwire.ShortChannelID) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchNode attempts to look up a target node by its identity
	// public key. channeldb.ErrGraphNodeNotFound is returned if the node
	// doesn't exist within the graph.
	FetchNode(context.Context, route.Vertex) (*models.Node, error)

	// MarkZombieEdge marks the channel with the given ID as a zombie edge.
	MarkZombieEdge(chanID uint64) error

	// IsZombieEdge returns true if the edge with the given channel ID is
	// currently marked as a zombie edge.
	IsZombieEdge(chanID lnwire.ShortChannelID) (bool, error)
}

// DB is an interface describing a persisted Lightning Network graph.
//
//nolint:interfacebloat
type DB interface {
	// PruneTip returns the block height and hash of the latest block that
	// has been used to prune channels in the graph. Knowing the "prune tip"
	// allows callers to tell if the graph is currently in sync with the
	// current best known UTXO state.
	PruneTip() (*chainhash.Hash, uint32, error)

	// PruneGraph prunes newly closed channels from the channel graph in
	// response to a new block being solved on the network. Any transactions
	// which spend the funding output of any known channels within the graph
	// will be deleted. Additionally, the "prune tip", or the last block
	// which has been used to prune the graph is stored so callers can
	// ensure the graph is fully in sync with the current UTXO state. A
	// slice of channels that have been closed by the target block are
	// returned if the function succeeds without error.
	PruneGraph(spentOutputs []*wire.OutPoint, blockHash *chainhash.Hash,
		blockHeight uint32) ([]*models.ChannelEdgeInfo, error)

	// ChannelView returns the verifiable edge information for each active
	// channel within the known channel graph. The set of UTXO's (along with
	// their scripts) returned are the ones that need to be watched on
	// chain to detect channel closes on the resident blockchain.
	ChannelView() ([]graphdb.EdgePoint, error)

	// PruneGraphNodes is a garbage collection method which attempts to
	// prune out any nodes from the channel graph that are currently
	// unconnected. This ensure that we only maintain a graph of reachable
	// nodes. In the event that a pruned node gains more channels, it will
	// be re-added back to the graph.
	PruneGraphNodes() error

	// SourceNode returns the source node of the graph. The source node is
	// treated as the center node within a star-graph. This method may be
	// used to kick off a path finding algorithm in order to explore the
	// reachability of another node based off the source node.
	SourceNode(ctx context.Context) (*models.Node, error)

	// DisabledChannelIDs returns the channel ids of disabled channels.
	// A channel is disabled when two of the associated ChanelEdgePolicies
	// have their disabled bit on.
	DisabledChannelIDs() ([]uint64, error)

	// FetchChanInfos returns the set of channel edges that correspond to
	// the passed channel ID's. If an edge is the query is unknown to the
	// database, it will skipped and the result will contain only those
	// edges that exist at the time of the query. This can be used to
	// respond to peer queries that are seeking to fill in gaps in their
	// view of the channel graph.
	FetchChanInfos(chanIDs []uint64) ([]graphdb.ChannelEdge, error)

	// ChanUpdatesInHorizon returns all the known channel edges which have
	// at least one edge that has an update timestamp within the specified
	// horizon.
	ChanUpdatesInHorizon(startTime, endTime time.Time) (
		[]graphdb.ChannelEdge, error)

	// DeleteChannelEdges removes edges with the given channel IDs from the
	// database and marks them as zombies. This ensures that we're unable to
	// re-add it to our database once again. If an edge does not exist
	// within the database, then ErrEdgeNotFound will be returned. If
	// strictZombiePruning is true, then when we mark these edges as
	// zombies, we'll set up the keys such that we require the node that
	// failed to send the fresh update to be the one that resurrects the
	// channel from its zombie state. The markZombie bool denotes whether
	// to mark the channel as a zombie.
	DeleteChannelEdges(strictZombiePruning, markZombie bool,
		chanIDs ...uint64) error

	// DisconnectBlockAtHeight is used to indicate that the block specified
	// by the passed height has been disconnected from the main chain. This
	// will "rewind" the graph back to the height below, deleting channels
	// that are no longer confirmed from the graph. The prune log will be
	// set to the last prune height valid for the remaining chain.
	// Channels that were removed from the graph resulting from the
	// disconnected block are returned.
	DisconnectBlockAtHeight(height uint32) ([]*models.ChannelEdgeInfo,
		error)

	// HasChannelEdge returns true if the database knows of a channel edge
	// with the passed channel ID, and false otherwise. If an edge with that
	// ID is found within the graph, then two time stamps representing the
	// last time the edge was updated for both directed edges are returned
	// along with the boolean. If it is not found, then the zombie index is
	// checked and its result is returned as the second boolean.
	HasChannelEdge(chanID uint64) (time.Time, time.Time, bool, bool, error)

	// FetchChannelEdgesByID attempts to lookup the two directed edges for
	// the channel identified by the channel ID. If the channel can't be
	// found, then ErrEdgeNotFound is returned. A struct which houses the
	// general information for the channel itself is returned as well as
	// two structs that contain the routing policies for the channel in
	// either direction.
	//
	// ErrZombieEdge an be returned if the edge is currently marked as a
	// zombie within the database. In this case, the ChannelEdgePolicy's
	// will be nil, and the ChannelEdgeInfo will only include the public
	// keys of each node.
	FetchChannelEdgesByID(chanID uint64) (*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error)

	// AddNode adds a vertex/node to the graph database. If the
	// node is not in the database from before, this will add a new,
	// unconnected one to the graph. If it is present from before, this will
	// update that node's information. Note that this method is expected to
	// only be called to update an already present node from a node
	// announcement, or to insert a node found in a channel update.
	AddNode(ctx context.Context, node *models.Node,
		op ...batch.SchedulerOption) error

	// AddChannelEdge adds a new (undirected, blank) edge to the graph
	// database. An undirected edge from the two target nodes are created.
	// The information stored denotes the static attributes of the channel,
	// such as the channelID, the keys involved in creation of the channel,
	// and the set of features that the channel supports. The chanPoint and
	// chanID are used to uniquely identify the edge globally within the
	// database.
	AddChannelEdge(ctx context.Context, edge *models.ChannelEdgeInfo,
		op ...batch.SchedulerOption) error

	// MarkEdgeZombie attempts to mark a channel identified by its channel
	// ID as a zombie. This method is used on an ad-hoc basis, when channels
	// need to be marked as zombies outside the normal pruning cycle.
	MarkEdgeZombie(chanID uint64, pubKey1, pubKey2 [33]byte) error

	// UpdateEdgePolicy updates the edge routing policy for a single
	// directed edge within the database for the referenced channel. The
	// `flags` attribute within the ChannelEdgePolicy determines which of
	// the directed edges are being updated. If the flag is 1, then the
	// first node's information is being updated, otherwise it's the second
	// node's information. The node ordering is determined by the
	// lexicographical ordering of the identity public keys of the nodes on
	// either side of the channel.
	UpdateEdgePolicy(ctx context.Context, edge *models.ChannelEdgePolicy,
		op ...batch.SchedulerOption) error

	// HasLightningNode determines if the graph has a vertex identified by
	// the target node identity public key. If the node exists in the
	// database, a timestamp of when the data for the node was lasted
	// updated is returned along with a true boolean. Otherwise, an empty
	// time.Time is returned with a false boolean.
	HasLightningNode(ctx context.Context, nodePub [33]byte) (time.Time,
		bool, error)

	// FetchNode attempts to look up a target node by its identity
	// public key. If the node isn't found in the database, then
	// ErrGraphNodeNotFound is returned.
	FetchNode(ctx context.Context, nodePub route.Vertex) (*models.Node,
		error)

	// ForEachNodeChannel iterates through all channels of the given node,
	// executing the passed callback with an edge info structure and the
	// policies of each end of the channel. The first edge policy is the
	// outgoing edge *to* the connecting node, while the second is the
	// incoming edge *from* the connecting node. If the callback returns an
	// error, then the iteration is halted with the error propagated back up
	// to the caller.
	//
	// Unknown policies are passed into the callback as nil values.
	ForEachNodeChannel(ctx context.Context, nodePub route.Vertex,
		cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error, reset func()) error

	// AddEdgeProof sets the proof of an existing edge in the graph
	// database.
	AddEdgeProof(chanID lnwire.ShortChannelID,
		proof *models.ChannelAuthProof) error

	// IsPublicNode is a helper method that determines whether the node with
	// the given public key is seen as a public node in the graph from the
	// graph's source node's point of view.
	IsPublicNode(pubKey [33]byte) (bool, error)

	// MarkEdgeLive clears an edge from our zombie index, deeming it as
	// live.
	MarkEdgeLive(chanID uint64) error
}
