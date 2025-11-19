package graphdb

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// ErrChanGraphShuttingDown indicates that the ChannelGraph has shutdown or is
// busy shutting down.
var ErrChanGraphShuttingDown = fmt.Errorf("ChannelGraph shutting down")

// ChannelGraph is a layer above the graph's CRUD layer.
type ChannelGraph struct {
	started atomic.Bool
	stopped atomic.Bool

	graphCache *GraphCache

	db Store
	*topologyManager

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewChannelGraph creates a new ChannelGraph instance with the given backend.
func NewChannelGraph(v1Store Store,
	options ...ChanGraphOption) (*ChannelGraph, error) {

	opts := defaultChanGraphOptions()
	for _, o := range options {
		o(opts)
	}

	g := &ChannelGraph{
		db:              v1Store,
		topologyManager: newTopologyManager(),
		quit:            make(chan struct{}),
	}

	// The graph cache can be turned off (e.g. for mobile users) for a
	// speed/memory usage tradeoff.
	if opts.useGraphCache {
		g.graphCache = NewGraphCache(opts.preAllocCacheNumNodes)
	}

	return g, nil
}

// Start kicks off any goroutines required for the ChannelGraph to function.
// If the graph cache is enabled, then it will be populated with the contents of
// the database.
func (c *ChannelGraph) Start() error {
	if !c.started.CompareAndSwap(false, true) {
		return nil
	}
	log.Debugf("ChannelGraph starting")
	defer log.Debug("ChannelGraph started")

	if c.graphCache != nil {
		if err := c.populateCache(context.TODO()); err != nil {
			return fmt.Errorf("could not populate the graph "+
				"cache: %w", err)
		}
	}

	c.wg.Add(1)
	go c.handleTopologySubscriptions()

	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (c *ChannelGraph) Stop() error {
	if !c.stopped.CompareAndSwap(false, true) {
		return nil
	}

	log.Debugf("ChannelGraph shutting down...")
	defer log.Debug("ChannelGraph shutdown complete")

	close(c.quit)
	c.wg.Wait()

	return nil
}

// handleTopologySubscriptions ensures that topology client subscriptions,
// subscription cancellations and topology notifications are handled
// synchronously.
//
// NOTE: this MUST be run in a goroutine.
func (c *ChannelGraph) handleTopologySubscriptions() {
	defer c.wg.Done()

	for {
		select {
		// A new fully validated topology update has just arrived.
		// We'll notify any registered clients.
		case update := <-c.topologyUpdate:
			// TODO(elle): change topology handling to be handled
			// synchronously so that we can guarantee the order of
			// notification delivery.
			c.wg.Add(1)
			go c.handleTopologyUpdate(update)

			// TODO(roasbeef): remove all unconnected vertexes
			// after N blocks pass with no corresponding
			// announcements.

		// A new notification client update has arrived. We're either
		// gaining a new client, or cancelling notifications for an
		// existing client.
		case ntfnUpdate := <-c.ntfnClientUpdates:
			clientID := ntfnUpdate.clientID

			if ntfnUpdate.cancel {
				client, ok := c.topologyClients.LoadAndDelete(
					clientID,
				)
				if ok {
					close(client.exit)
					client.wg.Wait()

					close(client.ntfnChan)
				}

				continue
			}

			c.topologyClients.Store(clientID, &topologyClient{
				ntfnChan: ntfnUpdate.ntfnChan,
				exit:     make(chan struct{}),
			})

		case <-c.quit:
			return
		}
	}
}

// populateCache loads the entire channel graph into the in-memory graph cache.
//
// NOTE: This should only be called if the graphCache has been constructed.
func (c *ChannelGraph) populateCache(ctx context.Context) error {
	startTime := time.Now()
	log.Info("Populating in-memory channel graph, this might take a " +
		"while...")

	err := c.db.ForEachNodeCacheable(ctx, func(node route.Vertex,
		features *lnwire.FeatureVector) error {

		c.graphCache.AddNodeFeatures(node, features)

		return nil
	}, func() {})
	if err != nil {
		return err
	}

	err = c.db.ForEachChannelCacheable(
		func(info *models.CachedEdgeInfo,
			policy1, policy2 *models.CachedEdgePolicy) error {

			c.graphCache.AddChannel(info, policy1, policy2)

			return nil
		}, func() {},
	)
	if err != nil {
		return err
	}

	log.Infof("Finished populating in-memory channel graph (took %v, %s)",
		time.Since(startTime), c.graphCache.Stats())

	return nil
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
	cb func(channel *DirectedChannel) error, reset func()) error {

	if c.graphCache != nil {
		return c.graphCache.ForEachChannel(node, cb)
	}

	return c.db.ForEachNodeDirectedChannel(node, cb, reset)
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

	return c.db.FetchNodeFeatures(lnwire.GossipVersion1, node)
}

// GraphSession will provide the call-back with access to a NodeTraverser
// instance which can be used to perform queries against the channel graph. If
// the graph cache is not enabled, then the call-back will be provided with
// access to the graph via a consistent read-only transaction.
func (c *ChannelGraph) GraphSession(cb func(graph NodeTraverser) error,
	reset func()) error {

	if c.graphCache != nil {
		return cb(c)
	}

	return c.db.GraphSession(cb, reset)
}

// ForEachNodeCached iterates through all the stored vertices/nodes in the
// graph, executing the passed callback with each node encountered.
//
// NOTE: The callback contents MUST not be modified.
func (c *ChannelGraph) ForEachNodeCached(ctx context.Context, withAddrs bool,
	cb func(ctx context.Context, node route.Vertex, addrs []net.Addr,
		chans map[uint64]*DirectedChannel) error, reset func()) error {

	if !withAddrs && c.graphCache != nil {
		return c.graphCache.ForEachNode(
			func(node route.Vertex,
				channels map[uint64]*DirectedChannel) error {

				return cb(ctx, node, nil, channels)
			},
		)
	}

	return c.db.ForEachNodeCached(ctx, withAddrs, cb, reset)
}

// AddNode adds a vertex/node to the graph database. If the node is not
// in the database from before, this will add a new, unconnected one to the
// graph. If it is present from before, this will update that node's
// information. Note that this method is expected to only be called to update an
// already present node from a node announcement, or to insert a node found in a
// channel update.
func (c *ChannelGraph) AddNode(ctx context.Context,
	node *models.Node, op ...batch.SchedulerOption) error {

	err := c.db.AddNode(ctx, node, op...)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.AddNodeFeatures(
			node.PubKeyBytes, node.Features,
		)
	}

	select {
	case c.topologyUpdate <- node:
	case <-c.quit:
		return ErrChanGraphShuttingDown
	}

	return nil
}

// AddChannelEdge adds a new (undirected, blank) edge to the graph database. An
// undirected edge from the two target nodes are created. The information stored
// denotes the static attributes of the channel, such as the channelID, the keys
// involved in creation of the channel, and the set of features that the channel
// supports. The chanPoint and chanID are used to uniquely identify the edge
// globally within the database.
func (c *ChannelGraph) AddChannelEdge(ctx context.Context,
	edge *models.ChannelEdgeInfo, op ...batch.SchedulerOption) error {

	err := c.db.AddChannelEdge(ctx, edge, op...)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.AddChannel(models.NewCachedEdge(edge), nil, nil)
	}

	select {
	case c.topologyUpdate <- edge:
	case <-c.quit:
		return ErrChanGraphShuttingDown
	}

	return nil
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
// If the cache is enabled, the edge will be added back to the graph cache if
// we still have a record of this channel in the DB.
func (c *ChannelGraph) MarkEdgeLive(chanID uint64) error {
	err := c.db.MarkEdgeLive(chanID)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		// We need to add the channel back into our graph cache,
		// otherwise we won't use it for path finding.
		infos, err := c.db.FetchChanInfos([]uint64{chanID})
		if err != nil {
			return err
		}

		if len(infos) == 0 {
			return nil
		}

		info := infos[0]

		var policy1, policy2 *models.CachedEdgePolicy
		if info.Policy1 != nil {
			policy1 = models.NewCachedPolicy(info.Policy1)
		}
		if info.Policy2 != nil {
			policy2 = models.NewCachedPolicy(info.Policy2)
		}

		c.graphCache.AddChannel(
			models.NewCachedEdge(info.Info), policy1, policy2,
		)
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
// denotes whether to mark the channel as a zombie.
func (c *ChannelGraph) DeleteChannelEdges(strictZombiePruning, markZombie bool,
	chanIDs ...uint64) error {

	infos, err := c.db.DeleteChannelEdges(
		lnwire.GossipVersion1, strictZombiePruning, markZombie,
		chanIDs...,
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

	edges, err := c.db.DisconnectBlockAtHeight(height)
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

	edges, nodes, err := c.db.PruneGraph(
		spentOutputs, blockHash, blockHeight,
	)
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

	if len(edges) != 0 {
		// Notify all currently registered clients of the newly closed
		// channels.
		closeSummaries := createCloseSummaries(
			blockHeight, edges...,
		)

		select {
		case c.topologyUpdate <- closeSummaries:
		case <-c.quit:
			return nil, ErrChanGraphShuttingDown
		}
	}

	return edges, nil
}

// PruneGraphNodes is a garbage collection method which attempts to prune out
// any nodes from the channel graph that are currently unconnected. This ensure
// that we only maintain a graph of reachable nodes. In the event that a pruned
// node gains more channels, it will be re-added back to the graph.
func (c *ChannelGraph) PruneGraphNodes() error {
	nodes, err := c.db.PruneGraphNodes()
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

	unknown, knownZombies, err := c.db.FilterKnownChanIDs(chansInfo)
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
		err := c.db.MarkEdgeLive(
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

	err := c.db.MarkEdgeZombie(chanID, pubKey1, pubKey2)
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
func (c *ChannelGraph) UpdateEdgePolicy(ctx context.Context,
	edge *models.ChannelEdgePolicy, op ...batch.SchedulerOption) error {

	from, to, err := c.db.UpdateEdgePolicy(ctx, edge, op...)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.UpdatePolicy(
			models.NewCachedPolicy(edge), from, to,
		)
	}

	select {
	case c.topologyUpdate <- edge:
	case <-c.quit:
		return ErrChanGraphShuttingDown
	}

	return nil
}

// ForEachSourceNodeChannel iterates through all channels of the source node.
func (c *ChannelGraph) ForEachSourceNodeChannel(ctx context.Context,
	cb func(chanPoint wire.OutPoint, havePolicy bool,
		otherNode *models.Node) error, reset func()) error {

	return c.db.ForEachSourceNodeChannel(ctx, cb, reset)
}

// ForEachNodeChannel iterates through all channels of the given node.
func (c *ChannelGraph) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	return c.db.ForEachNodeChannel(ctx, nodePub, cb, reset)
}

// ForEachNode iterates through all stored vertices/nodes in the graph.
func (c *ChannelGraph) ForEachNode(ctx context.Context,
	cb func(*models.Node) error, reset func()) error {

	return c.db.ForEachNode(ctx, cb, reset)
}

// ForEachNodeCacheable iterates through all stored vertices/nodes in the graph.
func (c *ChannelGraph) ForEachNodeCacheable(ctx context.Context,
	cb func(route.Vertex, *lnwire.FeatureVector) error,
	reset func()) error {

	return c.db.ForEachNodeCacheable(ctx, cb, reset)
}

// NodeUpdatesInHorizon returns all known lightning nodes with updates in the
// range.
func (c *ChannelGraph) NodeUpdatesInHorizon(startTime, endTime time.Time,
	opts ...IteratorOption) iter.Seq2[*models.Node, error] {

	return c.db.NodeUpdatesInHorizon(startTime, endTime, opts...)
}

// HasV1Node determines if the graph has a vertex identified by the target node
// in the V1 graph.
func (c *ChannelGraph) HasV1Node(ctx context.Context,
	nodePub [33]byte) (time.Time, bool, error) {

	return c.db.HasV1Node(ctx, nodePub)
}

// IsPublicNode determines whether the node is seen as public in the graph.
func (c *ChannelGraph) IsPublicNode(pubKey [33]byte) (bool, error) {
	return c.db.IsPublicNode(pubKey)
}

// ForEachChannel iterates through all channel edges stored within the graph.
func (c *ChannelGraph) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	return c.db.ForEachChannel(ctx, cb, reset)
}

// ForEachChannelCacheable iterates through all channel edges for the cache.
func (c *ChannelGraph) ForEachChannelCacheable(cb func(*models.CachedEdgeInfo,
	*models.CachedEdgePolicy, *models.CachedEdgePolicy) error,
	reset func()) error {

	return c.db.ForEachChannelCacheable(cb, reset)
}

// DisabledChannelIDs returns the channel ids of disabled channels.
func (c *ChannelGraph) DisabledChannelIDs() ([]uint64, error) {
	return c.db.DisabledChannelIDs()
}

// HasChannelEdge returns true if the database knows of a channel edge.
func (c *ChannelGraph) HasChannelEdge(chanID uint64) (time.Time, time.Time,
	bool, bool, error) {

	return c.db.HasChannelEdge(chanID)
}

// AddEdgeProof sets the proof of an existing edge in the graph database.
func (c *ChannelGraph) AddEdgeProof(chanID lnwire.ShortChannelID,
	proof *models.ChannelAuthProof) error {

	return c.db.AddEdgeProof(chanID, proof)
}

// ChannelID attempts to lookup the 8-byte compact channel ID.
func (c *ChannelGraph) ChannelID(chanPoint *wire.OutPoint) (uint64, error) {
	return c.db.ChannelID(chanPoint)
}

// HighestChanID returns the "highest" known channel ID in the channel graph.
func (c *ChannelGraph) HighestChanID(ctx context.Context) (uint64, error) {
	return c.db.HighestChanID(ctx)
}

// ChanUpdatesInHorizon returns all known channel edges with updates in the
// horizon.
func (c *ChannelGraph) ChanUpdatesInHorizon(startTime, endTime time.Time,
	opts ...IteratorOption) iter.Seq2[ChannelEdge, error] {

	return c.db.ChanUpdatesInHorizon(startTime, endTime, opts...)
}

// FilterChannelRange returns channel IDs within the passed block height range.
func (c *ChannelGraph) FilterChannelRange(startHeight, endHeight uint32,
	withTimestamps bool) ([]BlockChannelRange, error) {

	return c.db.FilterChannelRange(startHeight, endHeight, withTimestamps)
}

// FetchChanInfos returns the set of channel edges for the passed channel IDs.
func (c *ChannelGraph) FetchChanInfos(chanIDs []uint64) ([]ChannelEdge, error) {
	return c.db.FetchChanInfos(chanIDs)
}

// FetchChannelEdgesByOutpoint attempts to lookup directed edges by funding
// outpoint.
func (c *ChannelGraph) FetchChannelEdgesByOutpoint(op *wire.OutPoint) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return c.db.FetchChannelEdgesByOutpoint(op)
}

// FetchChannelEdgesByID attempts to lookup directed edges by channel ID.
func (c *ChannelGraph) FetchChannelEdgesByID(chanID uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return c.db.FetchChannelEdgesByID(chanID)
}

// ChannelView returns the verifiable edge information for each active channel.
func (c *ChannelGraph) ChannelView() ([]EdgePoint, error) {
	return c.db.ChannelView()
}

// IsZombieEdge returns whether the edge is considered zombie.
func (c *ChannelGraph) IsZombieEdge(chanID uint64) (bool, [33]byte, [33]byte,
	error) {

	return c.db.IsZombieEdge(chanID)
}

// NumZombies returns the current number of zombie channels in the graph.
func (c *ChannelGraph) NumZombies() (uint64, error) {
	return c.db.NumZombies()
}

// PutClosedScid stores a SCID for a closed channel in the database.
func (c *ChannelGraph) PutClosedScid(scid lnwire.ShortChannelID) error {
	return c.db.PutClosedScid(scid)
}

// IsClosedScid checks whether a channel identified by the scid is closed.
func (c *ChannelGraph) IsClosedScid(scid lnwire.ShortChannelID) (bool, error) {
	return c.db.IsClosedScid(scid)
}

// SetSourceNode sets the source node within the graph database.
func (c *ChannelGraph) SetSourceNode(ctx context.Context,
	node *models.Node) error {

	return c.db.SetSourceNode(ctx, node)
}

// PruneTip returns the block height and hash of the latest pruning block.
func (c *ChannelGraph) PruneTip() (*chainhash.Hash, uint32, error) {
	return c.db.PruneTip()
}

// VersionedGraph is a wrapper around ChannelGraph that will call underlying
// Store methods with a specific gossip version.
type VersionedGraph struct {
	*ChannelGraph
	v lnwire.GossipVersion
}

// NewVersionedGraph creates a new VersionedGraph.
func NewVersionedGraph(c *ChannelGraph,
	v lnwire.GossipVersion) *VersionedGraph {

	return &VersionedGraph{
		ChannelGraph: c,
		v:            v,
	}
}

// FetchNode attempts to look up a target node by its identity public key.
func (c *VersionedGraph) FetchNode(ctx context.Context,
	nodePub route.Vertex) (*models.Node, error) {

	return c.db.FetchNode(ctx, c.v, nodePub)
}

// AddrsForNode returns all known addresses for the target node public key.
func (c *VersionedGraph) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	return c.db.AddrsForNode(ctx, c.v, nodePub)
}

// DeleteNode starts a new database transaction to remove a vertex/node
// from the database according to the node's public key.
func (c *VersionedGraph) DeleteNode(ctx context.Context,
	nodePub route.Vertex) error {

	err := c.db.DeleteNode(ctx, c.v, nodePub)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.RemoveNode(nodePub)
	}

	return nil
}

// HasNode determines if the graph has a vertex identified by the target node
// in the V1 graph.
func (c *VersionedGraph) HasNode(ctx context.Context, nodePub [33]byte) (bool,
	error) {

	return c.db.HasNode(ctx, c.v, nodePub)
}

// LookupAlias attempts to return the alias as advertised by the target node.
func (c *VersionedGraph) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	return c.db.LookupAlias(ctx, c.v, pub)
}

// SourceNode returns the source node of the graph.
func (c *VersionedGraph) SourceNode(ctx context.Context) (*models.Node,
	error) {

	return c.db.SourceNode(ctx, c.v)
}

// DeleteChannelEdges removes edges with the given channel IDs from the
// database and marks them as zombies. This ensures that we're unable to re-add
// it to our database once again. If an edge does not exist within the
// database, then ErrEdgeNotFound will be returned. If strictZombiePruning is
// true, then when we mark these edges as zombies, we'll set up the keys such
// that we require the node that failed to send the fresh update to be the one
// that resurrects the channel from its zombie state. The markZombie bool
// denotes whether to mark the channel as a zombie.
func (c *VersionedGraph) DeleteChannelEdges(strictZombiePruning,
	markZombie bool, chanIDs ...uint64) error {

	infos, err := c.db.DeleteChannelEdges(
		c.v, strictZombiePruning, markZombie, chanIDs...,
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

// MakeTestGraph creates a new instance of the ChannelGraph for testing
// purposes. The backing Store implementation depends on the version of
// NewTestDB included in the current build.
//
// NOTE: this is currently unused, but is left here for future use to show how
// NewTestDB can be used. As the SQL implementation of the Store is
// implemented, unit tests will be switched to use this function instead of
// the existing MakeTestGraph helper. Once only this function is used, the
// existing MakeTestGraph function will be removed and this one will be renamed.
func MakeTestGraph(t testing.TB,
	opts ...ChanGraphOption) *ChannelGraph {

	t.Helper()

	store := NewTestDB(t)

	graph, err := NewChannelGraph(store, opts...)
	require.NoError(t, err)
	require.NoError(t, graph.Start())

	t.Cleanup(func() {
		require.NoError(t, graph.Stop())
	})

	return graph
}
