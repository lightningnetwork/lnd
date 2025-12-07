package graphdb

import (
	"context"
	"iter"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// NodeTraverser is an abstract read only interface that provides information
// about nodes and their edges. The interface is about providing fast read-only
// access to the graph and so if a cache is available, it should be used.
type NodeTraverser interface {
	// ForEachNodeDirectedChannel calls the callback for every channel of
	// the given node.
	ForEachNodeDirectedChannel(nodePub route.Vertex,
		cb func(channel *DirectedChannel) error, reset func()) error

	// FetchNodeFeatures returns the features of the given node.
	FetchNodeFeatures(nodePub route.Vertex) (*lnwire.FeatureVector, error)
}

// V1Store represents the main interface for the channel graph database for all
// channels and nodes gossiped via the V1 gossip protocol as defined in BOLT 7.
type V1Store interface { //nolint:interfacebloat
	NodeTraverser

	// AddNode adds a vertex/node to the graph database. If the
	// node is not in the database from before, this will add a new,
	// unconnected one to the graph. If it is present from before, this will
	// update that node's information. Note that this method is expected to
	// only be called to update an already present node from a node
	// announcement, or to insert a node found in a channel update.
	AddNode(ctx context.Context, node *models.Node,
		op ...batch.SchedulerOption) error

	// AddrsForNode returns all known addresses for the target node public
	// key that the graph DB is aware of. The returned boolean indicates if
	// the given node is unknown to the graph DB or not.
	AddrsForNode(ctx context.Context,
		nodePub *btcec.PublicKey) (bool, []net.Addr, error)

	// ForEachSourceNodeChannel iterates through all channels of the source
	// node, executing the passed callback on each. The call-back is
	// provided with the channel's outpoint, whether we have a policy for
	// the channel and the channel peer's node information.
	ForEachSourceNodeChannel(ctx context.Context,
		cb func(chanPoint wire.OutPoint, havePolicy bool,
			otherNode *models.Node) error,
		reset func()) error

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

	// ForEachNodeCached is similar to forEachNode, but it returns
	// DirectedChannel data to the call-back. If withAddrs is true, then
	// the call-back will also be provided with the addresses associated
	// with the node. The address retrieval will likely result in an
	// additional round-trip to the database, so it should only be used if
	// the addresses are actually needed.
	//
	// NOTE: The callback contents MUST not be modified.
	ForEachNodeCached(ctx context.Context, withAddrs bool,
		cb func(ctx context.Context, node route.Vertex,
			addrs []net.Addr,
			chans map[uint64]*DirectedChannel) error,
		reset func()) error

	// ForEachNode iterates through all the stored vertices/nodes in the
	// graph, executing the passed callback with each node encountered. If
	// the callback returns an error, then the transaction is aborted and
	// the iteration stops early.
	ForEachNode(ctx context.Context, cb func(*models.Node) error,
		reset func()) error

	// ForEachNodeCacheable iterates through all the stored vertices/nodes
	// in the graph, executing the passed callback with each node
	// encountered. If the callback returns an error, then the transaction
	// is aborted and the iteration stops early.
	ForEachNodeCacheable(ctx context.Context, cb func(route.Vertex,
		*lnwire.FeatureVector) error, reset func()) error

	// LookupAlias attempts to return the alias as advertised by the target
	// node.
	LookupAlias(ctx context.Context, pub *btcec.PublicKey) (string, error)

	// DeleteNode starts a new database transaction to remove a
	// vertex/node from the database according to the node's public key.
	DeleteNode(ctx context.Context, nodePub route.Vertex) error

	// NodeUpdatesInHorizon returns all the known lightning node which have
	// an update timestamp within the passed range. This method can be used
	// by two nodes to quickly determine if they have the same set of up to
	// date node announcements.
	NodeUpdatesInHorizon(startTime, endTime time.Time,
		opts ...IteratorOption) iter.Seq2[*models.Node, error]

	// FetchNode attempts to look up a target node by its identity
	// public key. If the node isn't found in the database, then
	// ErrGraphNodeNotFound is returned.
	FetchNode(ctx context.Context, nodePub route.Vertex) (*models.Node,
		error)

	// HasNode determines if the graph has a vertex identified by
	// the target node identity public key. If the node exists in the
	// database, a timestamp of when the data for the node was lasted
	// updated is returned along with a true boolean. Otherwise, an empty
	// time.Time is returned with a false boolean.
	HasNode(ctx context.Context, nodePub [33]byte) (time.Time, bool, error)

	// IsPublicNode is a helper method that determines whether the node with
	// the given public key is seen as a public node in the graph from the
	// graph's source node's point of view.
	IsPublicNode(pubKey [33]byte) (bool, error)

	// GraphSession will provide the call-back with access to a
	// NodeTraverser instance which can be used to perform queries against
	// the channel graph.
	GraphSession(cb func(graph NodeTraverser) error, reset func()) error

	// ForEachChannel iterates through all the channel edges stored within
	// the graph and invokes the passed callback for each edge. The callback
	// takes two edges as since this is a directed graph, both the in/out
	// edges are visited. If the callback returns an error, then the
	// transaction is aborted and the iteration stops early.
	//
	// NOTE: If an edge can't be found, or wasn't advertised, then a nil
	// pointer for that particular channel edge routing policy will be
	// passed into the callback.
	ForEachChannel(ctx context.Context, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy) error,
		reset func()) error

	// ForEachChannelCacheable iterates through all the channel edges stored
	// within the graph and invokes the passed callback for each edge. The
	// callback takes two edges as since this is a directed graph, both the
	// in/out edges are visited. If the callback returns an error, then the
	// transaction is aborted and the iteration stops early.
	//
	// NOTE: If an edge can't be found, or wasn't advertised, then a nil
	// pointer for that particular channel edge routing policy will be
	// passed into the callback.
	//
	// NOTE: this method is like ForEachChannel but fetches only the data
	// required for the graph cache.
	ForEachChannelCacheable(cb func(*models.CachedEdgeInfo,
		*models.CachedEdgePolicy, *models.CachedEdgePolicy) error,
		reset func()) error

	// DisabledChannelIDs returns the channel ids of disabled channels.
	// A channel is disabled when two of the associated ChanelEdgePolicies
	// have their disabled bit on.
	DisabledChannelIDs() ([]uint64, error)

	// AddChannelEdge adds a new (undirected, blank) edge to the graph
	// database. An undirected edge from the two target nodes are created.
	// The information stored denotes the static attributes of the channel,
	// such as the channelID, the keys involved in creation of the channel,
	// and the set of features that the channel supports. The chanPoint and
	// chanID are used to uniquely identify the edge globally within the
	// database.
	AddChannelEdge(ctx context.Context, edge *models.ChannelEdgeInfo,
		op ...batch.SchedulerOption) error

	// HasChannelEdge returns true if the database knows of a channel edge
	// with the passed channel ID, and false otherwise. If an edge with that
	// ID is found within the graph, then two time stamps representing the
	// last time the edge was updated for both directed edges are returned
	// along with the boolean. If it is not found, then the zombie index is
	// checked and its result is returned as the second boolean.
	HasChannelEdge(chanID uint64) (time.Time, time.Time, bool, bool,
		error)

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
		chanIDs ...uint64) ([]*models.ChannelEdgeInfo, error)

	// AddEdgeProof sets the proof of an existing edge in the graph
	// database.
	AddEdgeProof(chanID lnwire.ShortChannelID,
		proof *models.ChannelAuthProof) error

	// ChannelID attempt to lookup the 8-byte compact channel ID which maps
	// to the passed channel point (outpoint). If the passed channel doesn't
	// exist within the database, then ErrEdgeNotFound is returned.
	ChannelID(chanPoint *wire.OutPoint) (uint64, error)

	// HighestChanID returns the "highest" known channel ID in the channel
	// graph. This represents the "newest" channel from the PoV of the
	// chain. This method can be used by peers to quickly determine if
	// they're graphs are in sync.
	HighestChanID(ctx context.Context) (uint64, error)

	// ChanUpdatesInHorizon returns all the known channel edges which have
	// at least one edge that has an update timestamp within the specified
	// horizon.
	ChanUpdatesInHorizon(startTime, endTime time.Time,
		opts ...IteratorOption) iter.Seq2[ChannelEdge, error]

	// FilterKnownChanIDs takes a set of channel IDs and return the subset
	// of chan ID's that we don't know and are not known zombies of the
	// passed set. In other words, we perform a set difference of our set
	// of chan ID's and the ones passed in. This method can be used by
	// callers to determine the set of channels another peer knows of that
	// we don't. The ChannelUpdateInfos for the known zombies is also
	// returned.
	FilterKnownChanIDs(chansInfo []ChannelUpdateInfo) ([]uint64,
		[]ChannelUpdateInfo, error)

	// FilterChannelRange returns the channel ID's of all known channels
	// which were mined in a block height within the passed range. The
	// channel IDs are grouped by their common block height. This method can
	// be used to quickly share with a peer the set of channels we know of
	// within a particular range to catch them up after a period of time
	// offline. If withTimestamps is true then the timestamp info of the
	// latest received channel update messages of the channel will be
	// included in the response.
	FilterChannelRange(startHeight, endHeight uint32, withTimestamps bool) (
		[]BlockChannelRange, error)

	// FetchChanInfos returns the set of channel edges that correspond to
	// the passed channel ID's. If an edge is the query is unknown to the
	// database, it will skipped and the result will contain only those
	// edges that exist at the time of the query. This can be used to
	// respond to peer queries that are seeking to fill in gaps in their
	// view of the channel graph.
	FetchChanInfos(chanIDs []uint64) ([]ChannelEdge, error)

	// FetchChannelEdgesByOutpoint attempts to lookup the two directed edges
	// for the channel identified by the funding outpoint. If the channel
	// can't be found, then ErrEdgeNotFound is returned. A struct which
	// houses the general information for the channel itself is returned as
	// well as two structs that contain the routing policies for the channel
	// in either direction.
	FetchChannelEdgesByOutpoint(op *wire.OutPoint) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchChannelEdgesByID attempts to lookup the two directed edges for
	// the channel identified by the channel ID. If the channel can't be
	// found, then ErrEdgeNotFound is returned. A struct which houses the
	// general information for the channel itself is returned as well as
	// two structs that contain the routing policies for the channel in
	// either direction.
	//
	// ErrZombieEdge can be returned if the edge is currently marked as a
	// zombie within the database. In this case, the ChannelEdgePolicy's
	// will be nil, and the ChannelEdgeInfo will only include the public
	// keys of each node.
	FetchChannelEdgesByID(chanID uint64) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// ChannelView returns the verifiable edge information for each active
	// channel within the known channel graph. The set of UTXO's (along with
	// their scripts) returned are the ones that need to be watched on chain
	// to detect channel closes on the resident blockchain.
	ChannelView() ([]EdgePoint, error)

	// MarkEdgeZombie attempts to mark a channel identified by its channel
	// ID as a zombie. This method is used on an ad-hoc basis, when channels
	// need to be marked as zombies outside the normal pruning cycle.
	MarkEdgeZombie(chanID uint64,
		pubKey1, pubKey2 [33]byte) error

	// MarkEdgeLive clears an edge from our zombie index, deeming it as
	// live.
	MarkEdgeLive(chanID uint64) error

	// IsZombieEdge returns whether the edge is considered zombie. If it is
	// a zombie, then the two node public keys corresponding to this edge
	// are also returned.
	IsZombieEdge(chanID uint64) (bool, [33]byte, [33]byte, error)

	// NumZombies returns the current number of zombie channels in the
	// graph.
	NumZombies() (uint64, error)

	// PutClosedScid stores a SCID for a closed channel in the database.
	// This is so that we can ignore channel announcements that we know to
	// be closed without having to validate them and fetch a block.
	PutClosedScid(scid lnwire.ShortChannelID) error

	// IsClosedScid checks whether a channel identified by the passed in
	// scid is closed. This helps avoid having to perform expensive
	// validation checks.
	IsClosedScid(scid lnwire.ShortChannelID) (bool, error)

	// UpdateEdgePolicy updates the edge routing policy for a single
	// directed edge within the database for the referenced channel. The
	// `flags` attribute within the ChannelEdgePolicy determines which of
	// the directed edges are being updated. If the flag is 1, then the
	// first node's information is being updated, otherwise it's the second
	// node's information. The node ordering is determined by the
	// lexicographical ordering of the identity public keys of the nodes on
	// either side of the channel.
	UpdateEdgePolicy(ctx context.Context, edge *models.ChannelEdgePolicy,
		op ...batch.SchedulerOption) (route.Vertex, route.Vertex, error)

	// SourceNode returns the source node of the graph. The source node is
	// treated as the center node within a star-graph. This method may be
	// used to kick off a path finding algorithm in order to explore the
	// reachability of another node based off the source node.
	SourceNode(ctx context.Context) (*models.Node, error)

	// SetSourceNode sets the source node within the graph database. The
	// source node is to be used as the center of a star-graph within path
	// finding algorithms.
	SetSourceNode(ctx context.Context,
		node *models.Node) error

	// PruneTip returns the block height and hash of the latest block that
	// has been used to prune channels in the graph. Knowing the "prune tip"
	// allows callers to tell if the graph is currently in sync with the
	// current best known UTXO state.
	PruneTip() (*chainhash.Hash, uint32, error)

	// PruneGraphNodes is a garbage collection method which attempts to
	// prune out any nodes from the channel graph that are currently
	// unconnected. This ensures that we only maintain a graph of reachable
	// nodes. In the event that a pruned node gains more channels, it will
	// be re-added back to the graph.
	PruneGraphNodes() ([]route.Vertex, error)

	// PruneGraph prunes newly closed channels from the channel graph in
	// response to a new block being solved on the network. Any transactions
	// which spend the funding output of any known channels within he graph
	// will be deleted. Additionally, the "prune tip", or the last block
	// which has been used to prune the graph is stored so callers can
	// ensure the graph is fully in sync with the current UTXO state. A
	// slice of channels that have been closed by the target block along
	// with any pruned nodes are returned if the function succeeds without
	// error.
	PruneGraph(spentOutputs []*wire.OutPoint,
		blockHash *chainhash.Hash, blockHeight uint32) (
		[]*models.ChannelEdgeInfo, []route.Vertex, error)

	// DisconnectBlockAtHeight is used to indicate that the block specified
	// by the passed height has been disconnected from the main chain. This
	// will "rewind" the graph back to the height below, deleting channels
	// that are no longer confirmed from the graph. The prune log will be
	// set to the last prune height valid for the remaining chain.
	// Channels that were removed from the graph resulting from the
	// disconnected block are returned.
	DisconnectBlockAtHeight(height uint32) ([]*models.ChannelEdgeInfo,
		error)
}
