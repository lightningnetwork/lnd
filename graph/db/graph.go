package graphdb

import (
	"errors"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Config is a struct that holds all the necessary dependencies for a
// ChannelGraph.
type Config struct {
	// KVDB is the kvdb.Backend that will be used for initializing the
	// KVStore CRUD layer.
	KVDB kvdb.Backend

	// KVStoreOpts is a list of functional options that will be used when
	// initializing the KVStore.
	KVStoreOpts []KVStoreOptionModifier
}

// ChannelGraph is a layer above the graph's CRUD layer.
//
// NOTE: currently, this is purely a pass-through layer directly to the backing
// KVStore. Upcoming commits will move the graph cache out of the KVStore and
// into this layer so that the KVStore is only responsible for CRUD operations.
type ChannelGraph struct {
	graphCache *GraphCache

	*KVStore
}

// NewChannelGraph creates a new ChannelGraph instance with the given backend.
func NewChannelGraph(cfg *Config, options ...ChanGraphOption) (*ChannelGraph,
	error) {

	opts := defaultChanGraphOptions()
	for _, o := range options {
		o(opts)
	}

	store, err := NewKVStore(cfg.KVDB, cfg.KVStoreOpts...)
	if err != nil {
		return nil, err
	}

	if !opts.useGraphCache {
		return &ChannelGraph{
			KVStore: store,
		}, nil
	}

	// The graph cache can be turned off (e.g. for mobile users) for a
	// speed/memory usage tradeoff.
	graphCache := NewGraphCache(opts.preAllocCacheNumNodes)
	startTime := time.Now()
	log.Debugf("Populating in-memory channel graph, this might take a " +
		"while...")

	err = store.ForEachNodeCacheable(func(node route.Vertex,
		features *lnwire.FeatureVector) error {

		graphCache.AddNodeFeatures(node, features)

		return nil
	})
	if err != nil {
		return nil, err
	}

	err = store.ForEachChannel(func(info *models.ChannelEdgeInfo,
		policy1, policy2 *models.ChannelEdgePolicy) error {

		graphCache.AddChannel(info, policy1, policy2)

		return nil
	})
	if err != nil {
		return nil, err
	}

	log.Debugf("Finished populating in-memory channel graph (took %v, %s)",
		time.Since(startTime), graphCache.Stats())

	return &ChannelGraph{
		KVStore:    store,
		graphCache: graphCache,
	}, nil
}

// ForEachNodeDirectedChannel iterates through all channels of a given node,
// executing the passed callback on the directed edge representing the channel
// and its incoming policy. If the callback returns an error, then the iteration
// is halted with the error propagated back up to the caller. If the graphCache
// is available, then it will be used to retrieve the node's channels instead
// of the database.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (c *ChannelGraph) ForEachNodeDirectedChannel(node route.Vertex,
	cb func(channel *DirectedChannel) error) error {

	if c.graphCache != nil {
		return c.graphCache.ForEachChannel(node, cb)
	}

	return c.KVStore.ForEachNodeDirectedChannel(node, cb)
}

// FetchNodeFeatures returns the features of the given node. If no features are
// known for the node, an empty feature vector is returned.
// If the graphCache is available, then it will be used to retrieve the node's
// features instead of the database.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (c *ChannelGraph) FetchNodeFeatures(node route.Vertex) (
	*lnwire.FeatureVector, error) {

	if c.graphCache != nil {
		return c.graphCache.GetFeatures(node), nil
	}

	return c.KVStore.FetchNodeFeatures(node)
}

// GraphSession will provide the call-back with access to a NodeTraverser
// instance which can be used to perform queries against the channel graph. If
// the graph cache is not enabled, then the call-back will  be provided with
// access to the graph via a consistent read-only transaction.
func (c *ChannelGraph) GraphSession(cb func(graph NodeTraverser) error) error {
	if c.graphCache != nil {
		return cb(c)
	}

	return c.KVStore.GraphSession(cb)
}

// ForEachNodeCached iterates through all the stored vertices/nodes in the
// graph, executing the passed callback with each node encountered.
//
// NOTE: The callback contents MUST not be modified.
func (c *ChannelGraph) ForEachNodeCached(cb func(node route.Vertex,
	chans map[uint64]*DirectedChannel) error) error {

	if c.graphCache != nil {
		return c.graphCache.ForEachNode(cb)
	}

	return c.KVStore.ForEachNodeCached(cb)
}

// AddLightningNode adds a vertex/node to the graph database. If the node is not
// in the database from before, this will add a new, unconnected one to the
// graph. If it is present from before, this will update that node's
// information. Note that this method is expected to only be called to update an
// already present node from a node announcement, or to insert a node found in a
// channel update.
func (c *ChannelGraph) AddLightningNode(node *models.LightningNode,
	op ...batch.SchedulerOption) error {

	err := c.KVStore.AddLightningNode(node, op...)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.AddNodeFeatures(
			node.PubKeyBytes, node.Features,
		)
	}

	return nil
}

// DeleteLightningNode starts a new database transaction to remove a vertex/node
// from the database according to the node's public key.
func (c *ChannelGraph) DeleteLightningNode(nodePub route.Vertex) error {
	err := c.KVStore.DeleteLightningNode(nodePub)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.RemoveNode(nodePub)
	}

	return nil
}

// AddChannelEdge adds a new (undirected, blank) edge to the graph database. An
// undirected edge from the two target nodes are created. The information stored
// denotes the static attributes of the channel, such as the channelID, the keys
// involved in creation of the channel, and the set of features that the channel
// supports. The chanPoint and chanID are used to uniquely identify the edge
// globally within the database.
func (c *ChannelGraph) AddChannelEdge(edge *models.ChannelEdgeInfo,
	op ...batch.SchedulerOption) error {

	err := c.KVStore.AddChannelEdge(edge, op...)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.AddChannel(edge, nil, nil)
	}

	return nil
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
// If the cache is enabled, the edge will be added back to the graph cache if
// we still have a record of this channel in the DB.
func (c *ChannelGraph) MarkEdgeLive(chanID uint64) error {
	err := c.KVStore.MarkEdgeLive(chanID)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		// We need to add the channel back into our graph cache,
		// otherwise we won't use it for path finding.
		infos, err := c.KVStore.FetchChanInfos([]uint64{chanID})
		if err != nil {
			return err
		}

		if len(infos) == 0 {
			return nil
		}

		info := infos[0]

		c.graphCache.AddChannel(info.Info, info.Policy1, info.Policy2)
	}

	return nil
}

// DeleteChannelEdges removes edges with the given channel IDs from the
// database and marks them as zombies. This ensures that we're unable to re-add
// it to our database once again. If an edge does not exist within the
// database, then ErrEdgeNotFound will be returned. If strictZombiePruning is
// true, then when we mark these edges as zombies, we'll set up the keys such
// that we require the node that failed to send the fresh update to be the one
// that resurrects the channel from its zombie state. The markZombie bool
// denotes whether or not to mark the channel as a zombie.
func (c *ChannelGraph) DeleteChannelEdges(strictZombiePruning, markZombie bool,
	chanIDs ...uint64) error {

	infos, err := c.KVStore.DeleteChannelEdges(
		strictZombiePruning, markZombie, chanIDs...,
	)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		for _, info := range infos {
			c.graphCache.RemoveChannel(
				info.NodeKey1Bytes, info.NodeKey2Bytes,
				info.ChannelID,
			)
		}
	}

	return err
}

// DisconnectBlockAtHeight is used to indicate that the block specified
// by the passed height has been disconnected from the main chain. This
// will "rewind" the graph back to the height below, deleting channels
// that are no longer confirmed from the graph. The prune log will be
// set to the last prune height valid for the remaining chain.
// Channels that were removed from the graph resulting from the
// disconnected block are returned.
func (c *ChannelGraph) DisconnectBlockAtHeight(height uint32) (
	[]*models.ChannelEdgeInfo, error) {

	edges, err := c.KVStore.DisconnectBlockAtHeight(height)
	if err != nil {
		return nil, err
	}

	if c.graphCache != nil {
		for _, edge := range edges {
			c.graphCache.RemoveChannel(
				edge.NodeKey1Bytes, edge.NodeKey2Bytes,
				edge.ChannelID,
			)
		}
	}

	return edges, nil
}

// PruneGraph prunes newly closed channels from the channel graph in response
// to a new block being solved on the network. Any transactions which spend the
// funding output of any known channels within he graph will be deleted.
// Additionally, the "prune tip", or the last block which has been used to
// prune the graph is stored so callers can ensure the graph is fully in sync
// with the current UTXO state. A slice of channels that have been closed by
// the target block are returned if the function succeeds without error.
func (c *ChannelGraph) PruneGraph(spentOutputs []*wire.OutPoint,
	blockHash *chainhash.Hash, blockHeight uint32) (
	[]*models.ChannelEdgeInfo, error) {

	edges, err := c.KVStore.PruneGraph(spentOutputs, blockHash, blockHeight)
	if err != nil {
		return nil, err
	}

	// Now that the graph has been pruned, we'll also attempt to
	// prune any nodes that have had a channel closed within the
	// latest block.
	nodes, err := c.KVStore.PruneGraphNodes()
	if err != nil {
		return nil, err
	}

	if c.graphCache != nil {
		for _, edge := range edges {
			c.graphCache.RemoveChannel(
				edge.NodeKey1Bytes, edge.NodeKey2Bytes,
				edge.ChannelID,
			)
		}

		for _, node := range nodes {
			c.graphCache.RemoveNode(node)
		}

		log.Debugf("Pruned graph, cache now has %s",
			c.graphCache.Stats())
	}

	return edges, nil
}

// PruneGraphNodes is a garbage collection method which attempts to prune out
// any nodes from the channel graph that are currently unconnected. This ensure
// that we only maintain a graph of reachable nodes. In the event that a pruned
// node gains more channels, it will be re-added back to the graph.
func (c *ChannelGraph) PruneGraphNodes() error {
	nodes, err := c.KVStore.PruneGraphNodes()
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		for _, node := range nodes {
			c.graphCache.RemoveNode(node)
		}
	}

	return nil
}

// FilterKnownChanIDs takes a set of channel IDs and return the subset of chan
// ID's that we don't know and are not known zombies of the passed set. In other
// words, we perform a set difference of our set of chan ID's and the ones
// passed in. This method can be used by callers to determine the set of
// channels another peer knows of that we don't.
func (c *ChannelGraph) FilterKnownChanIDs(chansInfo []ChannelUpdateInfo,
	isZombieChan func(time.Time, time.Time) bool) ([]uint64, error) {

	unknown, knownZombies, err := c.KVStore.FilterKnownChanIDs(chansInfo)
	if err != nil {
		return nil, err
	}

	for _, info := range knownZombies {
		// TODO(ziggie): Make sure that for the strict pruning case we
		// compare the pubkeys and whether the right timestamp is not
		// older than the `ChannelPruneExpiry`.
		//
		// NOTE: The timestamp data has no verification attached to it
		// in the `ReplyChannelRange` msg so we are trusting this data
		// at this point. However it is not critical because we are just
		// removing the channel from the db when the timestamps are more
		// recent. During the querying of the gossip msg verification
		// happens as usual. However we should start punishing peers
		// when they don't provide us honest data ?
		isStillZombie := isZombieChan(
			info.Node1UpdateTimestamp, info.Node2UpdateTimestamp,
		)

		if isStillZombie {
			continue
		}

		// If we have marked it as a zombie but the latest update
		// timestamps could bring it back from the dead, then we mark it
		// alive, and we let it be added to the set of IDs to query our
		// peer for.
		err := c.KVStore.MarkEdgeLive(
			info.ShortChannelID.ToUint64(),
		)
		// Since there is a chance that the edge could have been marked
		// as "live" between the FilterKnownChanIDs call and the
		// MarkEdgeLive call, we ignore the error if the edge is already
		// marked as live.
		if err != nil && !errors.Is(err, ErrZombieEdgeNotFound) {
			return nil, err
		}
	}

	return unknown, nil
}

// MarkEdgeZombie attempts to mark a channel identified by its channel ID as a
// zombie. This method is used on an ad-hoc basis, when channels need to be
// marked as zombies outside the normal pruning cycle.
func (c *ChannelGraph) MarkEdgeZombie(chanID uint64,
	pubKey1, pubKey2 [33]byte) error {

	err := c.KVStore.MarkEdgeZombie(chanID, pubKey1, pubKey2)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.RemoveChannel(pubKey1, pubKey2, chanID)
	}

	return nil
}

// UpdateEdgePolicy updates the edge routing policy for a single directed edge
// within the database for the referenced channel. The `flags` attribute within
// the ChannelEdgePolicy determines which of the directed edges are being
// updated. If the flag is 1, then the first node's information is being
// updated, otherwise it's the second node's information. The node ordering is
// determined by the lexicographical ordering of the identity public keys of the
// nodes on either side of the channel.
func (c *ChannelGraph) UpdateEdgePolicy(edge *models.ChannelEdgePolicy,
	op ...batch.SchedulerOption) error {

	from, to, err := c.KVStore.UpdateEdgePolicy(edge, op...)
	if err != nil {
		return err
	}

	if c.graphCache == nil {
		return nil
	}

	var isUpdate1 bool
	if edge.ChannelFlags&lnwire.ChanUpdateDirection == 0 {
		isUpdate1 = true
	}

	c.graphCache.UpdatePolicy(edge, from, to, isUpdate1)

	return nil
}
