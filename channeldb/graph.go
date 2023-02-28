package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"io"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// nodeBucket is a bucket which houses all the vertices or nodes within
	// the channel graph. This bucket has a single-sub bucket which adds an
	// additional index from pubkey -> alias. Within the top-level of this
	// bucket, the key space maps a node's compressed public key to the
	// serialized information for that node. Additionally, there's a
	// special key "source" which stores the pubkey of the source node. The
	// source node is used as the starting point for all graph/queries and
	// traversals. The graph is formed as a star-graph with the source node
	// at the center.
	//
	// maps: pubKey -> nodeInfo
	// maps: source -> selfPubKey
	nodeBucket = []byte("graph-node")

	// nodeUpdateIndexBucket is a sub-bucket of the nodeBucket. This bucket
	// will be used to quickly look up the "freshness" of a node's last
	// update to the network. The bucket only contains keys, and no values,
	// it's mapping:
	//
	// maps: updateTime || nodeID -> nil
	nodeUpdateIndexBucket = []byte("graph-node-update-index")

	// sourceKey is a special key that resides within the nodeBucket. The
	// sourceKey maps a key to the public key of the "self node".
	sourceKey = []byte("source")

	// aliasIndexBucket is a sub-bucket that's nested within the main
	// nodeBucket. This bucket maps the public key of a node to its
	// current alias. This bucket is provided as it can be used within a
	// future UI layer to add an additional degree of confirmation.
	aliasIndexBucket = []byte("alias")

	// edgeBucket is a bucket which houses all of the edge or channel
	// information within the channel graph. This bucket essentially acts
	// as an adjacency list, which in conjunction with a range scan, can be
	// used to iterate over all the incoming and outgoing edges for a
	// particular node. Key in the bucket use a prefix scheme which leads
	// with the node's public key and sends with the compact edge ID.
	// For each chanID, there will be two entries within the bucket, as the
	// graph is directed: nodes may have different policies w.r.t to fees
	// for their respective directions.
	//
	// maps: pubKey || chanID -> channel edge policy for node
	edgeBucket = []byte("graph-edge")

	// unknownPolicy is represented as an empty slice. It is
	// used as the value in edgeBucket for unknown channel edge policies.
	// Unknown policies are still stored in the database to enable efficient
	// lookup of incoming channel edges.
	unknownPolicy = []byte{}

	// chanStart is an array of all zero bytes which is used to perform
	// range scans within the edgeBucket to obtain all of the outgoing
	// edges for a particular node.
	chanStart [8]byte

	// edgeIndexBucket is an index which can be used to iterate all edges
	// in the bucket, grouping them according to their in/out nodes.
	// Additionally, the items in this bucket also contain the complete
	// edge information for a channel. The edge information includes the
	// capacity of the channel, the nodes that made the channel, etc. This
	// bucket resides within the edgeBucket above. Creation of an edge
	// proceeds in two phases: first the edge is added to the edge index,
	// afterwards the edgeBucket can be updated with the latest details of
	// the edge as they are announced on the network.
	//
	// maps: chanID -> pubKey1 || pubKey2 || restofEdgeInfo
	edgeIndexBucket = []byte("edge-index")

	// edgeUpdateIndexBucket is a sub-bucket of the main edgeBucket. This
	// bucket contains an index which allows us to gauge the "freshness" of
	// a channel's last updates.
	//
	// maps: updateTime || chanID -> nil
	edgeUpdateIndexBucket = []byte("edge-update-index")

	// channelPointBucket maps a channel's full outpoint (txid:index) to
	// its short 8-byte channel ID. This bucket resides within the
	// edgeBucket above, and can be used to quickly remove an edge due to
	// the outpoint being spent, or to query for existence of a channel.
	//
	// maps: outPoint -> chanID
	channelPointBucket = []byte("chan-index")

	// zombieBucket is a sub-bucket of the main edgeBucket bucket
	// responsible for maintaining an index of zombie channels. Each entry
	// exists within the bucket as follows:
	//
	// maps: chanID -> pubKey1 || pubKey2
	//
	// The chanID represents the channel ID of the edge that is marked as a
	// zombie and is used as the key, which maps to the public keys of the
	// edge's participants.
	zombieBucket = []byte("zombie-index")

	// disabledEdgePolicyBucket is a sub-bucket of the main edgeBucket bucket
	// responsible for maintaining an index of disabled edge policies. Each
	// entry exists within the bucket as follows:
	//
	// maps: <chanID><direction> -> []byte{}
	//
	// The chanID represents the channel ID of the edge and the direction is
	// one byte representing the direction of the edge. The main purpose of
	// this index is to allow pruning disabled channels in a fast way without
	// the need to iterate all over the graph.
	disabledEdgePolicyBucket = []byte("disabled-edge-policy-index")

	// graphMetaBucket is a top-level bucket which stores various meta-deta
	// related to the on-disk channel graph. Data stored in this bucket
	// includes the block to which the graph has been synced to, the total
	// number of channels, etc.
	graphMetaBucket = []byte("graph-meta")

	// pruneLogBucket is a bucket within the graphMetaBucket that stores
	// a mapping from the block height to the hash for the blocks used to
	// prune the graph.
	// Once a new block is discovered, any channels that have been closed
	// (by spending the outpoint) can safely be removed from the graph, and
	// the block is added to the prune log. We need to keep such a log for
	// the case where a reorg happens, and we must "rewind" the state of the
	// graph by removing channels that were previously confirmed. In such a
	// case we'll remove all entries from the prune log with a block height
	// that no longer exists.
	pruneLogBucket = []byte("prune-log")
)

const (
	// MaxAllowedExtraOpaqueBytes is the largest amount of opaque bytes that
	// we'll permit to be written to disk. We limit this as otherwise, it
	// would be possible for a node to create a ton of updates and slowly
	// fill our disk, and also waste bandwidth due to relaying.
	MaxAllowedExtraOpaqueBytes = 10000

	// feeRateParts is the total number of parts used to express fee rates.
	feeRateParts = 1e6
)

// ChannelGraph is a persistent, on-disk graph representation of the Lightning
// Network. This struct can be used to implement path finding algorithms on top
// of, and also to update a node's view based on information received from the
// p2p network. Internally, the graph is stored using a modified adjacency list
// representation with some added object interaction possible with each
// serialized edge/node. The graph is stored is directed, meaning that are two
// edges stored for each channel: an inbound/outbound edge for each node pair.
// Nodes, edges, and edge information can all be added to the graph
// independently. Edge removal results in the deletion of all edge information
// for that edge.
type ChannelGraph struct {
	db kvdb.Backend

	cacheMu     sync.RWMutex
	rejectCache *rejectCache
	chanCache   *channelCache
	graphCache  *GraphCache

	chanScheduler batch.Scheduler
	nodeScheduler batch.Scheduler
}

// NewChannelGraph allocates a new ChannelGraph backed by a DB instance. The
// returned instance has its own unique reject cache and channel cache.
func NewChannelGraph(db kvdb.Backend, rejectCacheSize, chanCacheSize int,
	batchCommitInterval time.Duration, preAllocCacheNumNodes int,
	useGraphCache, noMigrations bool) (*ChannelGraph, error) {

	if !noMigrations {
		if err := initChannelGraph(db); err != nil {
			return nil, err
		}
	}

	g := &ChannelGraph{
		db:          db,
		rejectCache: newRejectCache(rejectCacheSize),
		chanCache:   newChannelCache(chanCacheSize),
	}
	g.chanScheduler = batch.NewTimeScheduler(
		db, &g.cacheMu, batchCommitInterval,
	)
	g.nodeScheduler = batch.NewTimeScheduler(
		db, nil, batchCommitInterval,
	)

	// The graph cache can be turned off (e.g. for mobile users) for a
	// speed/memory usage tradeoff.
	if useGraphCache {
		g.graphCache = NewGraphCache(preAllocCacheNumNodes)
		startTime := time.Now()
		log.Debugf("Populating in-memory channel graph, this might " +
			"take a while...")

		err := g.ForEachNodeCacheable(
			func(tx kvdb.RTx, node GraphCacheNode) error {
				g.graphCache.AddNodeFeatures(node)

				return nil
			},
		)
		if err != nil {
			return nil, err
		}

		err = g.ForEachChannel(func(info *ChannelEdgeInfo,
			policy1, policy2 *ChannelEdgePolicy) error {

			g.graphCache.AddChannel(info, policy1, policy2)

			return nil
		})
		if err != nil {
			return nil, err
		}

		log.Debugf("Finished populating in-memory channel graph (took "+
			"%v, %s)", time.Since(startTime), g.graphCache.Stats())
	}

	return g, nil
}

// channelMapKey is the key structure used for storing channel edge policies.
type channelMapKey struct {
	nodeKey route.Vertex
	chanID  [8]byte
}

// getChannelMap loads all channel edge policies from the database and stores
// them in a map.
func (c *ChannelGraph) getChannelMap(edges kvdb.RBucket) (
	map[channelMapKey]*ChannelEdgePolicy, error) {

	// Create a map to store all channel edge policies.
	channelMap := make(map[channelMapKey]*ChannelEdgePolicy)

	err := kvdb.ForAll(edges, func(k, edgeBytes []byte) error {
		// Skip embedded buckets.
		if bytes.Equal(k, edgeIndexBucket) ||
			bytes.Equal(k, edgeUpdateIndexBucket) ||
			bytes.Equal(k, zombieBucket) ||
			bytes.Equal(k, disabledEdgePolicyBucket) ||
			bytes.Equal(k, channelPointBucket) {

			return nil
		}

		// Validate key length.
		if len(k) != 33+8 {
			return fmt.Errorf("invalid edge key %x encountered", k)
		}

		var key channelMapKey
		copy(key.nodeKey[:], k[:33])
		copy(key.chanID[:], k[33:])

		// No need to deserialize unknown policy.
		if bytes.Equal(edgeBytes, unknownPolicy) {
			return nil
		}

		edgeReader := bytes.NewReader(edgeBytes)
		edge, err := deserializeChanEdgePolicyRaw(
			edgeReader,
		)

		switch {
		// If the db policy was missing an expected optional field, we
		// return nil as if the policy was unknown.
		case err == ErrEdgePolicyOptionalFieldNotFound:
			return nil

		case err != nil:
			return err
		}

		channelMap[key] = edge

		return nil
	})
	if err != nil {
		return nil, err
	}

	return channelMap, nil
}

var graphTopLevelBuckets = [][]byte{
	nodeBucket,
	edgeBucket,
	edgeIndexBucket,
	graphMetaBucket,
}

// Wipe completely deletes all saved state within all used buckets within the
// database. The deletion is done in a single transaction, therefore this
// operation is fully atomic.
func (c *ChannelGraph) Wipe() error {
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		for _, tlb := range graphTopLevelBuckets {
			err := tx.DeleteTopLevelBucket(tlb)
			if err != nil && err != kvdb.ErrBucketNotFound {
				return err
			}
		}
		return nil
	}, func() {})
	if err != nil {
		return err
	}

	return initChannelGraph(c.db)
}

// createChannelDB creates and initializes a fresh version of channeldb. In
// the case that the target path has not yet been created or doesn't yet exist,
// then the path is created. Additionally, all required top-level buckets used
// within the database are created.
func initChannelGraph(db kvdb.Backend) error {
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		for _, tlb := range graphTopLevelBuckets {
			if _, err := tx.CreateTopLevelBucket(tlb); err != nil {
				return err
			}
		}

		nodes := tx.ReadWriteBucket(nodeBucket)
		_, err := nodes.CreateBucketIfNotExists(aliasIndexBucket)
		if err != nil {
			return err
		}
		_, err = nodes.CreateBucketIfNotExists(nodeUpdateIndexBucket)
		if err != nil {
			return err
		}

		edges := tx.ReadWriteBucket(edgeBucket)
		_, err = edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		_, err = edges.CreateBucketIfNotExists(edgeUpdateIndexBucket)
		if err != nil {
			return err
		}
		_, err = edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}
		_, err = edges.CreateBucketIfNotExists(zombieBucket)
		if err != nil {
			return err
		}

		graphMeta := tx.ReadWriteBucket(graphMetaBucket)
		_, err = graphMeta.CreateBucketIfNotExists(pruneLogBucket)
		return err
	}, func() {})
	if err != nil {
		return fmt.Errorf("unable to create new channel graph: %v", err)
	}

	return nil
}

// NewPathFindTx returns a new read transaction that can be used for a single
// path finding session. Will return nil if the graph cache is enabled.
func (c *ChannelGraph) NewPathFindTx() (kvdb.RTx, error) {
	if c.graphCache != nil {
		return nil, nil
	}

	return c.db.BeginReadTx()
}

// ForEachChannel iterates through all the channel edges stored within the
// graph and invokes the passed callback for each edge. The callback takes two
// edges as since this is a directed graph, both the in/out edges are visited.
// If the callback returns an error, then the transaction is aborted and the
// iteration stops early.
//
// NOTE: If an edge can't be found, or wasn't advertised, then a nil pointer
// for that particular channel edge routing policy will be passed into the
// callback.
func (c *ChannelGraph) ForEachChannel(cb func(*ChannelEdgeInfo,
	*ChannelEdgePolicy, *ChannelEdgePolicy) error) error {

	return c.db.View(func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}

		// First, load all edges in memory indexed by node and channel
		// id.
		channelMap, err := c.getChannelMap(edges)
		if err != nil {
			return err
		}

		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// Load edge index, recombine each channel with the policies
		// loaded above and invoke the callback.
		return kvdb.ForAll(edgeIndex, func(k, edgeInfoBytes []byte) error {
			var chanID [8]byte
			copy(chanID[:], k)

			edgeInfoReader := bytes.NewReader(edgeInfoBytes)
			info, err := deserializeChanEdgeInfo(edgeInfoReader)
			if err != nil {
				return err
			}

			policy1 := channelMap[channelMapKey{
				nodeKey: info.NodeKey1Bytes,
				chanID:  chanID,
			}]

			policy2 := channelMap[channelMapKey{
				nodeKey: info.NodeKey2Bytes,
				chanID:  chanID,
			}]

			return cb(&info, policy1, policy2)
		})
	}, func() {})
}

// ForEachNodeChannel iterates through all channels of a given node, executing the
// passed callback with an edge info structure and the policies of each end
// of the channel. The first edge policy is the outgoing edge *to* the
// connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
func (c *ChannelGraph) ForEachNodeChannel(tx kvdb.RTx, node route.Vertex,
	cb func(channel *DirectedChannel) error) error {

	if c.graphCache != nil {
		return c.graphCache.ForEachChannel(node, cb)
	}

	// Fallback that uses the database.
	toNodeCallback := func() route.Vertex {
		return node
	}
	toNodeFeatures, err := c.FetchNodeFeatures(node)
	if err != nil {
		return err
	}

	dbCallback := func(tx kvdb.RTx, e *ChannelEdgeInfo, p1,
		p2 *ChannelEdgePolicy) error {

		var cachedInPolicy *CachedEdgePolicy
		if p2 != nil {
			cachedInPolicy = NewCachedPolicy(p2)
			cachedInPolicy.ToNodePubKey = toNodeCallback
			cachedInPolicy.ToNodeFeatures = toNodeFeatures
		}

		directedChannel := &DirectedChannel{
			ChannelID:    e.ChannelID,
			IsNode1:      node == e.NodeKey1Bytes,
			OtherNode:    e.NodeKey2Bytes,
			Capacity:     e.Capacity,
			OutPolicySet: p1 != nil,
			InPolicy:     cachedInPolicy,
		}

		if node == e.NodeKey2Bytes {
			directedChannel.OtherNode = e.NodeKey1Bytes
		}

		return cb(directedChannel)
	}
	return nodeTraversal(tx, node[:], c.db, dbCallback)
}

// FetchNodeFeatures returns the features of a given node. If no features are
// known for the node, an empty feature vector is returned.
func (c *ChannelGraph) FetchNodeFeatures(
	node route.Vertex) (*lnwire.FeatureVector, error) {

	if c.graphCache != nil {
		return c.graphCache.GetFeatures(node), nil
	}

	// Fallback that uses the database.
	targetNode, err := c.FetchLightningNode(node)
	switch err {
	// If the node exists and has features, return them directly.
	case nil:
		return targetNode.Features, nil

	// If we couldn't find a node announcement, populate a blank feature
	// vector.
	case ErrGraphNodeNotFound:
		return lnwire.EmptyFeatureVector(), nil

	// Otherwise, bubble the error up.
	default:
		return nil, err
	}
}

// ForEachNodeCached is similar to ForEachNode, but it utilizes the channel
// graph cache instead. Note that this doesn't return all the information the
// regular ForEachNode method does.
//
// NOTE: The callback contents MUST not be modified.
func (c *ChannelGraph) ForEachNodeCached(cb func(node route.Vertex,
	chans map[uint64]*DirectedChannel) error) error {

	if c.graphCache != nil {
		return c.graphCache.ForEachNode(cb)
	}

	// Otherwise call back to a version that uses the database directly.
	// We'll iterate over each node, then the set of channels for each
	// node, and construct a similar callback functiopn signature as the
	// main funcotin expects.
	return c.ForEachNode(func(tx kvdb.RTx, node *LightningNode) error {
		channels := make(map[uint64]*DirectedChannel)

		err := node.ForEachChannel(tx, func(tx kvdb.RTx,
			e *ChannelEdgeInfo, p1 *ChannelEdgePolicy,
			p2 *ChannelEdgePolicy) error {

			toNodeCallback := func() route.Vertex {
				return node.PubKeyBytes
			}
			toNodeFeatures, err := c.FetchNodeFeatures(
				node.PubKeyBytes,
			)
			if err != nil {
				return err
			}

			var cachedInPolicy *CachedEdgePolicy
			if p2 != nil {
				cachedInPolicy := NewCachedPolicy(p2)
				cachedInPolicy.ToNodePubKey = toNodeCallback
				cachedInPolicy.ToNodeFeatures = toNodeFeatures
			}

			directedChannel := &DirectedChannel{
				ChannelID:    e.ChannelID,
				IsNode1:      node.PubKeyBytes == e.NodeKey1Bytes,
				OtherNode:    e.NodeKey2Bytes,
				Capacity:     e.Capacity,
				OutPolicySet: p1 != nil,
				InPolicy:     cachedInPolicy,
			}

			if node.PubKeyBytes == e.NodeKey2Bytes {
				directedChannel.OtherNode = e.NodeKey1Bytes
			}

			channels[e.ChannelID] = directedChannel

			return nil
		})
		if err != nil {
			return err
		}

		return cb(node.PubKeyBytes, channels)
	})
}

// DisabledChannelIDs returns the channel ids of disabled channels.
// A channel is disabled when two of the associated ChanelEdgePolicies
// have their disabled bit on.
func (c *ChannelGraph) DisabledChannelIDs() ([]uint64, error) {
	var disabledChanIDs []uint64
	var chanEdgeFound map[uint64]struct{}

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}

		disabledEdgePolicyIndex := edges.NestedReadBucket(
			disabledEdgePolicyBucket,
		)
		if disabledEdgePolicyIndex == nil {
			return nil
		}

		// We iterate over all disabled policies and we add each channel that
		// has more than one disabled policy to disabledChanIDs array.
		return disabledEdgePolicyIndex.ForEach(func(k, v []byte) error {
			chanID := byteOrder.Uint64(k[:8])
			_, edgeFound := chanEdgeFound[chanID]
			if edgeFound {
				delete(chanEdgeFound, chanID)
				disabledChanIDs = append(disabledChanIDs, chanID)
				return nil
			}

			chanEdgeFound[chanID] = struct{}{}
			return nil
		})
	}, func() {
		disabledChanIDs = nil
		chanEdgeFound = make(map[uint64]struct{})
	})
	if err != nil {
		return nil, err
	}

	return disabledChanIDs, nil
}

// ForEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early.
//
// TODO(roasbeef): add iterator interface to allow for memory efficient graph
// traversal when graph gets mega
func (c *ChannelGraph) ForEachNode(
	cb func(kvdb.RTx, *LightningNode) error) error {

	traversal := func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		return nodes.ForEach(func(pubKey, nodeBytes []byte) error {
			// If this is the source key, then we skip this
			// iteration as the value for this key is a pubKey
			// rather than raw node information.
			if bytes.Equal(pubKey, sourceKey) || len(pubKey) != 33 {
				return nil
			}

			nodeReader := bytes.NewReader(nodeBytes)
			node, err := deserializeLightningNode(nodeReader)
			if err != nil {
				return err
			}
			node.db = c.db

			// Execute the callback, the transaction will abort if
			// this returns an error.
			return cb(tx, &node)
		})
	}

	return kvdb.View(c.db, traversal, func() {})
}

// ForEachNodeCacheable iterates through all the stored vertices/nodes in the
// graph, executing the passed callback with each node encountered. If the
// callback returns an error, then the transaction is aborted and the iteration
// stops early.
func (c *ChannelGraph) ForEachNodeCacheable(cb func(kvdb.RTx,
	GraphCacheNode) error) error {

	traversal := func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		return nodes.ForEach(func(pubKey, nodeBytes []byte) error {
			// If this is the source key, then we skip this
			// iteration as the value for this key is a pubKey
			// rather than raw node information.
			if bytes.Equal(pubKey, sourceKey) || len(pubKey) != 33 {
				return nil
			}

			nodeReader := bytes.NewReader(nodeBytes)
			cacheableNode, err := deserializeLightningNodeCacheable(
				nodeReader,
			)
			if err != nil {
				return err
			}

			// Execute the callback, the transaction will abort if
			// this returns an error.
			return cb(tx, cacheableNode)
		})
	}

	return kvdb.View(c.db, traversal, func() {})
}

// SourceNode returns the source node of the graph. The source node is treated
// as the center node within a star-graph. This method may be used to kick off
// a path finding algorithm in order to explore the reachability of another
// node based off the source node.
func (c *ChannelGraph) SourceNode() (*LightningNode, error) {
	var source *LightningNode
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		node, err := c.sourceNode(nodes)
		if err != nil {
			return err
		}
		source = node

		return nil
	}, func() {
		source = nil
	})
	if err != nil {
		return nil, err
	}

	return source, nil
}

// sourceNode uses an existing database transaction and returns the source node
// of the graph. The source node is treated as the center node within a
// star-graph. This method may be used to kick off a path finding algorithm in
// order to explore the reachability of another node based off the source node.
func (c *ChannelGraph) sourceNode(nodes kvdb.RBucket) (*LightningNode, error) {
	selfPub := nodes.Get(sourceKey)
	if selfPub == nil {
		return nil, ErrSourceNodeNotSet
	}

	// With the pubKey of the source node retrieved, we're able to
	// fetch the full node information.
	node, err := fetchLightningNode(nodes, selfPub)
	if err != nil {
		return nil, err
	}
	node.db = c.db

	return &node, nil
}

// SetSourceNode sets the source node within the graph database. The source
// node is to be used as the center of a star-graph within path finding
// algorithms.
func (c *ChannelGraph) SetSourceNode(node *LightningNode) error {
	nodePubBytes := node.PubKeyBytes[:]

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes, err := tx.CreateTopLevelBucket(nodeBucket)
		if err != nil {
			return err
		}

		// Next we create the mapping from source to the targeted
		// public key.
		if err := nodes.Put(sourceKey, nodePubBytes); err != nil {
			return err
		}

		// Finally, we commit the information of the lightning node
		// itself.
		return addLightningNode(tx, node)
	}, func() {})
}

// AddLightningNode adds a vertex/node to the graph database. If the node is not
// in the database from before, this will add a new, unconnected one to the
// graph. If it is present from before, this will update that node's
// information. Note that this method is expected to only be called to update an
// already present node from a node announcement, or to insert a node found in a
// channel update.
//
// TODO(roasbeef): also need sig of announcement
func (c *ChannelGraph) AddLightningNode(node *LightningNode,
	op ...batch.SchedulerOption) error {

	r := &batch.Request{
		Update: func(tx kvdb.RwTx) error {
			if c.graphCache != nil {
				cNode := newGraphCacheNode(
					node.PubKeyBytes, node.Features,
				)
				err := c.graphCache.AddNode(tx, cNode)
				if err != nil {
					return err
				}
			}

			return addLightningNode(tx, node)
		},
	}

	for _, f := range op {
		f(r)
	}

	return c.nodeScheduler.Execute(r)
}

func addLightningNode(tx kvdb.RwTx, node *LightningNode) error {
	nodes, err := tx.CreateTopLevelBucket(nodeBucket)
	if err != nil {
		return err
	}

	aliases, err := nodes.CreateBucketIfNotExists(aliasIndexBucket)
	if err != nil {
		return err
	}

	updateIndex, err := nodes.CreateBucketIfNotExists(
		nodeUpdateIndexBucket,
	)
	if err != nil {
		return err
	}

	return putLightningNode(nodes, aliases, updateIndex, node)
}

// LookupAlias attempts to return the alias as advertised by the target node.
// TODO(roasbeef): currently assumes that aliases are unique...
func (c *ChannelGraph) LookupAlias(pub *btcec.PublicKey) (string, error) {
	var alias string

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}

		aliases := nodes.NestedReadBucket(aliasIndexBucket)
		if aliases == nil {
			return ErrGraphNodesNotFound
		}

		nodePub := pub.SerializeCompressed()
		a := aliases.Get(nodePub)
		if a == nil {
			return ErrNodeAliasNotFound
		}

		// TODO(roasbeef): should actually be using the utf-8
		// package...
		alias = string(a)
		return nil
	}, func() {
		alias = ""
	})
	if err != nil {
		return "", err
	}

	return alias, nil
}

// DeleteLightningNode starts a new database transaction to remove a vertex/node
// from the database according to the node's public key.
func (c *ChannelGraph) DeleteLightningNode(nodePub route.Vertex) error {
	// TODO(roasbeef): ensure dangling edges are removed...
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		nodes := tx.ReadWriteBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodeNotFound
		}

		if c.graphCache != nil {
			c.graphCache.RemoveNode(nodePub)
		}

		return c.deleteLightningNode(nodes, nodePub[:])
	}, func() {})
}

// deleteLightningNode uses an existing database transaction to remove a
// vertex/node from the database according to the node's public key.
func (c *ChannelGraph) deleteLightningNode(nodes kvdb.RwBucket,
	compressedPubKey []byte) error {

	aliases := nodes.NestedReadWriteBucket(aliasIndexBucket)
	if aliases == nil {
		return ErrGraphNodesNotFound
	}

	if err := aliases.Delete(compressedPubKey); err != nil {
		return err
	}

	// Before we delete the node, we'll fetch its current state so we can
	// determine when its last update was to clear out the node update
	// index.
	node, err := fetchLightningNode(nodes, compressedPubKey)
	if err != nil {
		return err
	}

	if err := nodes.Delete(compressedPubKey); err != nil {
		return err
	}

	// Finally, we'll delete the index entry for the node within the
	// nodeUpdateIndexBucket as this node is no longer active, so we don't
	// need to track its last update.
	nodeUpdateIndex := nodes.NestedReadWriteBucket(nodeUpdateIndexBucket)
	if nodeUpdateIndex == nil {
		return ErrGraphNodesNotFound
	}

	// In order to delete the entry, we'll need to reconstruct the key for
	// its last update.
	updateUnix := uint64(node.LastUpdate.Unix())
	var indexKey [8 + 33]byte
	byteOrder.PutUint64(indexKey[:8], updateUnix)
	copy(indexKey[8:], compressedPubKey)

	return nodeUpdateIndex.Delete(indexKey[:])
}

// AddChannelEdge adds a new (undirected, blank) edge to the graph database. An
// undirected edge from the two target nodes are created. The information stored
// denotes the static attributes of the channel, such as the channelID, the keys
// involved in creation of the channel, and the set of features that the channel
// supports. The chanPoint and chanID are used to uniquely identify the edge
// globally within the database.
func (c *ChannelGraph) AddChannelEdge(edge *ChannelEdgeInfo,
	op ...batch.SchedulerOption) error {

	var alreadyExists bool
	r := &batch.Request{
		Reset: func() {
			alreadyExists = false
		},
		Update: func(tx kvdb.RwTx) error {
			err := c.addChannelEdge(tx, edge)

			// Silence ErrEdgeAlreadyExist so that the batch can
			// succeed, but propagate the error via local state.
			if err == ErrEdgeAlreadyExist {
				alreadyExists = true
				return nil
			}

			return err
		},
		OnCommit: func(err error) error {
			switch {
			case err != nil:
				return err
			case alreadyExists:
				return ErrEdgeAlreadyExist
			default:
				c.rejectCache.remove(edge.ChannelID)
				c.chanCache.remove(edge.ChannelID)
				return nil
			}
		},
	}

	for _, f := range op {
		f(r)
	}

	return c.chanScheduler.Execute(r)
}

// addChannelEdge is the private form of AddChannelEdge that allows callers to
// utilize an existing db transaction.
func (c *ChannelGraph) addChannelEdge(tx kvdb.RwTx, edge *ChannelEdgeInfo) error {
	// Construct the channel's primary key which is the 8-byte channel ID.
	var chanKey [8]byte
	binary.BigEndian.PutUint64(chanKey[:], edge.ChannelID)

	nodes, err := tx.CreateTopLevelBucket(nodeBucket)
	if err != nil {
		return err
	}
	edges, err := tx.CreateTopLevelBucket(edgeBucket)
	if err != nil {
		return err
	}
	edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
	if err != nil {
		return err
	}
	chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
	if err != nil {
		return err
	}

	// First, attempt to check if this edge has already been created. If
	// so, then we can exit early as this method is meant to be idempotent.
	if edgeInfo := edgeIndex.Get(chanKey[:]); edgeInfo != nil {
		return ErrEdgeAlreadyExist
	}

	if c.graphCache != nil {
		c.graphCache.AddChannel(edge, nil, nil)
	}

	// Before we insert the channel into the database, we'll ensure that
	// both nodes already exist in the channel graph. If either node
	// doesn't, then we'll insert a "shell" node that just includes its
	// public key, so subsequent validation and queries can work properly.
	_, node1Err := fetchLightningNode(nodes, edge.NodeKey1Bytes[:])
	switch {
	case node1Err == ErrGraphNodeNotFound:
		node1Shell := LightningNode{
			PubKeyBytes:          edge.NodeKey1Bytes,
			HaveNodeAnnouncement: false,
		}
		err := addLightningNode(tx, &node1Shell)
		if err != nil {
			return fmt.Errorf("unable to create shell node "+
				"for: %x", edge.NodeKey1Bytes)
		}
	case node1Err != nil:
		return err
	}

	_, node2Err := fetchLightningNode(nodes, edge.NodeKey2Bytes[:])
	switch {
	case node2Err == ErrGraphNodeNotFound:
		node2Shell := LightningNode{
			PubKeyBytes:          edge.NodeKey2Bytes,
			HaveNodeAnnouncement: false,
		}
		err := addLightningNode(tx, &node2Shell)
		if err != nil {
			return fmt.Errorf("unable to create shell node "+
				"for: %x", edge.NodeKey2Bytes)
		}
	case node2Err != nil:
		return err
	}

	// If the edge hasn't been created yet, then we'll first add it to the
	// edge index in order to associate the edge between two nodes and also
	// store the static components of the channel.
	if err := putChanEdgeInfo(edgeIndex, edge, chanKey); err != nil {
		return err
	}

	// Mark edge policies for both sides as unknown. This is to enable
	// efficient incoming channel lookup for a node.
	keys := []*[33]byte{
		&edge.NodeKey1Bytes,
		&edge.NodeKey2Bytes,
	}
	for _, key := range keys {
		err := putChanEdgePolicyUnknown(edges, edge.ChannelID, key[:])
		if err != nil {
			return err
		}
	}

	// Finally we add it to the channel index which maps channel points
	// (outpoints) to the shorter channel ID's.
	var b bytes.Buffer
	if err := writeOutpoint(&b, &edge.ChannelPoint); err != nil {
		return err
	}
	return chanIndex.Put(b.Bytes(), chanKey[:])
}

// HasChannelEdge returns true if the database knows of a channel edge with the
// passed channel ID, and false otherwise. If an edge with that ID is found
// within the graph, then two time stamps representing the last time the edge
// was updated for both directed edges are returned along with the boolean. If
// it is not found, then the zombie index is checked and its result is returned
// as the second boolean.
func (c *ChannelGraph) HasChannelEdge(
	chanID uint64) (time.Time, time.Time, bool, bool, error) {

	var (
		upd1Time time.Time
		upd2Time time.Time
		exists   bool
		isZombie bool
	)

	// We'll query the cache with the shared lock held to allow multiple
	// readers to access values in the cache concurrently if they exist.
	c.cacheMu.RLock()
	if entry, ok := c.rejectCache.get(chanID); ok {
		c.cacheMu.RUnlock()
		upd1Time = time.Unix(entry.upd1Time, 0)
		upd2Time = time.Unix(entry.upd2Time, 0)
		exists, isZombie = entry.flags.unpack()
		return upd1Time, upd2Time, exists, isZombie, nil
	}
	c.cacheMu.RUnlock()

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	// The item was not found with the shared lock, so we'll acquire the
	// exclusive lock and check the cache again in case another method added
	// the entry to the cache while no lock was held.
	if entry, ok := c.rejectCache.get(chanID); ok {
		upd1Time = time.Unix(entry.upd1Time, 0)
		upd2Time = time.Unix(entry.upd2Time, 0)
		exists, isZombie = entry.flags.unpack()
		return upd1Time, upd2Time, exists, isZombie, nil
	}

	if err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		var channelID [8]byte
		byteOrder.PutUint64(channelID[:], chanID)

		// If the edge doesn't exist, then we'll also check our zombie
		// index.
		if edgeIndex.Get(channelID[:]) == nil {
			exists = false
			zombieIndex := edges.NestedReadBucket(zombieBucket)
			if zombieIndex != nil {
				isZombie, _, _ = isZombieEdge(
					zombieIndex, chanID,
				)
			}

			return nil
		}

		exists = true
		isZombie = false

		// If the channel has been found in the graph, then retrieve
		// the edges itself so we can return the last updated
		// timestamps.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodeNotFound
		}

		e1, e2, err := fetchChanEdgePolicies(edgeIndex, edges, nodes,
			channelID[:], c.db)
		if err != nil {
			return err
		}

		// As we may have only one of the edges populated, only set the
		// update time if the edge was found in the database.
		if e1 != nil {
			upd1Time = e1.LastUpdate
		}
		if e2 != nil {
			upd2Time = e2.LastUpdate
		}

		return nil
	}, func() {}); err != nil {
		return time.Time{}, time.Time{}, exists, isZombie, err
	}

	c.rejectCache.insert(chanID, rejectCacheEntry{
		upd1Time: upd1Time.Unix(),
		upd2Time: upd2Time.Unix(),
		flags:    packRejectFlags(exists, isZombie),
	})

	return upd1Time, upd2Time, exists, isZombie, nil
}

// UpdateChannelEdge retrieves and update edge of the graph database. Method
// only reserved for updating an edge info after its already been created.
// In order to maintain this constraints, we return an error in the scenario
// that an edge info hasn't yet been created yet, but someone attempts to update
// it.
func (c *ChannelGraph) UpdateChannelEdge(edge *ChannelEdgeInfo) error {
	// Construct the channel's primary key which is the 8-byte channel ID.
	var chanKey [8]byte
	binary.BigEndian.PutUint64(chanKey[:], edge.ChannelID)

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		if edge == nil {
			return ErrEdgeNotFound
		}

		edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrEdgeNotFound
		}

		if edgeInfo := edgeIndex.Get(chanKey[:]); edgeInfo == nil {
			return ErrEdgeNotFound
		}

		if c.graphCache != nil {
			c.graphCache.UpdateChannel(edge)
		}

		return putChanEdgeInfo(edgeIndex, edge, chanKey)
	}, func() {})
}

const (
	// pruneTipBytes is the total size of the value which stores a prune
	// entry of the graph in the prune log. The "prune tip" is the last
	// entry in the prune log, and indicates if the channel graph is in
	// sync with the current UTXO state. The structure of the value
	// is: blockHash, taking 32 bytes total.
	pruneTipBytes = 32
)

// PruneGraph prunes newly closed channels from the channel graph in response
// to a new block being solved on the network. Any transactions which spend the
// funding output of any known channels within he graph will be deleted.
// Additionally, the "prune tip", or the last block which has been used to
// prune the graph is stored so callers can ensure the graph is fully in sync
// with the current UTXO state. A slice of channels that have been closed by
// the target block are returned if the function succeeds without error.
func (c *ChannelGraph) PruneGraph(spentOutputs []*wire.OutPoint,
	blockHash *chainhash.Hash, blockHeight uint32) ([]*ChannelEdgeInfo, error) {

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	var chansClosed []*ChannelEdgeInfo

	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		// First grab the edges bucket which houses the information
		// we'd like to delete
		edges, err := tx.CreateTopLevelBucket(edgeBucket)
		if err != nil {
			return err
		}

		// Next grab the two edge indexes which will also need to be updated.
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}
		nodes := tx.ReadWriteBucket(nodeBucket)
		if nodes == nil {
			return ErrSourceNodeNotSet
		}
		zombieIndex, err := edges.CreateBucketIfNotExists(zombieBucket)
		if err != nil {
			return err
		}

		// For each of the outpoints that have been spent within the
		// block, we attempt to delete them from the graph as if that
		// outpoint was a channel, then it has now been closed.
		for _, chanPoint := range spentOutputs {
			// TODO(roasbeef): load channel bloom filter, continue
			// if NOT if filter

			var opBytes bytes.Buffer
			if err := writeOutpoint(&opBytes, chanPoint); err != nil {
				return err
			}

			// First attempt to see if the channel exists within
			// the database, if not, then we can exit early.
			chanID := chanIndex.Get(opBytes.Bytes())
			if chanID == nil {
				continue
			}

			// However, if it does, then we'll read out the full
			// version so we can add it to the set of deleted
			// channels.
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				return err
			}

			// Attempt to delete the channel, an ErrEdgeNotFound
			// will be returned if that outpoint isn't known to be
			// a channel. If no error is returned, then a channel
			// was successfully pruned.
			err = c.delChannelEdge(
				edges, edgeIndex, chanIndex, zombieIndex, nodes,
				chanID, false, false,
			)
			if err != nil && err != ErrEdgeNotFound {
				return err
			}

			chansClosed = append(chansClosed, &edgeInfo)
		}

		metaBucket, err := tx.CreateTopLevelBucket(graphMetaBucket)
		if err != nil {
			return err
		}

		pruneBucket, err := metaBucket.CreateBucketIfNotExists(pruneLogBucket)
		if err != nil {
			return err
		}

		// With the graph pruned, add a new entry to the prune log,
		// which can be used to check if the graph is fully synced with
		// the current UTXO state.
		var blockHeightBytes [4]byte
		byteOrder.PutUint32(blockHeightBytes[:], blockHeight)

		var newTip [pruneTipBytes]byte
		copy(newTip[:], blockHash[:])

		err = pruneBucket.Put(blockHeightBytes[:], newTip[:])
		if err != nil {
			return err
		}

		// Now that the graph has been pruned, we'll also attempt to
		// prune any nodes that have had a channel closed within the
		// latest block.
		return c.pruneGraphNodes(nodes, edgeIndex)
	}, func() {
		chansClosed = nil
	})
	if err != nil {
		return nil, err
	}

	for _, channel := range chansClosed {
		c.rejectCache.remove(channel.ChannelID)
		c.chanCache.remove(channel.ChannelID)
	}

	if c.graphCache != nil {
		log.Debugf("Pruned graph, cache now has %s",
			c.graphCache.Stats())
	}

	return chansClosed, nil
}

// PruneGraphNodes is a garbage collection method which attempts to prune out
// any nodes from the channel graph that are currently unconnected. This ensure
// that we only maintain a graph of reachable nodes. In the event that a pruned
// node gains more channels, it will be re-added back to the graph.
func (c *ChannelGraph) PruneGraphNodes() error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		nodes := tx.ReadWriteBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}
		edges := tx.ReadWriteBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNotFound
		}
		edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		return c.pruneGraphNodes(nodes, edgeIndex)
	}, func() {})
}

// pruneGraphNodes attempts to remove any nodes from the graph who have had a
// channel closed within the current block. If the node still has existing
// channels in the graph, this will act as a no-op.
func (c *ChannelGraph) pruneGraphNodes(nodes kvdb.RwBucket,
	edgeIndex kvdb.RwBucket) error {

	log.Trace("Pruning nodes from graph with no open channels")

	// We'll retrieve the graph's source node to ensure we don't remove it
	// even if it no longer has any open channels.
	sourceNode, err := c.sourceNode(nodes)
	if err != nil {
		return err
	}

	// We'll use this map to keep count the number of references to a node
	// in the graph. A node should only be removed once it has no more
	// references in the graph.
	nodeRefCounts := make(map[[33]byte]int)
	err = nodes.ForEach(func(pubKey, nodeBytes []byte) error {
		// If this is the source key, then we skip this
		// iteration as the value for this key is a pubKey
		// rather than raw node information.
		if bytes.Equal(pubKey, sourceKey) || len(pubKey) != 33 {
			return nil
		}

		var nodePub [33]byte
		copy(nodePub[:], pubKey)
		nodeRefCounts[nodePub] = 0

		return nil
	})
	if err != nil {
		return err
	}

	// To ensure we never delete the source node, we'll start off by
	// bumping its ref count to 1.
	nodeRefCounts[sourceNode.PubKeyBytes] = 1

	// Next, we'll run through the edgeIndex which maps a channel ID to the
	// edge info. We'll use this scan to populate our reference count map
	// above.
	err = edgeIndex.ForEach(func(chanID, edgeInfoBytes []byte) error {
		// The first 66 bytes of the edge info contain the pubkeys of
		// the nodes that this edge attaches. We'll extract them, and
		// add them to the ref count map.
		var node1, node2 [33]byte
		copy(node1[:], edgeInfoBytes[:33])
		copy(node2[:], edgeInfoBytes[33:])

		// With the nodes extracted, we'll increase the ref count of
		// each of the nodes.
		nodeRefCounts[node1]++
		nodeRefCounts[node2]++

		return nil
	})
	if err != nil {
		return err
	}

	// Finally, we'll make a second pass over the set of nodes, and delete
	// any nodes that have a ref count of zero.
	var numNodesPruned int
	for nodePubKey, refCount := range nodeRefCounts {
		// If the ref count of the node isn't zero, then we can safely
		// skip it as it still has edges to or from it within the
		// graph.
		if refCount != 0 {
			continue
		}

		if c.graphCache != nil {
			c.graphCache.RemoveNode(nodePubKey)
		}

		// If we reach this point, then there are no longer any edges
		// that connect this node, so we can delete it.
		if err := c.deleteLightningNode(nodes, nodePubKey[:]); err != nil {
			log.Warnf("Unable to prune node %x from the "+
				"graph: %v", nodePubKey, err)
			continue
		}

		log.Infof("Pruned unconnected node %x from channel graph",
			nodePubKey[:])

		numNodesPruned++
	}

	if numNodesPruned > 0 {
		log.Infof("Pruned %v unconnected nodes from the channel graph",
			numNodesPruned)
	}

	return nil
}

// DisconnectBlockAtHeight is used to indicate that the block specified
// by the passed height has been disconnected from the main chain. This
// will "rewind" the graph back to the height below, deleting channels
// that are no longer confirmed from the graph. The prune log will be
// set to the last prune height valid for the remaining chain.
// Channels that were removed from the graph resulting from the
// disconnected block are returned.
func (c *ChannelGraph) DisconnectBlockAtHeight(height uint32) ([]*ChannelEdgeInfo,
	error) {

	// Every channel having a ShortChannelID starting at 'height'
	// will no longer be confirmed.
	startShortChanID := lnwire.ShortChannelID{
		BlockHeight: height,
	}

	// Delete everything after this height from the db up until the
	// SCID alias range.
	endShortChanID := aliasmgr.StartingAlias

	// The block height will be the 3 first bytes of the channel IDs.
	var chanIDStart [8]byte
	byteOrder.PutUint64(chanIDStart[:], startShortChanID.ToUint64())
	var chanIDEnd [8]byte
	byteOrder.PutUint64(chanIDEnd[:], endShortChanID.ToUint64())

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	// Keep track of the channels that are removed from the graph.
	var removedChans []*ChannelEdgeInfo

	if err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		edges, err := tx.CreateTopLevelBucket(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}
		zombieIndex, err := edges.CreateBucketIfNotExists(zombieBucket)
		if err != nil {
			return err
		}
		nodes, err := tx.CreateTopLevelBucket(nodeBucket)
		if err != nil {
			return err
		}

		// Scan from chanIDStart to chanIDEnd, deleting every
		// found edge.
		// NOTE: we must delete the edges after the cursor loop, since
		// modifying the bucket while traversing is not safe.
		// NOTE: We use a < comparison in bytes.Compare instead of <=
		// so that the StartingAlias itself isn't deleted.
		var keys [][]byte
		cursor := edgeIndex.ReadWriteCursor()

		//nolint:lll
		for k, v := cursor.Seek(chanIDStart[:]); k != nil &&
			bytes.Compare(k, chanIDEnd[:]) < 0; k, v = cursor.Next() {
			edgeInfoReader := bytes.NewReader(v)
			edgeInfo, err := deserializeChanEdgeInfo(edgeInfoReader)
			if err != nil {
				return err
			}

			keys = append(keys, k)
			removedChans = append(removedChans, &edgeInfo)
		}

		for _, k := range keys {
			err = c.delChannelEdge(
				edges, edgeIndex, chanIndex, zombieIndex, nodes,
				k, false, false,
			)
			if err != nil && err != ErrEdgeNotFound {
				return err
			}
		}

		// Delete all the entries in the prune log having a height
		// greater or equal to the block disconnected.
		metaBucket, err := tx.CreateTopLevelBucket(graphMetaBucket)
		if err != nil {
			return err
		}

		pruneBucket, err := metaBucket.CreateBucketIfNotExists(pruneLogBucket)
		if err != nil {
			return err
		}

		var pruneKeyStart [4]byte
		byteOrder.PutUint32(pruneKeyStart[:], height)

		var pruneKeyEnd [4]byte
		byteOrder.PutUint32(pruneKeyEnd[:], math.MaxUint32)

		// To avoid modifying the bucket while traversing, we delete
		// the keys in a second loop.
		var pruneKeys [][]byte
		pruneCursor := pruneBucket.ReadWriteCursor()
		for k, _ := pruneCursor.Seek(pruneKeyStart[:]); k != nil &&
			bytes.Compare(k, pruneKeyEnd[:]) <= 0; k, _ = pruneCursor.Next() {

			pruneKeys = append(pruneKeys, k)
		}

		for _, k := range pruneKeys {
			if err := pruneBucket.Delete(k); err != nil {
				return err
			}
		}

		return nil
	}, func() {
		removedChans = nil
	}); err != nil {
		return nil, err
	}

	for _, channel := range removedChans {
		c.rejectCache.remove(channel.ChannelID)
		c.chanCache.remove(channel.ChannelID)
	}

	return removedChans, nil
}

// PruneTip returns the block height and hash of the latest block that has been
// used to prune channels in the graph. Knowing the "prune tip" allows callers
// to tell if the graph is currently in sync with the current best known UTXO
// state.
func (c *ChannelGraph) PruneTip() (*chainhash.Hash, uint32, error) {
	var (
		tipHash   chainhash.Hash
		tipHeight uint32
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		graphMeta := tx.ReadBucket(graphMetaBucket)
		if graphMeta == nil {
			return ErrGraphNotFound
		}
		pruneBucket := graphMeta.NestedReadBucket(pruneLogBucket)
		if pruneBucket == nil {
			return ErrGraphNeverPruned
		}

		pruneCursor := pruneBucket.ReadCursor()

		// The prune key with the largest block height will be our
		// prune tip.
		k, v := pruneCursor.Last()
		if k == nil {
			return ErrGraphNeverPruned
		}

		// Once we have the prune tip, the value will be the block hash,
		// and the key the block height.
		copy(tipHash[:], v[:])
		tipHeight = byteOrder.Uint32(k[:])

		return nil
	}, func() {})
	if err != nil {
		return nil, 0, err
	}

	return &tipHash, tipHeight, nil
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

	// TODO(roasbeef): possibly delete from node bucket if node has no more
	// channels
	// TODO(roasbeef): don't delete both edges?

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		if edges == nil {
			return ErrEdgeNotFound
		}
		edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrEdgeNotFound
		}
		chanIndex := edges.NestedReadWriteBucket(channelPointBucket)
		if chanIndex == nil {
			return ErrEdgeNotFound
		}
		nodes := tx.ReadWriteBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodeNotFound
		}
		zombieIndex, err := edges.CreateBucketIfNotExists(zombieBucket)
		if err != nil {
			return err
		}

		var rawChanID [8]byte
		for _, chanID := range chanIDs {
			byteOrder.PutUint64(rawChanID[:], chanID)
			err := c.delChannelEdge(
				edges, edgeIndex, chanIndex, zombieIndex, nodes,
				rawChanID[:], markZombie, strictZombiePruning,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		return err
	}

	for _, chanID := range chanIDs {
		c.rejectCache.remove(chanID)
		c.chanCache.remove(chanID)
	}

	return nil
}

// ChannelID attempt to lookup the 8-byte compact channel ID which maps to the
// passed channel point (outpoint). If the passed channel doesn't exist within
// the database, then ErrEdgeNotFound is returned.
func (c *ChannelGraph) ChannelID(chanPoint *wire.OutPoint) (uint64, error) {
	var chanID uint64
	if err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		var err error
		chanID, err = getChanID(tx, chanPoint)
		return err
	}, func() {
		chanID = 0
	}); err != nil {
		return 0, err
	}

	return chanID, nil
}

// getChanID returns the assigned channel ID for a given channel point.
func getChanID(tx kvdb.RTx, chanPoint *wire.OutPoint) (uint64, error) {
	var b bytes.Buffer
	if err := writeOutpoint(&b, chanPoint); err != nil {
		return 0, err
	}

	edges := tx.ReadBucket(edgeBucket)
	if edges == nil {
		return 0, ErrGraphNoEdgesFound
	}
	chanIndex := edges.NestedReadBucket(channelPointBucket)
	if chanIndex == nil {
		return 0, ErrGraphNoEdgesFound
	}

	chanIDBytes := chanIndex.Get(b.Bytes())
	if chanIDBytes == nil {
		return 0, ErrEdgeNotFound
	}

	chanID := byteOrder.Uint64(chanIDBytes)

	return chanID, nil
}

// TODO(roasbeef): allow updates to use Batch?

// HighestChanID returns the "highest" known channel ID in the channel graph.
// This represents the "newest" channel from the PoV of the chain. This method
// can be used by peers to quickly determine if they're graphs are in sync.
func (c *ChannelGraph) HighestChanID() (uint64, error) {
	var cid uint64

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// In order to find the highest chan ID, we'll fetch a cursor
		// and use that to seek to the "end" of our known rage.
		cidCursor := edgeIndex.ReadCursor()

		lastChanID, _ := cidCursor.Last()

		// If there's no key, then this means that we don't actually
		// know of any channels, so we'll return a predicable error.
		if lastChanID == nil {
			return ErrGraphNoEdgesFound
		}

		// Otherwise, we'll de serialize the channel ID and return it
		// to the caller.
		cid = byteOrder.Uint64(lastChanID)
		return nil
	}, func() {
		cid = 0
	})
	if err != nil && err != ErrGraphNoEdgesFound {
		return 0, err
	}

	return cid, nil
}

// ChannelEdge represents the complete set of information for a channel edge in
// the known channel graph. This struct couples the core information of the
// edge as well as each of the known advertised edge policies.
type ChannelEdge struct {
	// Info contains all the static information describing the channel.
	Info *ChannelEdgeInfo

	// Policy1 points to the "first" edge policy of the channel containing
	// the dynamic information required to properly route through the edge.
	Policy1 *ChannelEdgePolicy

	// Policy2 points to the "second" edge policy of the channel containing
	// the dynamic information required to properly route through the edge.
	Policy2 *ChannelEdgePolicy
}

// ChanUpdatesInHorizon returns all the known channel edges which have at least
// one edge that has an update timestamp within the specified horizon.
func (c *ChannelGraph) ChanUpdatesInHorizon(startTime,
	endTime time.Time) ([]ChannelEdge, error) {

	// To ensure we don't return duplicate ChannelEdges, we'll use an
	// additional map to keep track of the edges already seen to prevent
	// re-adding it.
	var edgesSeen map[uint64]struct{}
	var edgesToCache map[uint64]ChannelEdge
	var edgesInHorizon []ChannelEdge

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	var hits int
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}
		edgeUpdateIndex := edges.NestedReadBucket(edgeUpdateIndexBucket)
		if edgeUpdateIndex == nil {
			return ErrGraphNoEdgesFound
		}

		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}

		// We'll now obtain a cursor to perform a range query within
		// the index to find all channels within the horizon.
		updateCursor := edgeUpdateIndex.ReadCursor()

		var startTimeBytes, endTimeBytes [8 + 8]byte
		byteOrder.PutUint64(
			startTimeBytes[:8], uint64(startTime.Unix()),
		)
		byteOrder.PutUint64(
			endTimeBytes[:8], uint64(endTime.Unix()),
		)

		// With our start and end times constructed, we'll step through
		// the index collecting the info and policy of each update of
		// each channel that has a last update within the time range.
		for indexKey, _ := updateCursor.Seek(startTimeBytes[:]); indexKey != nil &&
			bytes.Compare(indexKey, endTimeBytes[:]) <= 0; indexKey, _ = updateCursor.Next() {

			// We have a new eligible entry, so we'll slice of the
			// chan ID so we can query it in the DB.
			chanID := indexKey[8:]

			// If we've already retrieved the info and policies for
			// this edge, then we can skip it as we don't need to do
			// so again.
			chanIDInt := byteOrder.Uint64(chanID)
			if _, ok := edgesSeen[chanIDInt]; ok {
				continue
			}

			if channel, ok := c.chanCache.get(chanIDInt); ok {
				hits++
				edgesSeen[chanIDInt] = struct{}{}
				edgesInHorizon = append(edgesInHorizon, channel)
				continue
			}

			// First, we'll fetch the static edge information.
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				chanID := byteOrder.Uint64(chanID)
				return fmt.Errorf("unable to fetch info for "+
					"edge with chan_id=%v: %v", chanID, err)
			}
			edgeInfo.db = c.db

			// With the static information obtained, we'll now
			// fetch the dynamic policy info.
			edge1, edge2, err := fetchChanEdgePolicies(
				edgeIndex, edges, nodes, chanID, c.db,
			)
			if err != nil {
				chanID := byteOrder.Uint64(chanID)
				return fmt.Errorf("unable to fetch policies "+
					"for edge with chan_id=%v: %v", chanID,
					err)
			}

			// Finally, we'll collate this edge with the rest of
			// edges to be returned.
			edgesSeen[chanIDInt] = struct{}{}
			channel := ChannelEdge{
				Info:    &edgeInfo,
				Policy1: edge1,
				Policy2: edge2,
			}
			edgesInHorizon = append(edgesInHorizon, channel)
			edgesToCache[chanIDInt] = channel
		}

		return nil
	}, func() {
		edgesSeen = make(map[uint64]struct{})
		edgesToCache = make(map[uint64]ChannelEdge)
		edgesInHorizon = nil
	})
	switch {
	case err == ErrGraphNoEdgesFound:
		fallthrough
	case err == ErrGraphNodesNotFound:
		break

	case err != nil:
		return nil, err
	}

	// Insert any edges loaded from disk into the cache.
	for chanid, channel := range edgesToCache {
		c.chanCache.insert(chanid, channel)
	}

	log.Debugf("ChanUpdatesInHorizon hit percentage: %f (%d/%d)",
		float64(hits)/float64(len(edgesInHorizon)), hits,
		len(edgesInHorizon))

	return edgesInHorizon, nil
}

// NodeUpdatesInHorizon returns all the known lightning node which have an
// update timestamp within the passed range. This method can be used by two
// nodes to quickly determine if they have the same set of up to date node
// announcements.
func (c *ChannelGraph) NodeUpdatesInHorizon(startTime,
	endTime time.Time) ([]LightningNode, error) {

	var nodesInHorizon []LightningNode

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}

		nodeUpdateIndex := nodes.NestedReadBucket(nodeUpdateIndexBucket)
		if nodeUpdateIndex == nil {
			return ErrGraphNodesNotFound
		}

		// We'll now obtain a cursor to perform a range query within
		// the index to find all node announcements within the horizon.
		updateCursor := nodeUpdateIndex.ReadCursor()

		var startTimeBytes, endTimeBytes [8 + 33]byte
		byteOrder.PutUint64(
			startTimeBytes[:8], uint64(startTime.Unix()),
		)
		byteOrder.PutUint64(
			endTimeBytes[:8], uint64(endTime.Unix()),
		)

		// With our start and end times constructed, we'll step through
		// the index collecting info for each node within the time
		// range.
		for indexKey, _ := updateCursor.Seek(startTimeBytes[:]); indexKey != nil &&
			bytes.Compare(indexKey, endTimeBytes[:]) <= 0; indexKey, _ = updateCursor.Next() {

			nodePub := indexKey[8:]
			node, err := fetchLightningNode(nodes, nodePub)
			if err != nil {
				return err
			}
			node.db = c.db

			nodesInHorizon = append(nodesInHorizon, node)
		}

		return nil
	}, func() {
		nodesInHorizon = nil
	})
	switch {
	case err == ErrGraphNoEdgesFound:
		fallthrough
	case err == ErrGraphNodesNotFound:
		break

	case err != nil:
		return nil, err
	}

	return nodesInHorizon, nil
}

// FilterKnownChanIDs takes a set of channel IDs and return the subset of chan
// ID's that we don't know and are not known zombies of the passed set. In other
// words, we perform a set difference of our set of chan ID's and the ones
// passed in. This method can be used by callers to determine the set of
// channels another peer knows of that we don't.
func (c *ChannelGraph) FilterKnownChanIDs(chanIDs []uint64) ([]uint64, error) {
	var newChanIDs []uint64

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// Fetch the zombie index, it may not exist if no edges have
		// ever been marked as zombies. If the index has been
		// initialized, we will use it later to skip known zombie edges.
		zombieIndex := edges.NestedReadBucket(zombieBucket)

		// We'll run through the set of chanIDs and collate only the
		// set of channel that are unable to be found within our db.
		var cidBytes [8]byte
		for _, cid := range chanIDs {
			byteOrder.PutUint64(cidBytes[:], cid)

			// If the edge is already known, skip it.
			if v := edgeIndex.Get(cidBytes[:]); v != nil {
				continue
			}

			// If the edge is a known zombie, skip it.
			if zombieIndex != nil {
				isZombie, _, _ := isZombieEdge(zombieIndex, cid)
				if isZombie {
					continue
				}
			}

			newChanIDs = append(newChanIDs, cid)
		}

		return nil
	}, func() {
		newChanIDs = nil
	})
	switch {
	// If we don't know of any edges yet, then we'll return the entire set
	// of chan IDs specified.
	case err == ErrGraphNoEdgesFound:
		return chanIDs, nil

	case err != nil:
		return nil, err
	}

	return newChanIDs, nil
}

// BlockChannelRange represents a range of channels for a given block height.
type BlockChannelRange struct {
	// Height is the height of the block all of the channels below were
	// included in.
	Height uint32

	// Channels is the list of channels identified by their short ID
	// representation known to us that were included in the block height
	// above.
	Channels []lnwire.ShortChannelID
}

// FilterChannelRange returns the channel ID's of all known channels which were
// mined in a block height within the passed range. The channel IDs are grouped
// by their common block height. This method can be used to quickly share with a
// peer the set of channels we know of within a particular range to catch them
// up after a period of time offline.
func (c *ChannelGraph) FilterChannelRange(startHeight,
	endHeight uint32) ([]BlockChannelRange, error) {

	startChanID := &lnwire.ShortChannelID{
		BlockHeight: startHeight,
	}

	endChanID := lnwire.ShortChannelID{
		BlockHeight: endHeight,
		TxIndex:     math.MaxUint32 & 0x00ffffff,
		TxPosition:  math.MaxUint16,
	}

	// As we need to perform a range scan, we'll convert the starting and
	// ending height to their corresponding values when encoded using short
	// channel ID's.
	var chanIDStart, chanIDEnd [8]byte
	byteOrder.PutUint64(chanIDStart[:], startChanID.ToUint64())
	byteOrder.PutUint64(chanIDEnd[:], endChanID.ToUint64())

	var channelsPerBlock map[uint32][]lnwire.ShortChannelID
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		cursor := edgeIndex.ReadCursor()

		// We'll now iterate through the database, and find each
		// channel ID that resides within the specified range.
		for k, v := cursor.Seek(chanIDStart[:]); k != nil &&
			bytes.Compare(k, chanIDEnd[:]) <= 0; k, v = cursor.Next() {
			// Don't send alias SCIDs during gossip sync.
			edgeReader := bytes.NewReader(v)
			edgeInfo, err := deserializeChanEdgeInfo(edgeReader)
			if err != nil {
				return err
			}

			if edgeInfo.AuthProof == nil {
				continue
			}

			// This channel ID rests within the target range, so
			// we'll add it to our returned set.
			rawCid := byteOrder.Uint64(k)
			cid := lnwire.NewShortChanIDFromInt(rawCid)
			channelsPerBlock[cid.BlockHeight] = append(
				channelsPerBlock[cid.BlockHeight], cid,
			)
		}

		return nil
	}, func() {
		channelsPerBlock = make(map[uint32][]lnwire.ShortChannelID)
	})

	switch {
	// If we don't know of any channels yet, then there's nothing to
	// filter, so we'll return an empty slice.
	case err == ErrGraphNoEdgesFound || len(channelsPerBlock) == 0:
		return nil, nil

	case err != nil:
		return nil, err
	}

	// Return the channel ranges in ascending block height order.
	blocks := make([]uint32, 0, len(channelsPerBlock))
	for block := range channelsPerBlock {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i] < blocks[j]
	})

	channelRanges := make([]BlockChannelRange, 0, len(channelsPerBlock))
	for _, block := range blocks {
		channelRanges = append(channelRanges, BlockChannelRange{
			Height:   block,
			Channels: channelsPerBlock[block],
		})
	}

	return channelRanges, nil
}

// FetchChanInfos returns the set of channel edges that correspond to the passed
// channel ID's. If an edge is the query is unknown to the database, it will
// skipped and the result will contain only those edges that exist at the time
// of the query. This can be used to respond to peer queries that are seeking to
// fill in gaps in their view of the channel graph.
func (c *ChannelGraph) FetchChanInfos(chanIDs []uint64) ([]ChannelEdge, error) {
	// TODO(roasbeef): sort cids?

	var (
		chanEdges []ChannelEdge
		cidBytes  [8]byte
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		for _, cid := range chanIDs {
			byteOrder.PutUint64(cidBytes[:], cid)

			// First, we'll fetch the static edge information. If
			// the edge is unknown, we will skip the edge and
			// continue gathering all known edges.
			edgeInfo, err := fetchChanEdgeInfo(
				edgeIndex, cidBytes[:],
			)
			switch {
			case err == ErrEdgeNotFound:
				continue
			case err != nil:
				return err
			}
			edgeInfo.db = c.db

			// With the static information obtained, we'll now
			// fetch the dynamic policy info.
			edge1, edge2, err := fetchChanEdgePolicies(
				edgeIndex, edges, nodes, cidBytes[:], c.db,
			)
			if err != nil {
				return err
			}

			chanEdges = append(chanEdges, ChannelEdge{
				Info:    &edgeInfo,
				Policy1: edge1,
				Policy2: edge2,
			})
		}
		return nil
	}, func() {
		chanEdges = nil
	})
	if err != nil {
		return nil, err
	}

	return chanEdges, nil
}

func delEdgeUpdateIndexEntry(edgesBucket kvdb.RwBucket, chanID uint64,
	edge1, edge2 *ChannelEdgePolicy) error {

	// First, we'll fetch the edge update index bucket which currently
	// stores an entry for the channel we're about to delete.
	updateIndex := edgesBucket.NestedReadWriteBucket(edgeUpdateIndexBucket)
	if updateIndex == nil {
		// No edges in bucket, return early.
		return nil
	}

	// Now that we have the bucket, we'll attempt to construct a template
	// for the index key: updateTime || chanid.
	var indexKey [8 + 8]byte
	byteOrder.PutUint64(indexKey[8:], chanID)

	// With the template constructed, we'll attempt to delete an entry that
	// would have been created by both edges: we'll alternate the update
	// times, as one may had overridden the other.
	if edge1 != nil {
		byteOrder.PutUint64(indexKey[:8], uint64(edge1.LastUpdate.Unix()))
		if err := updateIndex.Delete(indexKey[:]); err != nil {
			return err
		}
	}

	// We'll also attempt to delete the entry that may have been created by
	// the second edge.
	if edge2 != nil {
		byteOrder.PutUint64(indexKey[:8], uint64(edge2.LastUpdate.Unix()))
		if err := updateIndex.Delete(indexKey[:]); err != nil {
			return err
		}
	}

	return nil
}

func (c *ChannelGraph) delChannelEdge(edges, edgeIndex, chanIndex, zombieIndex,
	nodes kvdb.RwBucket, chanID []byte, isZombie, strictZombie bool) error {

	edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
	if err != nil {
		return err
	}

	if c.graphCache != nil {
		c.graphCache.RemoveChannel(
			edgeInfo.NodeKey1Bytes, edgeInfo.NodeKey2Bytes,
			edgeInfo.ChannelID,
		)
	}

	// We'll also remove the entry in the edge update index bucket before
	// we delete the edges themselves so we can access their last update
	// times.
	cid := byteOrder.Uint64(chanID)
	edge1, edge2, err := fetchChanEdgePolicies(
		edgeIndex, edges, nodes, chanID, nil,
	)
	if err != nil {
		return err
	}
	err = delEdgeUpdateIndexEntry(edges, cid, edge1, edge2)
	if err != nil {
		return err
	}

	// The edge key is of the format pubKey || chanID. First we construct
	// the latter half, populating the channel ID.
	var edgeKey [33 + 8]byte
	copy(edgeKey[33:], chanID)

	// With the latter half constructed, copy over the first public key to
	// delete the edge in this direction, then the second to delete the
	// edge in the opposite direction.
	copy(edgeKey[:33], edgeInfo.NodeKey1Bytes[:])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return err
		}
	}
	copy(edgeKey[:33], edgeInfo.NodeKey2Bytes[:])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return err
		}
	}

	// As part of deleting the edge we also remove all disabled entries
	// from the edgePolicyDisabledIndex bucket. We do that for both directions.
	updateEdgePolicyDisabledIndex(edges, cid, false, false)
	updateEdgePolicyDisabledIndex(edges, cid, true, false)

	// With the edge data deleted, we can purge the information from the two
	// edge indexes.
	if err := edgeIndex.Delete(chanID); err != nil {
		return err
	}
	var b bytes.Buffer
	if err := writeOutpoint(&b, &edgeInfo.ChannelPoint); err != nil {
		return err
	}
	if err := chanIndex.Delete(b.Bytes()); err != nil {
		return err
	}

	// Finally, we'll mark the edge as a zombie within our index if it's
	// being removed due to the channel becoming a zombie. We do this to
	// ensure we don't store unnecessary data for spent channels.
	if !isZombie {
		return nil
	}

	nodeKey1, nodeKey2 := edgeInfo.NodeKey1Bytes, edgeInfo.NodeKey2Bytes
	if strictZombie {
		nodeKey1, nodeKey2 = makeZombiePubkeys(&edgeInfo, edge1, edge2)
	}

	return markEdgeZombie(
		zombieIndex, byteOrder.Uint64(chanID), nodeKey1, nodeKey2,
	)
}

// makeZombiePubkeys derives the node pubkeys to store in the zombie index for a
// particular pair of channel policies. The return values are one of:
//  1. (pubkey1, pubkey2)
//  2. (pubkey1, blank)
//  3. (blank, pubkey2)
//
// A blank pubkey means that corresponding node will be unable to resurrect a
// channel on its own. For example, node1 may continue to publish recent
// updates, but node2 has fallen way behind. After marking an edge as a zombie,
// we don't want another fresh update from node1 to resurrect, as the edge can
// only become live once node2 finally sends something recent.
//
// In the case where we have neither update, we allow either party to resurrect
// the channel. If the channel were to be marked zombie again, it would be
// marked with the correct lagging channel since we received an update from only
// one side.
func makeZombiePubkeys(info *ChannelEdgeInfo,
	e1, e2 *ChannelEdgePolicy) ([33]byte, [33]byte) {

	switch {
	// If we don't have either edge policy, we'll return both pubkeys so
	// that the channel can be resurrected by either party.
	case e1 == nil && e2 == nil:
		return info.NodeKey1Bytes, info.NodeKey2Bytes

	// If we're missing edge1, or if both edges are present but edge1 is
	// older, we'll return edge1's pubkey and a blank pubkey for edge2. This
	// means that only an update from edge1 will be able to resurrect the
	// channel.
	case e1 == nil || (e2 != nil && e1.LastUpdate.Before(e2.LastUpdate)):
		return info.NodeKey1Bytes, [33]byte{}

	// Otherwise, we're missing edge2 or edge2 is the older side, so we
	// return a blank pubkey for edge1. In this case, only an update from
	// edge2 can resurect the channel.
	default:
		return [33]byte{}, info.NodeKey2Bytes
	}
}

// UpdateEdgePolicy updates the edge routing policy for a single directed edge
// within the database for the referenced channel. The `flags` attribute within
// the ChannelEdgePolicy determines which of the directed edges are being
// updated. If the flag is 1, then the first node's information is being
// updated, otherwise it's the second node's information. The node ordering is
// determined by the lexicographical ordering of the identity public keys of the
// nodes on either side of the channel.
func (c *ChannelGraph) UpdateEdgePolicy(edge *ChannelEdgePolicy,
	op ...batch.SchedulerOption) error {

	var (
		isUpdate1    bool
		edgeNotFound bool
	)

	r := &batch.Request{
		Reset: func() {
			isUpdate1 = false
			edgeNotFound = false
		},
		Update: func(tx kvdb.RwTx) error {
			var err error
			isUpdate1, err = updateEdgePolicy(
				tx, edge, c.graphCache,
			)

			// Silence ErrEdgeNotFound so that the batch can
			// succeed, but propagate the error via local state.
			if err == ErrEdgeNotFound {
				edgeNotFound = true
				return nil
			}

			return err
		},
		OnCommit: func(err error) error {
			switch {
			case err != nil:
				return err
			case edgeNotFound:
				return ErrEdgeNotFound
			default:
				c.updateEdgeCache(edge, isUpdate1)
				return nil
			}
		},
	}

	for _, f := range op {
		f(r)
	}

	return c.chanScheduler.Execute(r)
}

func (c *ChannelGraph) updateEdgeCache(e *ChannelEdgePolicy, isUpdate1 bool) {
	// If an entry for this channel is found in reject cache, we'll modify
	// the entry with the updated timestamp for the direction that was just
	// written. If the edge doesn't exist, we'll load the cache entry lazily
	// during the next query for this edge.
	if entry, ok := c.rejectCache.get(e.ChannelID); ok {
		if isUpdate1 {
			entry.upd1Time = e.LastUpdate.Unix()
		} else {
			entry.upd2Time = e.LastUpdate.Unix()
		}
		c.rejectCache.insert(e.ChannelID, entry)
	}

	// If an entry for this channel is found in channel cache, we'll modify
	// the entry with the updated policy for the direction that was just
	// written. If the edge doesn't exist, we'll defer loading the info and
	// policies and lazily read from disk during the next query.
	if channel, ok := c.chanCache.get(e.ChannelID); ok {
		if isUpdate1 {
			channel.Policy1 = e
		} else {
			channel.Policy2 = e
		}
		c.chanCache.insert(e.ChannelID, channel)
	}
}

// updateEdgePolicy attempts to update an edge's policy within the relevant
// buckets using an existing database transaction. The returned boolean will be
// true if the updated policy belongs to node1, and false if the policy belonged
// to node2.
func updateEdgePolicy(tx kvdb.RwTx, edge *ChannelEdgePolicy,
	graphCache *GraphCache) (bool, error) {

	edges := tx.ReadWriteBucket(edgeBucket)
	if edges == nil {
		return false, ErrEdgeNotFound
	}
	edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
	if edgeIndex == nil {
		return false, ErrEdgeNotFound
	}
	nodes, err := tx.CreateTopLevelBucket(nodeBucket)
	if err != nil {
		return false, err
	}

	// Create the channelID key be converting the channel ID
	// integer into a byte slice.
	var chanID [8]byte
	byteOrder.PutUint64(chanID[:], edge.ChannelID)

	// With the channel ID, we then fetch the value storing the two
	// nodes which connect this channel edge.
	nodeInfo := edgeIndex.Get(chanID[:])
	if nodeInfo == nil {
		return false, ErrEdgeNotFound
	}

	// Depending on the flags value passed above, either the first
	// or second edge policy is being updated.
	var fromNode, toNode []byte
	var isUpdate1 bool
	if edge.ChannelFlags&lnwire.ChanUpdateDirection == 0 {
		fromNode = nodeInfo[:33]
		toNode = nodeInfo[33:66]
		isUpdate1 = true
	} else {
		fromNode = nodeInfo[33:66]
		toNode = nodeInfo[:33]
		isUpdate1 = false
	}

	// Finally, with the direction of the edge being updated
	// identified, we update the on-disk edge representation.
	err = putChanEdgePolicy(edges, nodes, edge, fromNode, toNode)
	if err != nil {
		return false, err
	}

	var (
		fromNodePubKey route.Vertex
		toNodePubKey   route.Vertex
	)
	copy(fromNodePubKey[:], fromNode)
	copy(toNodePubKey[:], toNode)

	if graphCache != nil {
		graphCache.UpdatePolicy(
			edge, fromNodePubKey, toNodePubKey, isUpdate1,
		)
	}

	return isUpdate1, nil
}

// LightningNode represents an individual vertex/node within the channel graph.
// A node is connected to other nodes by one or more channel edges emanating
// from it. As the graph is directed, a node will also have an incoming edge
// attached to it for each outgoing edge.
type LightningNode struct {
	// PubKeyBytes is the raw bytes of the public key of the target node.
	PubKeyBytes [33]byte
	pubKey      *btcec.PublicKey

	// HaveNodeAnnouncement indicates whether we received a node
	// announcement for this particular node. If true, the remaining fields
	// will be set, if false only the PubKey is known for this node.
	HaveNodeAnnouncement bool

	// LastUpdate is the last time the vertex information for this node has
	// been updated.
	LastUpdate time.Time

	// Address is the TCP address this node is reachable over.
	Addresses []net.Addr

	// Color is the selected color for the node.
	Color color.RGBA

	// Alias is a nick-name for the node. The alias can be used to confirm
	// a node's identity or to serve as a short ID for an address book.
	Alias string

	// AuthSigBytes is the raw signature under the advertised public key
	// which serves to authenticate the attributes announced by this node.
	AuthSigBytes []byte

	// Features is the list of protocol features supported by this node.
	Features *lnwire.FeatureVector

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte

	db kvdb.Backend

	// TODO(roasbeef): discovery will need storage to keep it's last IP
	// address and re-announce if interface changes?

	// TODO(roasbeef): add update method and fetch?
}

// PubKey is the node's long-term identity public key. This key will be used to
// authenticated any advertisements/updates sent by the node.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (l *LightningNode) PubKey() (*btcec.PublicKey, error) {
	if l.pubKey != nil {
		return l.pubKey, nil
	}

	key, err := btcec.ParsePubKey(l.PubKeyBytes[:])
	if err != nil {
		return nil, err
	}
	l.pubKey = key

	return key, nil
}

// AuthSig is a signature under the advertised public key which serves to
// authenticate the attributes announced by this node.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (l *LightningNode) AuthSig() (*ecdsa.Signature, error) {
	return ecdsa.ParseSignature(l.AuthSigBytes)
}

// AddPubKey is a setter-link method that can be used to swap out the public
// key for a node.
func (l *LightningNode) AddPubKey(key *btcec.PublicKey) {
	l.pubKey = key
	copy(l.PubKeyBytes[:], key.SerializeCompressed())
}

// NodeAnnouncement retrieves the latest node announcement of the node.
func (l *LightningNode) NodeAnnouncement(signed bool) (*lnwire.NodeAnnouncement,
	error) {

	if !l.HaveNodeAnnouncement {
		return nil, fmt.Errorf("node does not have node announcement")
	}

	alias, err := lnwire.NewNodeAlias(l.Alias)
	if err != nil {
		return nil, err
	}

	nodeAnn := &lnwire.NodeAnnouncement{
		Features:        l.Features.RawFeatureVector,
		NodeID:          l.PubKeyBytes,
		RGBColor:        l.Color,
		Alias:           alias,
		Addresses:       l.Addresses,
		Timestamp:       uint32(l.LastUpdate.Unix()),
		ExtraOpaqueData: l.ExtraOpaqueData,
	}

	if !signed {
		return nodeAnn, nil
	}

	sig, err := lnwire.NewSigFromRawSignature(l.AuthSigBytes)
	if err != nil {
		return nil, err
	}

	nodeAnn.Signature = sig

	return nodeAnn, nil
}

// isPublic determines whether the node is seen as public within the graph from
// the source node's point of view. An existing database transaction can also be
// specified.
func (l *LightningNode) isPublic(tx kvdb.RTx, sourcePubKey []byte) (bool, error) {
	// In order to determine whether this node is publicly advertised within
	// the graph, we'll need to look at all of its edges and check whether
	// they extend to any other node than the source node. errDone will be
	// used to terminate the check early.
	nodeIsPublic := false
	errDone := errors.New("done")
	err := l.ForEachChannel(tx, func(_ kvdb.RTx, info *ChannelEdgeInfo,
		_, _ *ChannelEdgePolicy) error {

		// If this edge doesn't extend to the source node, we'll
		// terminate our search as we can now conclude that the node is
		// publicly advertised within the graph due to the local node
		// knowing of the current edge.
		if !bytes.Equal(info.NodeKey1Bytes[:], sourcePubKey) &&
			!bytes.Equal(info.NodeKey2Bytes[:], sourcePubKey) {

			nodeIsPublic = true
			return errDone
		}

		// Since the edge _does_ extend to the source node, we'll also
		// need to ensure that this is a public edge.
		if info.AuthProof != nil {
			nodeIsPublic = true
			return errDone
		}

		// Otherwise, we'll continue our search.
		return nil
	})
	if err != nil && err != errDone {
		return false, err
	}

	return nodeIsPublic, nil
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then ErrGraphNodeNotFound is
// returned.
func (c *ChannelGraph) FetchLightningNode(nodePub route.Vertex) (
	*LightningNode, error) {

	var node *LightningNode
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// If a key for this serialized public key isn't found, then
		// the target node doesn't exist within the database.
		nodeBytes := nodes.Get(nodePub[:])
		if nodeBytes == nil {
			return ErrGraphNodeNotFound
		}

		// If the node is found, then we can de deserialize the node
		// information to return to the user.
		nodeReader := bytes.NewReader(nodeBytes)
		n, err := deserializeLightningNode(nodeReader)
		if err != nil {
			return err
		}
		n.db = c.db

		node = &n

		return nil
	}, func() {
		node = nil
	})
	if err != nil {
		return nil, err
	}

	return node, nil
}

// graphCacheNode is a struct that wraps a LightningNode in a way that it can be
// cached in the graph cache.
type graphCacheNode struct {
	pubKeyBytes route.Vertex
	features    *lnwire.FeatureVector
}

// newGraphCacheNode returns a new cache optimized node.
func newGraphCacheNode(pubKey route.Vertex,
	features *lnwire.FeatureVector) *graphCacheNode {

	return &graphCacheNode{
		pubKeyBytes: pubKey,
		features:    features,
	}
}

// PubKey returns the node's public identity key.
func (n *graphCacheNode) PubKey() route.Vertex {
	return n.pubKeyBytes
}

// Features returns the node's features.
func (n *graphCacheNode) Features() *lnwire.FeatureVector {
	return n.features
}

// ForEachChannel iterates through all channels of this node, executing the
// passed callback with an edge info structure and the policies of each end
// of the channel. The first edge policy is the outgoing edge *to* the
// connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
func (n *graphCacheNode) ForEachChannel(tx kvdb.RTx,
	cb func(kvdb.RTx, *ChannelEdgeInfo, *ChannelEdgePolicy,
		*ChannelEdgePolicy) error) error {

	return nodeTraversal(tx, n.pubKeyBytes[:], nil, cb)
}

var _ GraphCacheNode = (*graphCacheNode)(nil)

// HasLightningNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean.
func (c *ChannelGraph) HasLightningNode(nodePub [33]byte) (time.Time, bool, error) {
	var (
		updateTime time.Time
		exists     bool
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// If a key for this serialized public key isn't found, we can
		// exit early.
		nodeBytes := nodes.Get(nodePub[:])
		if nodeBytes == nil {
			exists = false
			return nil
		}

		// Otherwise we continue on to obtain the time stamp
		// representing the last time the data for this node was
		// updated.
		nodeReader := bytes.NewReader(nodeBytes)
		node, err := deserializeLightningNode(nodeReader)
		if err != nil {
			return err
		}

		exists = true
		updateTime = node.LastUpdate
		return nil
	}, func() {
		updateTime = time.Time{}
		exists = false
	})
	if err != nil {
		return time.Time{}, exists, err
	}

	return updateTime, exists, nil
}

// nodeTraversal is used to traverse all channels of a node given by its
// public key and passes channel information into the specified callback.
func nodeTraversal(tx kvdb.RTx, nodePub []byte, db kvdb.Backend,
	cb func(kvdb.RTx, *ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy) error) error {

	traversal := func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNotFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// In order to reach all the edges for this node, we take
		// advantage of the construction of the key-space within the
		// edge bucket. The keys are stored in the form: pubKey ||
		// chanID. Therefore, starting from a chanID of zero, we can
		// scan forward in the bucket, grabbing all the edges for the
		// node. Once the prefix no longer matches, then we know we're
		// done.
		var nodeStart [33 + 8]byte
		copy(nodeStart[:], nodePub)
		copy(nodeStart[33:], chanStart[:])

		// Starting from the key pubKey || 0, we seek forward in the
		// bucket until the retrieved key no longer has the public key
		// as its prefix. This indicates that we've stepped over into
		// another node's edges, so we can terminate our scan.
		edgeCursor := edges.ReadCursor()
		for nodeEdge, _ := edgeCursor.Seek(nodeStart[:]); bytes.HasPrefix(nodeEdge, nodePub); nodeEdge, _ = edgeCursor.Next() {
			// If the prefix still matches, the channel id is
			// returned in nodeEdge. Channel id is used to lookup
			// the node at the other end of the channel and both
			// edge policies.
			chanID := nodeEdge[33:]
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				return err
			}
			edgeInfo.db = db

			outgoingPolicy, err := fetchChanEdgePolicy(
				edges, chanID, nodePub, nodes,
			)
			if err != nil {
				return err
			}

			otherNode, err := edgeInfo.OtherNodeKeyBytes(nodePub)
			if err != nil {
				return err
			}

			incomingPolicy, err := fetchChanEdgePolicy(
				edges, chanID, otherNode[:], nodes,
			)
			if err != nil {
				return err
			}

			// Finally, we execute the callback.
			err = cb(tx, &edgeInfo, outgoingPolicy, incomingPolicy)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// If no transaction was provided, then we'll create a new transaction
	// to execute the transaction within.
	if tx == nil {
		return kvdb.View(db, traversal, func() {})
	}

	// Otherwise, we re-use the existing transaction to execute the graph
	// traversal.
	return traversal(tx)
}

// ForEachChannel iterates through all channels of this node, executing the
// passed callback with an edge info structure and the policies of each end
// of the channel. The first edge policy is the outgoing edge *to* the
// connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
//
// If the caller wishes to re-use an existing boltdb transaction, then it
// should be passed as the first argument.  Otherwise the first argument should
// be nil and a fresh transaction will be created to execute the graph
// traversal.
func (l *LightningNode) ForEachChannel(tx kvdb.RTx,
	cb func(kvdb.RTx, *ChannelEdgeInfo, *ChannelEdgePolicy,
		*ChannelEdgePolicy) error) error {

	nodePub := l.PubKeyBytes[:]
	db := l.db

	return nodeTraversal(tx, nodePub, db, cb)
}

// ChannelEdgeInfo represents a fully authenticated channel along with all its
// unique attributes. Once an authenticated channel announcement has been
// processed on the network, then an instance of ChannelEdgeInfo encapsulating
// the channels attributes is stored. The other portions relevant to routing
// policy of a channel are stored within a ChannelEdgePolicy for each direction
// of the channel.
type ChannelEdgeInfo struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// ChainHash is the hash that uniquely identifies the chain that this
	// channel was opened within.
	//
	// TODO(roasbeef): need to modify db keying for multi-chain
	//  * must add chain hash to prefix as well
	ChainHash chainhash.Hash

	// NodeKey1Bytes is the raw public key of the first node.
	NodeKey1Bytes [33]byte
	nodeKey1      *btcec.PublicKey

	// NodeKey2Bytes is the raw public key of the first node.
	NodeKey2Bytes [33]byte
	nodeKey2      *btcec.PublicKey

	// BitcoinKey1Bytes is the raw public key of the first node.
	BitcoinKey1Bytes [33]byte
	bitcoinKey1      *btcec.PublicKey

	// BitcoinKey2Bytes is the raw public key of the first node.
	BitcoinKey2Bytes [33]byte
	bitcoinKey2      *btcec.PublicKey

	// Features is an opaque byte slice that encodes the set of channel
	// specific features that this channel edge supports.
	Features []byte

	// AuthProof is the authentication proof for this channel. This proof
	// contains a set of signatures binding four identities, which attests
	// to the legitimacy of the advertised channel.
	AuthProof *ChannelAuthProof

	// ChannelPoint is the funding outpoint of the channel. This can be
	// used to uniquely identify the channel within the channel graph.
	ChannelPoint wire.OutPoint

	// Capacity is the total capacity of the channel, this is determined by
	// the value output in the outpoint that created this channel.
	Capacity btcutil.Amount

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte

	db kvdb.Backend
}

// AddNodeKeys is a setter-like method that can be used to replace the set of
// keys for the target ChannelEdgeInfo.
func (c *ChannelEdgeInfo) AddNodeKeys(nodeKey1, nodeKey2, bitcoinKey1,
	bitcoinKey2 *btcec.PublicKey) {

	c.nodeKey1 = nodeKey1
	copy(c.NodeKey1Bytes[:], c.nodeKey1.SerializeCompressed())

	c.nodeKey2 = nodeKey2
	copy(c.NodeKey2Bytes[:], nodeKey2.SerializeCompressed())

	c.bitcoinKey1 = bitcoinKey1
	copy(c.BitcoinKey1Bytes[:], c.bitcoinKey1.SerializeCompressed())

	c.bitcoinKey2 = bitcoinKey2
	copy(c.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())
}

// NodeKey1 is the identity public key of the "first" node that was involved in
// the creation of this channel. A node is considered "first" if the
// lexicographical ordering the its serialized public key is "smaller" than
// that of the other node involved in channel creation.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo) NodeKey1() (*btcec.PublicKey, error) {
	if c.nodeKey1 != nil {
		return c.nodeKey1, nil
	}

	key, err := btcec.ParsePubKey(c.NodeKey1Bytes[:])
	if err != nil {
		return nil, err
	}
	c.nodeKey1 = key

	return key, nil
}

// NodeKey2 is the identity public key of the "second" node that was
// involved in the creation of this channel. A node is considered
// "second" if the lexicographical ordering the its serialized public
// key is "larger" than that of the other node involved in channel
// creation.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo) NodeKey2() (*btcec.PublicKey, error) {
	if c.nodeKey2 != nil {
		return c.nodeKey2, nil
	}

	key, err := btcec.ParsePubKey(c.NodeKey2Bytes[:])
	if err != nil {
		return nil, err
	}
	c.nodeKey2 = key

	return key, nil
}

// BitcoinKey1 is the Bitcoin multi-sig key belonging to the first
// node, that was involved in the funding transaction that originally
// created the channel that this struct represents.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo) BitcoinKey1() (*btcec.PublicKey, error) {
	if c.bitcoinKey1 != nil {
		return c.bitcoinKey1, nil
	}

	key, err := btcec.ParsePubKey(c.BitcoinKey1Bytes[:])
	if err != nil {
		return nil, err
	}
	c.bitcoinKey1 = key

	return key, nil
}

// BitcoinKey2 is the Bitcoin multi-sig key belonging to the second
// node, that was involved in the funding transaction that originally
// created the channel that this struct represents.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the pubkey if absolutely necessary.
func (c *ChannelEdgeInfo) BitcoinKey2() (*btcec.PublicKey, error) {
	if c.bitcoinKey2 != nil {
		return c.bitcoinKey2, nil
	}

	key, err := btcec.ParsePubKey(c.BitcoinKey2Bytes[:])
	if err != nil {
		return nil, err
	}
	c.bitcoinKey2 = key

	return key, nil
}

// OtherNodeKeyBytes returns the node key bytes of the other end of
// the channel.
func (c *ChannelEdgeInfo) OtherNodeKeyBytes(thisNodeKey []byte) (
	[33]byte, error) {

	switch {
	case bytes.Equal(c.NodeKey1Bytes[:], thisNodeKey):
		return c.NodeKey2Bytes, nil
	case bytes.Equal(c.NodeKey2Bytes[:], thisNodeKey):
		return c.NodeKey1Bytes, nil
	default:
		return [33]byte{}, fmt.Errorf("node not participating in this channel")
	}
}

// FetchOtherNode attempts to fetch the full LightningNode that's opposite of
// the target node in the channel. This is useful when one knows the pubkey of
// one of the nodes, and wishes to obtain the full LightningNode for the other
// end of the channel.
func (c *ChannelEdgeInfo) FetchOtherNode(tx kvdb.RTx,
	thisNodeKey []byte) (*LightningNode, error) {

	// Ensure that the node passed in is actually a member of the channel.
	var targetNodeBytes [33]byte
	switch {
	case bytes.Equal(c.NodeKey1Bytes[:], thisNodeKey):
		targetNodeBytes = c.NodeKey2Bytes
	case bytes.Equal(c.NodeKey2Bytes[:], thisNodeKey):
		targetNodeBytes = c.NodeKey1Bytes
	default:
		return nil, fmt.Errorf("node not participating in this channel")
	}

	var targetNode *LightningNode
	fetchNodeFunc := func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		node, err := fetchLightningNode(nodes, targetNodeBytes[:])
		if err != nil {
			return err
		}
		node.db = c.db

		targetNode = &node

		return nil
	}

	// If the transaction is nil, then we'll need to create a new one,
	// otherwise we can use the existing db transaction.
	var err error
	if tx == nil {
		err = kvdb.View(c.db, fetchNodeFunc, func() { targetNode = nil })
	} else {
		err = fetchNodeFunc(tx)
	}

	return targetNode, err
}

// ChannelAuthProof is the authentication proof (the signature portion) for a
// channel. Using the four signatures contained in the struct, and some
// auxiliary knowledge (the funding script, node identities, and outpoint) nodes
// on the network are able to validate the authenticity and existence of a
// channel. Each of these signatures signs the following digest: chanID ||
// nodeID1 || nodeID2 || bitcoinKey1|| bitcoinKey2 || 2-byte-feature-len ||
// features.
type ChannelAuthProof struct {
	// nodeSig1 is a cached instance of the first node signature.
	nodeSig1 *ecdsa.Signature

	// NodeSig1Bytes are the raw bytes of the first node signature encoded
	// in DER format.
	NodeSig1Bytes []byte

	// nodeSig2 is a cached instance of the second node signature.
	nodeSig2 *ecdsa.Signature

	// NodeSig2Bytes are the raw bytes of the second node signature
	// encoded in DER format.
	NodeSig2Bytes []byte

	// bitcoinSig1 is a cached instance of the first bitcoin signature.
	bitcoinSig1 *ecdsa.Signature

	// BitcoinSig1Bytes are the raw bytes of the first bitcoin signature
	// encoded in DER format.
	BitcoinSig1Bytes []byte

	// bitcoinSig2 is a cached instance of the second bitcoin signature.
	bitcoinSig2 *ecdsa.Signature

	// BitcoinSig2Bytes are the raw bytes of the second bitcoin signature
	// encoded in DER format.
	BitcoinSig2Bytes []byte
}

// Node1Sig is the signature using the identity key of the node that is first
// in a lexicographical ordering of the serialized public keys of the two nodes
// that created the channel.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) Node1Sig() (*ecdsa.Signature, error) {
	if c.nodeSig1 != nil {
		return c.nodeSig1, nil
	}

	sig, err := ecdsa.ParseSignature(c.NodeSig1Bytes)
	if err != nil {
		return nil, err
	}

	c.nodeSig1 = sig

	return sig, nil
}

// Node2Sig is the signature using the identity key of the node that is second
// in a lexicographical ordering of the serialized public keys of the two nodes
// that created the channel.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) Node2Sig() (*ecdsa.Signature, error) {
	if c.nodeSig2 != nil {
		return c.nodeSig2, nil
	}

	sig, err := ecdsa.ParseSignature(c.NodeSig2Bytes)
	if err != nil {
		return nil, err
	}

	c.nodeSig2 = sig

	return sig, nil
}

// BitcoinSig1 is the signature using the public key of the first node that was
// used in the channel's multi-sig output.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) BitcoinSig1() (*ecdsa.Signature, error) {
	if c.bitcoinSig1 != nil {
		return c.bitcoinSig1, nil
	}

	sig, err := ecdsa.ParseSignature(c.BitcoinSig1Bytes)
	if err != nil {
		return nil, err
	}

	c.bitcoinSig1 = sig

	return sig, nil
}

// BitcoinSig2 is the signature using the public key of the second node that
// was used in the channel's multi-sig output.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelAuthProof) BitcoinSig2() (*ecdsa.Signature, error) {
	if c.bitcoinSig2 != nil {
		return c.bitcoinSig2, nil
	}

	sig, err := ecdsa.ParseSignature(c.BitcoinSig2Bytes)
	if err != nil {
		return nil, err
	}

	c.bitcoinSig2 = sig

	return sig, nil
}

// IsEmpty check is the authentication proof is empty Proof is empty if at
// least one of the signatures are equal to nil.
func (c *ChannelAuthProof) IsEmpty() bool {
	return len(c.NodeSig1Bytes) == 0 ||
		len(c.NodeSig2Bytes) == 0 ||
		len(c.BitcoinSig1Bytes) == 0 ||
		len(c.BitcoinSig2Bytes) == 0
}

// ChannelEdgePolicy represents a *directed* edge within the channel graph. For
// each channel in the database, there are two distinct edges: one for each
// possible direction of travel along the channel. The edges themselves hold
// information concerning fees, and minimum time-lock information which is
// utilized during path finding.
type ChannelEdgePolicy struct {
	// SigBytes is the raw bytes of the signature of the channel edge
	// policy. We'll only parse these if the caller needs to access the
	// signature for validation purposes. Do not set SigBytes directly, but
	// use SetSigBytes instead to make sure that the cache is invalidated.
	SigBytes []byte

	// sig is a cached fully parsed signature.
	sig *ecdsa.Signature

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// LastUpdate is the last time an authenticated edge for this channel
	// was received.
	LastUpdate time.Time

	// MessageFlags is a bitfield which indicates the presence of optional
	// fields (like max_htlc) in the policy.
	MessageFlags lnwire.ChanUpdateMsgFlags

	// ChannelFlags is a bitfield which signals the capabilities of the
	// channel as well as the directed edge this update applies to.
	ChannelFlags lnwire.ChanUpdateChanFlags

	// TimeLockDelta is the number of blocks this node will subtract from
	// the expiry of an incoming HTLC. This value expresses the time buffer
	// the node would like to HTLC exchanges.
	TimeLockDelta uint16

	// MinHTLC is the smallest value HTLC this node will forward, expressed
	// in millisatoshi.
	MinHTLC lnwire.MilliSatoshi

	// MaxHTLC is the largest value HTLC this node will forward, expressed
	// in millisatoshi.
	MaxHTLC lnwire.MilliSatoshi

	// FeeBaseMSat is the base HTLC fee that will be charged for forwarding
	// ANY HTLC, expressed in mSAT's.
	FeeBaseMSat lnwire.MilliSatoshi

	// FeeProportionalMillionths is the rate that the node will charge for
	// HTLCs for each millionth of a satoshi forwarded.
	FeeProportionalMillionths lnwire.MilliSatoshi

	// Node is the LightningNode that this directed edge leads to. Using
	// this pointer the channel graph can further be traversed.
	Node *LightningNode

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData []byte

	db kvdb.Backend
}

// Signature is a channel announcement signature, which is needed for proper
// edge policy announcement.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelEdgePolicy) Signature() (*ecdsa.Signature, error) {
	if c.sig != nil {
		return c.sig, nil
	}

	sig, err := ecdsa.ParseSignature(c.SigBytes)
	if err != nil {
		return nil, err
	}

	c.sig = sig

	return sig, nil
}

// SetSigBytes updates the signature and invalidates the cached parsed
// signature.
func (c *ChannelEdgePolicy) SetSigBytes(sig []byte) {
	c.SigBytes = sig
	c.sig = nil
}

// IsDisabled determines whether the edge has the disabled bit set.
func (c *ChannelEdgePolicy) IsDisabled() bool {
	return c.ChannelFlags.IsDisabled()
}

// ComputeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func (c *ChannelEdgePolicy) ComputeFee(
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return c.FeeBaseMSat + (amt*c.FeeProportionalMillionths)/feeRateParts
}

// divideCeil divides dividend by factor and rounds the result up.
func divideCeil(dividend, factor lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return (dividend + factor - 1) / factor
}

// ComputeFeeFromIncoming computes the fee to forward an HTLC given the incoming
// amount.
func (c *ChannelEdgePolicy) ComputeFeeFromIncoming(
	incomingAmt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return incomingAmt - divideCeil(
		feeRateParts*(incomingAmt-c.FeeBaseMSat),
		feeRateParts+c.FeeProportionalMillionths,
	)
}

// FetchChannelEdgesByOutpoint attempts to lookup the two directed edges for
// the channel identified by the funding outpoint. If the channel can't be
// found, then ErrEdgeNotFound is returned. A struct which houses the general
// information for the channel itself is returned as well as two structs that
// contain the routing policies for the channel in either direction.
func (c *ChannelGraph) FetchChannelEdgesByOutpoint(op *wire.OutPoint,
) (*ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy, error) {

	var (
		edgeInfo *ChannelEdgeInfo
		policy1  *ChannelEdgePolicy
		policy2  *ChannelEdgePolicy
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// If the channel's outpoint doesn't exist within the outpoint
		// index, then the edge does not exist.
		chanIndex := edges.NestedReadBucket(channelPointBucket)
		if chanIndex == nil {
			return ErrGraphNoEdgesFound
		}
		var b bytes.Buffer
		if err := writeOutpoint(&b, op); err != nil {
			return err
		}
		chanID := chanIndex.Get(b.Bytes())
		if chanID == nil {
			return ErrEdgeNotFound
		}

		// If the channel is found to exists, then we'll first retrieve
		// the general information for the channel.
		edge, err := fetchChanEdgeInfo(edgeIndex, chanID)
		if err != nil {
			return err
		}
		edgeInfo = &edge
		edgeInfo.db = c.db

		// Once we have the information about the channels' parameters,
		// we'll fetch the routing policies for each for the directed
		// edges.
		e1, e2, err := fetchChanEdgePolicies(
			edgeIndex, edges, nodes, chanID, c.db,
		)
		if err != nil {
			return err
		}

		policy1 = e1
		policy2 = e2
		return nil
	}, func() {
		edgeInfo = nil
		policy1 = nil
		policy2 = nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return edgeInfo, policy1, policy2, nil
}

// FetchChannelEdgesByID attempts to lookup the two directed edges for the
// channel identified by the channel ID. If the channel can't be found, then
// ErrEdgeNotFound is returned. A struct which houses the general information
// for the channel itself is returned as well as two structs that contain the
// routing policies for the channel in either direction.
//
// ErrZombieEdge an be returned if the edge is currently marked as a zombie
// within the database. In this case, the ChannelEdgePolicy's will be nil, and
// the ChannelEdgeInfo will only include the public keys of each node.
func (c *ChannelGraph) FetchChannelEdgesByID(chanID uint64,
) (*ChannelEdgeInfo, *ChannelEdgePolicy, *ChannelEdgePolicy, error) {

	var (
		edgeInfo  *ChannelEdgeInfo
		policy1   *ChannelEdgePolicy
		policy2   *ChannelEdgePolicy
		channelID [8]byte
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		byteOrder.PutUint64(channelID[:], chanID)

		// Now, attempt to fetch edge.
		edge, err := fetchChanEdgeInfo(edgeIndex, channelID[:])

		// If it doesn't exist, we'll quickly check our zombie index to
		// see if we've previously marked it as so.
		if err == ErrEdgeNotFound {
			// If the zombie index doesn't exist, or the edge is not
			// marked as a zombie within it, then we'll return the
			// original ErrEdgeNotFound error.
			zombieIndex := edges.NestedReadBucket(zombieBucket)
			if zombieIndex == nil {
				return ErrEdgeNotFound
			}

			isZombie, pubKey1, pubKey2 := isZombieEdge(
				zombieIndex, chanID,
			)
			if !isZombie {
				return ErrEdgeNotFound
			}

			// Otherwise, the edge is marked as a zombie, so we'll
			// populate the edge info with the public keys of each
			// party as this is the only information we have about
			// it and return an error signaling so.
			edgeInfo = &ChannelEdgeInfo{
				NodeKey1Bytes: pubKey1,
				NodeKey2Bytes: pubKey2,
			}
			return ErrZombieEdge
		}

		// Otherwise, we'll just return the error if any.
		if err != nil {
			return err
		}

		edgeInfo = &edge
		edgeInfo.db = c.db

		// Then we'll attempt to fetch the accompanying policies of this
		// edge.
		e1, e2, err := fetchChanEdgePolicies(
			edgeIndex, edges, nodes, channelID[:], c.db,
		)
		if err != nil {
			return err
		}

		policy1 = e1
		policy2 = e2
		return nil
	}, func() {
		edgeInfo = nil
		policy1 = nil
		policy2 = nil
	})
	if err == ErrZombieEdge {
		return edgeInfo, nil, nil, err
	}
	if err != nil {
		return nil, nil, nil, err
	}

	return edgeInfo, policy1, policy2, nil
}

// IsPublicNode is a helper method that determines whether the node with the
// given public key is seen as a public node in the graph from the graph's
// source node's point of view.
func (c *ChannelGraph) IsPublicNode(pubKey [33]byte) (bool, error) {
	var nodeIsPublic bool
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}
		ourPubKey := nodes.Get(sourceKey)
		if ourPubKey == nil {
			return ErrSourceNodeNotSet
		}
		node, err := fetchLightningNode(nodes, pubKey[:])
		if err != nil {
			return err
		}

		nodeIsPublic, err = node.isPublic(tx, ourPubKey)
		return err
	}, func() {
		nodeIsPublic = false
	})
	if err != nil {
		return false, err
	}

	return nodeIsPublic, nil
}

// genMultiSigP2WSH generates the p2wsh'd multisig script for 2 of 2 pubkeys.
func genMultiSigP2WSH(aPub, bPub []byte) ([]byte, error) {
	witnessScript, err := input.GenMultiSigScript(aPub, bPub)
	if err != nil {
		return nil, err
	}

	// With the witness script generated, we'll now turn it into a p2wsh
	// script:
	//  * OP_0 <sha256(script)>
	bldr := txscript.NewScriptBuilder(
		txscript.WithScriptAllocSize(input.P2WSHSize),
	)
	bldr.AddOp(txscript.OP_0)
	scriptHash := sha256.Sum256(witnessScript)
	bldr.AddData(scriptHash[:])

	return bldr.Script()
}

// EdgePoint couples the outpoint of a channel with the funding script that it
// creates. The FilteredChainView will use this to watch for spends of this
// edge point on chain. We require both of these values as depending on the
// concrete implementation, either the pkScript, or the out point will be used.
type EdgePoint struct {
	// FundingPkScript is the p2wsh multi-sig script of the target channel.
	FundingPkScript []byte

	// OutPoint is the outpoint of the target channel.
	OutPoint wire.OutPoint
}

// String returns a human readable version of the target EdgePoint. We return
// the outpoint directly as it is enough to uniquely identify the edge point.
func (e *EdgePoint) String() string {
	return e.OutPoint.String()
}

// ChannelView returns the verifiable edge information for each active channel
// within the known channel graph. The set of UTXO's (along with their scripts)
// returned are the ones that need to be watched on chain to detect channel
// closes on the resident blockchain.
func (c *ChannelGraph) ChannelView() ([]EdgePoint, error) {
	var edgePoints []EdgePoint
	if err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		// We're going to iterate over the entire channel index, so
		// we'll need to fetch the edgeBucket to get to the index as
		// it's a sub-bucket.
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		chanIndex := edges.NestedReadBucket(channelPointBucket)
		if chanIndex == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// Once we have the proper bucket, we'll range over each key
		// (which is the channel point for the channel) and decode it,
		// accumulating each entry.
		return chanIndex.ForEach(func(chanPointBytes, chanID []byte) error {
			chanPointReader := bytes.NewReader(chanPointBytes)

			var chanPoint wire.OutPoint
			err := readOutpoint(chanPointReader, &chanPoint)
			if err != nil {
				return err
			}

			edgeInfo, err := fetchChanEdgeInfo(
				edgeIndex, chanID,
			)
			if err != nil {
				return err
			}

			pkScript, err := genMultiSigP2WSH(
				edgeInfo.BitcoinKey1Bytes[:],
				edgeInfo.BitcoinKey2Bytes[:],
			)
			if err != nil {
				return err
			}

			edgePoints = append(edgePoints, EdgePoint{
				FundingPkScript: pkScript,
				OutPoint:        chanPoint,
			})

			return nil
		})
	}, func() {
		edgePoints = nil
	}); err != nil {
		return nil, err
	}

	return edgePoints, nil
}

// NewChannelEdgePolicy returns a new blank ChannelEdgePolicy.
func (c *ChannelGraph) NewChannelEdgePolicy() *ChannelEdgePolicy {
	return &ChannelEdgePolicy{db: c.db}
}

// MarkEdgeZombie attempts to mark a channel identified by its channel ID as a
// zombie. This method is used on an ad-hoc basis, when channels need to be
// marked as zombies outside the normal pruning cycle.
func (c *ChannelGraph) MarkEdgeZombie(chanID uint64,
	pubKey1, pubKey2 [33]byte) error {

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	err := kvdb.Batch(c.db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		zombieIndex, err := edges.CreateBucketIfNotExists(zombieBucket)
		if err != nil {
			return fmt.Errorf("unable to create zombie "+
				"bucket: %w", err)
		}

		if c.graphCache != nil {
			c.graphCache.RemoveChannel(pubKey1, pubKey2, chanID)
		}

		return markEdgeZombie(zombieIndex, chanID, pubKey1, pubKey2)
	})
	if err != nil {
		return err
	}

	c.rejectCache.remove(chanID)
	c.chanCache.remove(chanID)

	return nil
}

// markEdgeZombie marks an edge as a zombie within our zombie index. The public
// keys should represent the node public keys of the two parties involved in the
// edge.
func markEdgeZombie(zombieIndex kvdb.RwBucket, chanID uint64, pubKey1,
	pubKey2 [33]byte) error {

	var k [8]byte
	byteOrder.PutUint64(k[:], chanID)

	var v [66]byte
	copy(v[:33], pubKey1[:])
	copy(v[33:], pubKey2[:])

	return zombieIndex.Put(k[:], v[:])
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
func (c *ChannelGraph) MarkEdgeLive(chanID uint64) error {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		zombieIndex := edges.NestedReadWriteBucket(zombieBucket)
		if zombieIndex == nil {
			return nil
		}

		var k [8]byte
		byteOrder.PutUint64(k[:], chanID)
		return zombieIndex.Delete(k[:])
	}, func() {})
	if err != nil {
		return err
	}

	c.rejectCache.remove(chanID)
	c.chanCache.remove(chanID)

	// We need to add the channel back into our graph cache, otherwise we
	// won't use it for path finding.
	edgeInfos, err := c.FetchChanInfos([]uint64{chanID})
	if err != nil {
		return err
	}
	if c.graphCache != nil {
		for _, edgeInfo := range edgeInfos {
			c.graphCache.AddChannel(
				edgeInfo.Info, edgeInfo.Policy1,
				edgeInfo.Policy2,
			)
		}
	}

	return nil
}

// IsZombieEdge returns whether the edge is considered zombie. If it is a
// zombie, then the two node public keys corresponding to this edge are also
// returned.
func (c *ChannelGraph) IsZombieEdge(chanID uint64) (bool, [33]byte, [33]byte) {
	var (
		isZombie         bool
		pubKey1, pubKey2 [33]byte
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		zombieIndex := edges.NestedReadBucket(zombieBucket)
		if zombieIndex == nil {
			return nil
		}

		isZombie, pubKey1, pubKey2 = isZombieEdge(zombieIndex, chanID)
		return nil
	}, func() {
		isZombie = false
		pubKey1 = [33]byte{}
		pubKey2 = [33]byte{}
	})
	if err != nil {
		return false, [33]byte{}, [33]byte{}
	}

	return isZombie, pubKey1, pubKey2
}

// isZombieEdge returns whether an entry exists for the given channel in the
// zombie index. If an entry exists, then the two node public keys corresponding
// to this edge are also returned.
func isZombieEdge(zombieIndex kvdb.RBucket,
	chanID uint64) (bool, [33]byte, [33]byte) {

	var k [8]byte
	byteOrder.PutUint64(k[:], chanID)

	v := zombieIndex.Get(k[:])
	if v == nil {
		return false, [33]byte{}, [33]byte{}
	}

	var pubKey1, pubKey2 [33]byte
	copy(pubKey1[:], v[:33])
	copy(pubKey2[:], v[33:])

	return true, pubKey1, pubKey2
}

// NumZombies returns the current number of zombie channels in the graph.
func (c *ChannelGraph) NumZombies() (uint64, error) {
	var numZombies uint64
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return nil
		}
		zombieIndex := edges.NestedReadBucket(zombieBucket)
		if zombieIndex == nil {
			return nil
		}

		return zombieIndex.ForEach(func(_, _ []byte) error {
			numZombies++
			return nil
		})
	}, func() {
		numZombies = 0
	})
	if err != nil {
		return 0, err
	}

	return numZombies, nil
}

func putLightningNode(nodeBucket kvdb.RwBucket, aliasBucket kvdb.RwBucket, // nolint:dupl
	updateIndex kvdb.RwBucket, node *LightningNode) error {

	var (
		scratch [16]byte
		b       bytes.Buffer
	)

	pub, err := node.PubKey()
	if err != nil {
		return err
	}
	nodePub := pub.SerializeCompressed()

	// If the node has the update time set, write it, else write 0.
	updateUnix := uint64(0)
	if node.LastUpdate.Unix() > 0 {
		updateUnix = uint64(node.LastUpdate.Unix())
	}

	byteOrder.PutUint64(scratch[:8], updateUnix)
	if _, err := b.Write(scratch[:8]); err != nil {
		return err
	}

	if _, err := b.Write(nodePub); err != nil {
		return err
	}

	// If we got a node announcement for this node, we will have the rest
	// of the data available. If not we don't have more data to write.
	if !node.HaveNodeAnnouncement {
		// Write HaveNodeAnnouncement=0.
		byteOrder.PutUint16(scratch[:2], 0)
		if _, err := b.Write(scratch[:2]); err != nil {
			return err
		}

		return nodeBucket.Put(nodePub, b.Bytes())
	}

	// Write HaveNodeAnnouncement=1.
	byteOrder.PutUint16(scratch[:2], 1)
	if _, err := b.Write(scratch[:2]); err != nil {
		return err
	}

	if err := binary.Write(&b, byteOrder, node.Color.R); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, node.Color.G); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, node.Color.B); err != nil {
		return err
	}

	if err := wire.WriteVarString(&b, 0, node.Alias); err != nil {
		return err
	}

	if err := node.Features.Encode(&b); err != nil {
		return err
	}

	numAddresses := uint16(len(node.Addresses))
	byteOrder.PutUint16(scratch[:2], numAddresses)
	if _, err := b.Write(scratch[:2]); err != nil {
		return err
	}

	for _, address := range node.Addresses {
		if err := serializeAddr(&b, address); err != nil {
			return err
		}
	}

	sigLen := len(node.AuthSigBytes)
	if sigLen > 80 {
		return fmt.Errorf("max sig len allowed is 80, had %v",
			sigLen)
	}

	err = wire.WriteVarBytes(&b, 0, node.AuthSigBytes)
	if err != nil {
		return err
	}

	if len(node.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(node.ExtraOpaqueData))
	}
	err = wire.WriteVarBytes(&b, 0, node.ExtraOpaqueData)
	if err != nil {
		return err
	}

	if err := aliasBucket.Put(nodePub, []byte(node.Alias)); err != nil {
		return err
	}

	// With the alias bucket updated, we'll now update the index that
	// tracks the time series of node updates.
	var indexKey [8 + 33]byte
	byteOrder.PutUint64(indexKey[:8], updateUnix)
	copy(indexKey[8:], nodePub)

	// If there was already an old index entry for this node, then we'll
	// delete the old one before we write the new entry.
	if nodeBytes := nodeBucket.Get(nodePub); nodeBytes != nil {
		// Extract out the old update time to we can reconstruct the
		// prior index key to delete it from the index.
		oldUpdateTime := nodeBytes[:8]

		var oldIndexKey [8 + 33]byte
		copy(oldIndexKey[:8], oldUpdateTime)
		copy(oldIndexKey[8:], nodePub)

		if err := updateIndex.Delete(oldIndexKey[:]); err != nil {
			return err
		}
	}

	if err := updateIndex.Put(indexKey[:], nil); err != nil {
		return err
	}

	return nodeBucket.Put(nodePub, b.Bytes())
}

func fetchLightningNode(nodeBucket kvdb.RBucket,
	nodePub []byte) (LightningNode, error) {

	nodeBytes := nodeBucket.Get(nodePub)
	if nodeBytes == nil {
		return LightningNode{}, ErrGraphNodeNotFound
	}

	nodeReader := bytes.NewReader(nodeBytes)
	return deserializeLightningNode(nodeReader)
}

func deserializeLightningNodeCacheable(r io.Reader) (*graphCacheNode, error) {
	// Always populate a feature vector, even if we don't have a node
	// announcement and short circuit below.
	node := newGraphCacheNode(
		route.Vertex{},
		lnwire.EmptyFeatureVector(),
	)

	var nodeScratch [8]byte

	// Skip ahead:
	// - LastUpdate (8 bytes)
	if _, err := r.Read(nodeScratch[:]); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, node.pubKeyBytes[:]); err != nil {
		return nil, err
	}

	// Read the node announcement flag.
	if _, err := r.Read(nodeScratch[:2]); err != nil {
		return nil, err
	}
	hasNodeAnn := byteOrder.Uint16(nodeScratch[:2])

	// The rest of the data is optional, and will only be there if we got a
	// node announcement for this node.
	if hasNodeAnn == 0 {
		return node, nil
	}

	// We did get a node announcement for this node, so we'll have the rest
	// of the data available.
	var rgb uint8
	if err := binary.Read(r, byteOrder, &rgb); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &rgb); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &rgb); err != nil {
		return nil, err
	}

	if _, err := wire.ReadVarString(r, 0); err != nil {
		return nil, err
	}

	if err := node.features.Decode(r); err != nil {
		return nil, err
	}

	return node, nil
}

func deserializeLightningNode(r io.Reader) (LightningNode, error) {
	var (
		node    LightningNode
		scratch [8]byte
		err     error
	)

	// Always populate a feature vector, even if we don't have a node
	// announcement and short circuit below.
	node.Features = lnwire.EmptyFeatureVector()

	if _, err := r.Read(scratch[:]); err != nil {
		return LightningNode{}, err
	}

	unix := int64(byteOrder.Uint64(scratch[:]))
	node.LastUpdate = time.Unix(unix, 0)

	if _, err := io.ReadFull(r, node.PubKeyBytes[:]); err != nil {
		return LightningNode{}, err
	}

	if _, err := r.Read(scratch[:2]); err != nil {
		return LightningNode{}, err
	}

	hasNodeAnn := byteOrder.Uint16(scratch[:2])
	if hasNodeAnn == 1 {
		node.HaveNodeAnnouncement = true
	} else {
		node.HaveNodeAnnouncement = false
	}

	// The rest of the data is optional, and will only be there if we got a node
	// announcement for this node.
	if !node.HaveNodeAnnouncement {
		return node, nil
	}

	// We did get a node announcement for this node, so we'll have the rest
	// of the data available.
	if err := binary.Read(r, byteOrder, &node.Color.R); err != nil {
		return LightningNode{}, err
	}
	if err := binary.Read(r, byteOrder, &node.Color.G); err != nil {
		return LightningNode{}, err
	}
	if err := binary.Read(r, byteOrder, &node.Color.B); err != nil {
		return LightningNode{}, err
	}

	node.Alias, err = wire.ReadVarString(r, 0)
	if err != nil {
		return LightningNode{}, err
	}

	err = node.Features.Decode(r)
	if err != nil {
		return LightningNode{}, err
	}

	if _, err := r.Read(scratch[:2]); err != nil {
		return LightningNode{}, err
	}
	numAddresses := int(byteOrder.Uint16(scratch[:2]))

	var addresses []net.Addr
	for i := 0; i < numAddresses; i++ {
		address, err := deserializeAddr(r)
		if err != nil {
			return LightningNode{}, err
		}
		addresses = append(addresses, address)
	}
	node.Addresses = addresses

	node.AuthSigBytes, err = wire.ReadVarBytes(r, 0, 80, "sig")
	if err != nil {
		return LightningNode{}, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the node as is.
	node.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case err == io.ErrUnexpectedEOF:
	case err == io.EOF:
	case err != nil:
		return LightningNode{}, err
	}

	return node, nil
}

func putChanEdgeInfo(edgeIndex kvdb.RwBucket, edgeInfo *ChannelEdgeInfo, chanID [8]byte) error {
	var b bytes.Buffer

	if _, err := b.Write(edgeInfo.NodeKey1Bytes[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.NodeKey2Bytes[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, edgeInfo.Features); err != nil {
		return err
	}

	authProof := edgeInfo.AuthProof
	var nodeSig1, nodeSig2, bitcoinSig1, bitcoinSig2 []byte
	if authProof != nil {
		nodeSig1 = authProof.NodeSig1Bytes
		nodeSig2 = authProof.NodeSig2Bytes
		bitcoinSig1 = authProof.BitcoinSig1Bytes
		bitcoinSig2 = authProof.BitcoinSig2Bytes
	}

	if err := wire.WriteVarBytes(&b, 0, nodeSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, nodeSig2); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, bitcoinSig1); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, bitcoinSig2); err != nil {
		return err
	}

	if err := writeOutpoint(&b, &edgeInfo.ChannelPoint); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edgeInfo.Capacity)); err != nil {
		return err
	}
	if _, err := b.Write(chanID[:]); err != nil {
		return err
	}
	if _, err := b.Write(edgeInfo.ChainHash[:]); err != nil {
		return err
	}

	if len(edgeInfo.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edgeInfo.ExtraOpaqueData))
	}
	err := wire.WriteVarBytes(&b, 0, edgeInfo.ExtraOpaqueData)
	if err != nil {
		return err
	}

	return edgeIndex.Put(chanID[:], b.Bytes())
}

func fetchChanEdgeInfo(edgeIndex kvdb.RBucket,
	chanID []byte) (ChannelEdgeInfo, error) {

	edgeInfoBytes := edgeIndex.Get(chanID)
	if edgeInfoBytes == nil {
		return ChannelEdgeInfo{}, ErrEdgeNotFound
	}

	edgeInfoReader := bytes.NewReader(edgeInfoBytes)
	return deserializeChanEdgeInfo(edgeInfoReader)
}

func deserializeChanEdgeInfo(r io.Reader) (ChannelEdgeInfo, error) {
	var (
		err      error
		edgeInfo ChannelEdgeInfo
	)

	if _, err := io.ReadFull(r, edgeInfo.NodeKey1Bytes[:]); err != nil {
		return ChannelEdgeInfo{}, err
	}
	if _, err := io.ReadFull(r, edgeInfo.NodeKey2Bytes[:]); err != nil {
		return ChannelEdgeInfo{}, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return ChannelEdgeInfo{}, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return ChannelEdgeInfo{}, err
	}

	edgeInfo.Features, err = wire.ReadVarBytes(r, 0, 900, "features")
	if err != nil {
		return ChannelEdgeInfo{}, err
	}

	proof := &ChannelAuthProof{}

	proof.NodeSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return ChannelEdgeInfo{}, err
	}
	proof.NodeSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return ChannelEdgeInfo{}, err
	}
	proof.BitcoinSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return ChannelEdgeInfo{}, err
	}
	proof.BitcoinSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return ChannelEdgeInfo{}, err
	}

	if !proof.IsEmpty() {
		edgeInfo.AuthProof = proof
	}

	edgeInfo.ChannelPoint = wire.OutPoint{}
	if err := readOutpoint(r, &edgeInfo.ChannelPoint); err != nil {
		return ChannelEdgeInfo{}, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.Capacity); err != nil {
		return ChannelEdgeInfo{}, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.ChannelID); err != nil {
		return ChannelEdgeInfo{}, err
	}

	if _, err := io.ReadFull(r, edgeInfo.ChainHash[:]); err != nil {
		return ChannelEdgeInfo{}, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edgeInfo.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case err == io.ErrUnexpectedEOF:
	case err == io.EOF:
	case err != nil:
		return ChannelEdgeInfo{}, err
	}

	return edgeInfo, nil
}

func putChanEdgePolicy(edges, nodes kvdb.RwBucket, edge *ChannelEdgePolicy,
	from, to []byte) error {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], from)
	byteOrder.PutUint64(edgeKey[33:], edge.ChannelID)

	var b bytes.Buffer
	if err := serializeChanEdgePolicy(&b, edge, to); err != nil {
		return err
	}

	// Before we write out the new edge, we'll create a new entry in the
	// update index in order to keep it fresh.
	updateUnix := uint64(edge.LastUpdate.Unix())
	var indexKey [8 + 8]byte
	byteOrder.PutUint64(indexKey[:8], updateUnix)
	byteOrder.PutUint64(indexKey[8:], edge.ChannelID)

	updateIndex, err := edges.CreateBucketIfNotExists(edgeUpdateIndexBucket)
	if err != nil {
		return err
	}

	// If there was already an entry for this edge, then we'll need to
	// delete the old one to ensure we don't leave around any after-images.
	// An unknown policy value does not have a update time recorded, so
	// it also does not need to be removed.
	if edgeBytes := edges.Get(edgeKey[:]); edgeBytes != nil &&
		!bytes.Equal(edgeBytes[:], unknownPolicy) {

		// In order to delete the old entry, we'll need to obtain the
		// *prior* update time in order to delete it. To do this, we'll
		// need to deserialize the existing policy within the database
		// (now outdated by the new one), and delete its corresponding
		// entry within the update index. We'll ignore any
		// ErrEdgePolicyOptionalFieldNotFound error, as we only need
		// the channel ID and update time to delete the entry.
		// TODO(halseth): get rid of these invalid policies in a
		// migration.
		oldEdgePolicy, err := deserializeChanEdgePolicy(
			bytes.NewReader(edgeBytes), nodes,
		)
		if err != nil && err != ErrEdgePolicyOptionalFieldNotFound {
			return err
		}

		oldUpdateTime := uint64(oldEdgePolicy.LastUpdate.Unix())

		var oldIndexKey [8 + 8]byte
		byteOrder.PutUint64(oldIndexKey[:8], oldUpdateTime)
		byteOrder.PutUint64(oldIndexKey[8:], edge.ChannelID)

		if err := updateIndex.Delete(oldIndexKey[:]); err != nil {
			return err
		}
	}

	if err := updateIndex.Put(indexKey[:], nil); err != nil {
		return err
	}

	updateEdgePolicyDisabledIndex(
		edges, edge.ChannelID,
		edge.ChannelFlags&lnwire.ChanUpdateDirection > 0,
		edge.IsDisabled(),
	)

	return edges.Put(edgeKey[:], b.Bytes()[:])
}

// updateEdgePolicyDisabledIndex is used to update the disabledEdgePolicyIndex
// bucket by either add a new disabled ChannelEdgePolicy or remove an existing
// one.
// The direction represents the direction of the edge and disabled is used for
// deciding whether to remove or add an entry to the bucket.
// In general a channel is disabled if two entries for the same chanID exist
// in this bucket.
// Maintaining the bucket this way allows a fast retrieval of disabled
// channels, for example when prune is needed.
func updateEdgePolicyDisabledIndex(edges kvdb.RwBucket, chanID uint64,
	direction bool, disabled bool) error {

	var disabledEdgeKey [8 + 1]byte
	byteOrder.PutUint64(disabledEdgeKey[0:], chanID)
	if direction {
		disabledEdgeKey[8] = 1
	}

	disabledEdgePolicyIndex, err := edges.CreateBucketIfNotExists(
		disabledEdgePolicyBucket,
	)
	if err != nil {
		return err
	}

	if disabled {
		return disabledEdgePolicyIndex.Put(disabledEdgeKey[:], []byte{})
	}

	return disabledEdgePolicyIndex.Delete(disabledEdgeKey[:])
}

// putChanEdgePolicyUnknown marks the edge policy as unknown
// in the edges bucket.
func putChanEdgePolicyUnknown(edges kvdb.RwBucket, channelID uint64,
	from []byte) error {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], from)
	byteOrder.PutUint64(edgeKey[33:], channelID)

	if edges.Get(edgeKey[:]) != nil {
		return fmt.Errorf("cannot write unknown policy for channel %v "+
			" when there is already a policy present", channelID)
	}

	return edges.Put(edgeKey[:], unknownPolicy)
}

func fetchChanEdgePolicy(edges kvdb.RBucket, chanID []byte,
	nodePub []byte, nodes kvdb.RBucket) (*ChannelEdgePolicy, error) {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], nodePub)
	copy(edgeKey[33:], chanID[:])

	edgeBytes := edges.Get(edgeKey[:])
	if edgeBytes == nil {
		return nil, ErrEdgeNotFound
	}

	// No need to deserialize unknown policy.
	if bytes.Equal(edgeBytes[:], unknownPolicy) {
		return nil, nil
	}

	edgeReader := bytes.NewReader(edgeBytes)

	ep, err := deserializeChanEdgePolicy(edgeReader, nodes)
	switch {
	// If the db policy was missing an expected optional field, we return
	// nil as if the policy was unknown.
	case err == ErrEdgePolicyOptionalFieldNotFound:
		return nil, nil

	case err != nil:
		return nil, err
	}

	return ep, nil
}

func fetchChanEdgePolicies(edgeIndex kvdb.RBucket, edges kvdb.RBucket,
	nodes kvdb.RBucket, chanID []byte,
	db kvdb.Backend) (*ChannelEdgePolicy, *ChannelEdgePolicy, error) {

	edgeInfo := edgeIndex.Get(chanID)
	if edgeInfo == nil {
		return nil, nil, ErrEdgeNotFound
	}

	// The first node is contained within the first half of the edge
	// information. We only propagate the error here and below if it's
	// something other than edge non-existence.
	node1Pub := edgeInfo[:33]
	edge1, err := fetchChanEdgePolicy(edges, chanID, node1Pub, nodes)
	if err != nil {
		return nil, nil, err
	}

	// As we may have a single direction of the edge but not the other,
	// only fill in the database pointers if the edge is found.
	if edge1 != nil {
		edge1.db = db
		edge1.Node.db = db
	}

	// Similarly, the second node is contained within the latter
	// half of the edge information.
	node2Pub := edgeInfo[33:66]
	edge2, err := fetchChanEdgePolicy(edges, chanID, node2Pub, nodes)
	if err != nil {
		return nil, nil, err
	}

	if edge2 != nil {
		edge2.db = db
		edge2.Node.db = db
	}

	return edge1, edge2, nil
}

func serializeChanEdgePolicy(w io.Writer, edge *ChannelEdgePolicy,
	to []byte) error {

	err := wire.WriteVarBytes(w, 0, edge.SigBytes)
	if err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, edge.ChannelID); err != nil {
		return err
	}

	var scratch [8]byte
	updateUnix := uint64(edge.LastUpdate.Unix())
	byteOrder.PutUint64(scratch[:], updateUnix)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, edge.MessageFlags); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, edge.ChannelFlags); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, edge.TimeLockDelta); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, uint64(edge.MinHTLC)); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, uint64(edge.FeeBaseMSat)); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, uint64(edge.FeeProportionalMillionths)); err != nil {
		return err
	}

	if _, err := w.Write(to); err != nil {
		return err
	}

	// If the max_htlc field is present, we write it. To be compatible with
	// older versions that wasn't aware of this field, we write it as part
	// of the opaque data.
	// TODO(halseth): clean up when moving to TLV.
	var opaqueBuf bytes.Buffer
	if edge.MessageFlags.HasMaxHtlc() {
		err := binary.Write(&opaqueBuf, byteOrder, uint64(edge.MaxHTLC))
		if err != nil {
			return err
		}
	}

	if len(edge.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edge.ExtraOpaqueData))
	}
	if _, err := opaqueBuf.Write(edge.ExtraOpaqueData); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, opaqueBuf.Bytes()); err != nil {
		return err
	}
	return nil
}

func deserializeChanEdgePolicy(r io.Reader,
	nodes kvdb.RBucket) (*ChannelEdgePolicy, error) {

	// Deserialize the policy. Note that in case an optional field is not
	// found, both an error and a populated policy object are returned.
	edge, deserializeErr := deserializeChanEdgePolicyRaw(r)
	if deserializeErr != nil &&
		deserializeErr != ErrEdgePolicyOptionalFieldNotFound {

		return nil, deserializeErr
	}

	// Populate full LightningNode struct.
	pub := edge.Node.PubKeyBytes[:]
	node, err := fetchLightningNode(nodes, pub)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch node: %x, %v", pub, err)
	}
	edge.Node = &node

	return edge, deserializeErr
}

func deserializeChanEdgePolicyRaw(r io.Reader) (*ChannelEdgePolicy, error) {
	edge := &ChannelEdgePolicy{}

	var err error
	edge.SigBytes, err = wire.ReadVarBytes(r, 0, 80, "sig")
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &edge.ChannelID); err != nil {
		return nil, err
	}

	var scratch [8]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	unix := int64(byteOrder.Uint64(scratch[:]))
	edge.LastUpdate = time.Unix(unix, 0)

	if err := binary.Read(r, byteOrder, &edge.MessageFlags); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edge.ChannelFlags); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edge.TimeLockDelta); err != nil {
		return nil, err
	}

	var n uint64
	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.MinHTLC = lnwire.MilliSatoshi(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeBaseMSat = lnwire.MilliSatoshi(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeProportionalMillionths = lnwire.MilliSatoshi(n)

	var pub [33]byte
	if _, err := r.Read(pub[:]); err != nil {
		return nil, err
	}
	edge.Node = &LightningNode{
		PubKeyBytes: pub,
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edge.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case err == io.ErrUnexpectedEOF:
	case err == io.EOF:
	case err != nil:
		return nil, err
	}

	// See if optional fields are present.
	if edge.MessageFlags.HasMaxHtlc() {
		// The max_htlc field should be at the beginning of the opaque
		// bytes.
		opq := edge.ExtraOpaqueData

		// If the max_htlc field is not present, it might be old data
		// stored before this field was validated. We'll return the
		// edge along with an error.
		if len(opq) < 8 {
			return edge, ErrEdgePolicyOptionalFieldNotFound
		}

		maxHtlc := byteOrder.Uint64(opq[:8])
		edge.MaxHTLC = lnwire.MilliSatoshi(maxHtlc)

		// Exclude the parsed field from the rest of the opaque data.
		edge.ExtraOpaqueData = opq[8:]
	}

	return edge, nil
}
