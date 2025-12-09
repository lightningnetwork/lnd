package graphdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"io"
	"iter"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
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

	// disabledEdgePolicyBucket is a sub-bucket of the main edgeBucket
	// bucket responsible for maintaining an index of disabled edge
	// policies. Each entry exists within the bucket as follows:
	//
	// maps: <chanID><direction> -> []byte{}
	//
	// The chanID represents the channel ID of the edge and the direction is
	// one byte representing the direction of the edge. The main purpose of
	// this index is to allow pruning disabled channels in a fast way
	// without the need to iterate all over the graph.
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

	// closedScidBucket is a top-level bucket that stores scids for
	// channels that we know to be closed. This is used so that we don't
	// need to perform expensive validation checks if we receive a channel
	// announcement for the channel again.
	//
	// maps: scid -> []byte{}
	closedScidBucket = []byte("closed-scid")
)

const (
	// MaxAllowedExtraOpaqueBytes is the largest amount of opaque bytes that
	// we'll permit to be written to disk. We limit this as otherwise, it
	// would be possible for a node to create a ton of updates and slowly
	// fill our disk, and also waste bandwidth due to relaying.
	MaxAllowedExtraOpaqueBytes = 10000
)

// KVStore is a persistent, on-disk graph representation of the Lightning
// Network. This struct can be used to implement path finding algorithms on top
// of, and also to update a node's view based on information received from the
// p2p network. Internally, the graph is stored using a modified adjacency list
// representation with some added object interaction possible with each
// serialized edge/node. The graph is stored is directed, meaning that are two
// edges stored for each channel: an inbound/outbound edge for each node pair.
// Nodes, edges, and edge information can all be added to the graph
// independently. Edge removal results in the deletion of all edge information
// for that edge.
type KVStore struct {
	db kvdb.Backend

	// cacheMu guards all caches (rejectCache and chanCache). If
	// this mutex will be acquired at the same time as the DB mutex then
	// the cacheMu MUST be acquired first to prevent deadlock.
	cacheMu     sync.RWMutex
	rejectCache *rejectCache
	chanCache   *channelCache

	chanScheduler batch.Scheduler[kvdb.RwTx]
	nodeScheduler batch.Scheduler[kvdb.RwTx]
}

// A compile-time assertion to ensure that the KVStore struct implements the
// V1Store interface.
var _ V1Store = (*KVStore)(nil)

// NewKVStore allocates a new KVStore backed by a DB instance. The
// returned instance has its own unique reject cache and channel cache.
func NewKVStore(db kvdb.Backend, options ...StoreOptionModifier) (*KVStore,
	error) {

	opts := DefaultOptions()
	for _, o := range options {
		o(opts)
	}

	if !opts.NoMigration {
		if err := initKVStore(db); err != nil {
			return nil, err
		}
	}

	g := &KVStore{
		db:          db,
		rejectCache: newRejectCache(opts.RejectCacheSize),
		chanCache:   newChannelCache(opts.ChannelCacheSize),
	}
	g.chanScheduler = batch.NewTimeScheduler(
		batch.NewBoltBackend[kvdb.RwTx](db), &g.cacheMu,
		opts.BatchCommitInterval,
	)
	g.nodeScheduler = batch.NewTimeScheduler(
		batch.NewBoltBackend[kvdb.RwTx](db), nil,
		opts.BatchCommitInterval,
	)

	return g, nil
}

// channelMapKey is the key structure used for storing channel edge policies.
type channelMapKey struct {
	nodeKey route.Vertex
	chanID  [8]byte
}

// String returns a human-readable representation of the key.
func (c channelMapKey) String() string {
	return fmt.Sprintf("node=%v, chanID=%x", c.nodeKey, c.chanID)
}

// getChannelMap loads all channel edge policies from the database and stores
// them in a map.
func getChannelMap(edges kvdb.RBucket) (
	map[channelMapKey]*models.ChannelEdgePolicy, error) {

	// Create a map to store all channel edge policies.
	channelMap := make(map[channelMapKey]*models.ChannelEdgePolicy)

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
		case errors.Is(err, ErrEdgePolicyOptionalFieldNotFound):
			return nil

		// We don't want a single policy with bad TLV data to stop us
		// from loading the rest of the data, so we just skip this
		// policy. This is for backwards compatibility since we did not
		// use to validate TLV data in the past before persisting it.
		case errors.Is(err, ErrParsingExtraTLVBytes):
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
	graphMetaBucket,
	closedScidBucket,
}

// createChannelDB creates and initializes a fresh version of  In
// the case that the target path has not yet been created or doesn't yet exist,
// then the path is created. Additionally, all required top-level buckets used
// within the database are created.
func initKVStore(db kvdb.Backend) error {
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
		return fmt.Errorf("unable to create new channel graph: %w", err)
	}

	return nil
}

// AddrsForNode returns all known addresses for the target node public key that
// the graph DB is aware of. The returned boolean indicates if the given node is
// unknown to the graph DB or not.
//
// NOTE: this is part of the channeldb.AddrSource interface.
func (c *KVStore) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	pubKey, err := route.NewVertexFromBytes(nodePub.SerializeCompressed())
	if err != nil {
		return false, nil, err
	}

	node, err := c.FetchNode(ctx, pubKey)
	// We don't consider it an error if the graph is unaware of the node.
	switch {
	case err != nil && !errors.Is(err, ErrGraphNodeNotFound):
		return false, nil, err

	case errors.Is(err, ErrGraphNodeNotFound):
		return false, nil, nil
	}

	return true, node.Addresses, nil
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
func (c *KVStore) ForEachChannel(_ context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	return forEachChannel(c.db, cb, reset)
}

// forEachChannel iterates through all the channel edges stored within the
// graph and invokes the passed callback for each edge. The callback takes two
// edges as since this is a directed graph, both the in/out edges are visited.
// If the callback returns an error, then the transaction is aborted and the
// iteration stops early.
//
// NOTE: If an edge can't be found, or wasn't advertised, then a nil pointer
// for that particular channel edge routing policy will be passed into the
// callback.
func forEachChannel(db kvdb.Backend, cb func(*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy) error,
	reset func()) error {

	return db.View(func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}

		// First, load all edges in memory indexed by node and channel
		// id.
		channelMap, err := getChannelMap(edges)
		if err != nil {
			return err
		}

		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// Load edge index, recombine each channel with the policies
		// loaded above and invoke the callback.
		return kvdb.ForAll(
			edgeIndex, func(k, edgeInfoBytes []byte) error {
				var chanID [8]byte
				copy(chanID[:], k)

				edgeInfoReader := bytes.NewReader(edgeInfoBytes)
				info, err := deserializeChanEdgeInfo(
					edgeInfoReader,
				)
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

				return cb(info, policy1, policy2)
			},
		)
	}, reset)
}

// ForEachChannelCacheable iterates through all the channel edges stored within
// the graph and invokes the passed callback for each edge. The callback takes
// two edges as since this is a directed graph, both the in/out edges are
// visited. If the callback returns an error, then the transaction is aborted
// and the iteration stops early.
//
// NOTE: If an edge can't be found, or wasn't advertised, then a nil pointer
// for that particular channel edge routing policy will be passed into the
// callback.
//
// NOTE: this method is like ForEachChannel but fetches only the data required
// for the graph cache.
func (c *KVStore) ForEachChannelCacheable(cb func(*models.CachedEdgeInfo,
	*models.CachedEdgePolicy, *models.CachedEdgePolicy) error,
	reset func()) error {

	return c.db.View(func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}

		// First, load all edges in memory indexed by node and channel
		// id.
		channelMap, err := getChannelMap(edges)
		if err != nil {
			return err
		}

		edgeIndex := edges.NestedReadBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// Load edge index, recombine each channel with the policies
		// loaded above and invoke the callback.
		return kvdb.ForAll(
			edgeIndex, func(k, edgeInfoBytes []byte) error {
				var chanID [8]byte
				copy(chanID[:], k)

				edgeInfoReader := bytes.NewReader(edgeInfoBytes)
				info, err := deserializeChanEdgeInfo(
					edgeInfoReader,
				)
				if err != nil {
					return err
				}

				key1 := channelMapKey{
					nodeKey: info.NodeKey1Bytes,
					chanID:  chanID,
				}
				policy1 := channelMap[key1]

				key2 := channelMapKey{
					nodeKey: info.NodeKey2Bytes,
					chanID:  chanID,
				}
				policy2 := channelMap[key2]

				// We now create the cached edge policies, but
				// only when the above policies are found in the
				// `channelMap`.
				var (
					cachedPolicy1 *models.CachedEdgePolicy
					cachedPolicy2 *models.CachedEdgePolicy
				)

				if policy1 != nil {
					cachedPolicy1 = models.NewCachedPolicy(
						policy1,
					)
				}

				if policy2 != nil {
					cachedPolicy2 = models.NewCachedPolicy(
						policy2,
					)
				}

				return cb(
					models.NewCachedEdge(info),
					cachedPolicy1, cachedPolicy2,
				)
			},
		)
	}, reset)
}

// forEachNodeDirectedChannel iterates through all channels of a given node,
// executing the passed callback on the directed edge representing the channel
// and its incoming policy. If the callback returns an error, then the iteration
// is halted with the error propagated back up to the caller. An optional read
// transaction may be provided. If none is provided, a new one will be created.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: the reset param is only meaningful if the tx param is nil. If it is
// not nil, the caller is expected to have passed in a reset to the parent
// function's View/Update call which will then apply to the whole transaction.
func (c *KVStore) forEachNodeDirectedChannel(tx kvdb.RTx,
	node route.Vertex, cb func(channel *DirectedChannel) error,
	reset func()) error {

	// Fallback that uses the database.
	toNodeCallback := func() route.Vertex {
		return node
	}
	toNodeFeatures, err := c.fetchNodeFeatures(tx, node)
	if err != nil {
		return err
	}

	dbCallback := func(tx kvdb.RTx, e *models.ChannelEdgeInfo, p1,
		p2 *models.ChannelEdgePolicy) error {

		var cachedInPolicy *models.CachedEdgePolicy
		if p2 != nil {
			cachedInPolicy = models.NewCachedPolicy(p2)
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

		if p1 != nil {
			p1.InboundFee.WhenSome(func(fee lnwire.Fee) {
				directedChannel.InboundFee = fee
			})
		}

		if node == e.NodeKey2Bytes {
			directedChannel.OtherNode = e.NodeKey1Bytes
		}

		return cb(directedChannel)
	}

	return nodeTraversal(tx, node[:], c.db, dbCallback, reset)
}

// fetchNodeFeatures returns the features of a given node. If no features are
// known for the node, an empty feature vector is returned. An optional read
// transaction may be provided. If none is provided, a new one will be created.
func (c *KVStore) fetchNodeFeatures(tx kvdb.RTx,
	node route.Vertex) (*lnwire.FeatureVector, error) {

	// Fallback that uses the database.
	targetNode, err := c.fetchNodeTx(tx, node)
	switch {
	// If the node exists and has features, return them directly.
	case err == nil:
		return targetNode.Features, nil

	// If we couldn't find a node announcement, populate a blank feature
	// vector.
	case errors.Is(err, ErrGraphNodeNotFound):
		return lnwire.EmptyFeatureVector(), nil

	// Otherwise, bubble the error up.
	default:
		return nil, err
	}
}

// ForEachNodeDirectedChannel iterates through all channels of a given node,
// executing the passed callback on the directed edge representing the channel
// and its incoming policy. If the callback returns an error, then the iteration
// is halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (c *KVStore) ForEachNodeDirectedChannel(nodePub route.Vertex,
	cb func(channel *DirectedChannel) error, reset func()) error {

	return c.forEachNodeDirectedChannel(nil, nodePub, cb, reset)
}

// FetchNodeFeatures returns the features of the given node. If no features are
// known for the node, an empty feature vector is returned.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (c *KVStore) FetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	return c.fetchNodeFeatures(nil, nodePub)
}

// ForEachNodeCached is similar to forEachNode, but it returns DirectedChannel
// data to the call-back.
//
// NOTE: The callback contents MUST not be modified.
func (c *KVStore) ForEachNodeCached(ctx context.Context, withAddrs bool,
	cb func(ctx context.Context, node route.Vertex, addrs []net.Addr,
		chans map[uint64]*DirectedChannel) error, reset func()) error {

	// Otherwise call back to a version that uses the database directly.
	// We'll iterate over each node, then the set of channels for each
	// node, and construct a similar callback functiopn signature as the
	// main funcotin expects.
	return forEachNode(c.db, func(tx kvdb.RTx,
		node *models.Node) error {

		channels := make(map[uint64]*DirectedChannel)

		err := c.forEachNodeChannelTx(tx, node.PubKeyBytes,
			func(tx kvdb.RTx, e *models.ChannelEdgeInfo,
				p1 *models.ChannelEdgePolicy,
				p2 *models.ChannelEdgePolicy) error {

				toNodeCallback := func() route.Vertex {
					return node.PubKeyBytes
				}
				toNodeFeatures, err := c.fetchNodeFeatures(
					tx, node.PubKeyBytes,
				)
				if err != nil {
					return err
				}

				var cachedInPolicy *models.CachedEdgePolicy
				if p2 != nil {
					cachedInPolicy =
						models.NewCachedPolicy(p2)
					cachedInPolicy.ToNodePubKey =
						toNodeCallback
					cachedInPolicy.ToNodeFeatures =
						toNodeFeatures
				}

				directedChannel := &DirectedChannel{
					ChannelID: e.ChannelID,
					IsNode1: node.PubKeyBytes ==
						e.NodeKey1Bytes,
					OtherNode:    e.NodeKey2Bytes,
					Capacity:     e.Capacity,
					OutPolicySet: p1 != nil,
					InPolicy:     cachedInPolicy,
				}

				if node.PubKeyBytes == e.NodeKey2Bytes {
					directedChannel.OtherNode =
						e.NodeKey1Bytes
				}

				channels[e.ChannelID] = directedChannel

				return nil
			}, reset,
		)
		if err != nil {
			return err
		}

		var addrs []net.Addr
		if withAddrs {
			addrs = node.Addresses
		}

		return cb(ctx, node.PubKeyBytes, addrs, channels)
	}, reset)
}

// DisabledChannelIDs returns the channel ids of disabled channels.
// A channel is disabled when two of the associated ChanelEdgePolicies
// have their disabled bit on.
func (c *KVStore) DisabledChannelIDs() ([]uint64, error) {
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

		// We iterate over all disabled policies and we add each channel
		// that has more than one disabled policy to disabledChanIDs
		// array.
		return disabledEdgePolicyIndex.ForEach(
			func(k, v []byte) error {
				chanID := byteOrder.Uint64(k[:8])
				_, edgeFound := chanEdgeFound[chanID]
				if edgeFound {
					delete(chanEdgeFound, chanID)
					disabledChanIDs = append(
						disabledChanIDs, chanID,
					)

					return nil
				}

				chanEdgeFound[chanID] = struct{}{}

				return nil
			},
		)
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
// NOTE: this is part of the V1Store interface.
func (c *KVStore) ForEachNode(_ context.Context,
	cb func(*models.Node) error, reset func()) error {

	return forEachNode(c.db, func(tx kvdb.RTx,
		node *models.Node) error {

		return cb(node)
	}, reset)
}

// forEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early.
//
// TODO(roasbeef): add iterator interface to allow for memory efficient graph
// traversal when graph gets mega.
func forEachNode(db kvdb.Backend,
	cb func(kvdb.RTx, *models.Node) error, reset func()) error {

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

			// Execute the callback, the transaction will abort if
			// this returns an error.
			return cb(tx, node)
		})
	}

	return kvdb.View(db, traversal, reset)
}

// ForEachNodeCacheable iterates through all the stored vertices/nodes in the
// graph, executing the passed callback with each node encountered. If the
// callback returns an error, then the transaction is aborted and the iteration
// stops early.
func (c *KVStore) ForEachNodeCacheable(_ context.Context,
	cb func(route.Vertex, *lnwire.FeatureVector) error,
	reset func()) error {

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
			node, features, err := deserializeLightningNodeCacheable( //nolint:ll
				nodeReader,
			)
			if err != nil {
				return err
			}

			// Execute the callback, the transaction will abort if
			// this returns an error.
			return cb(node, features)
		})
	}

	return kvdb.View(c.db, traversal, reset)
}

// SourceNode returns the source node of the graph. The source node is treated
// as the center node within a star-graph. This method may be used to kick off
// a path finding algorithm in order to explore the reachability of another
// node based off the source node.
func (c *KVStore) SourceNode(_ context.Context) (*models.Node, error) {
	return sourceNode(c.db)
}

// sourceNode fetches the source node of the graph. The source node is treated
// as the center node within a star-graph.
func sourceNode(db kvdb.Backend) (*models.Node, error) {
	var source *models.Node
	err := kvdb.View(db, func(tx kvdb.RTx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		node, err := sourceNodeWithTx(nodes)
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

// sourceNodeWithTx uses an existing database transaction and returns the source
// node of the graph. The source node is treated as the center node within a
// star-graph. This method may be used to kick off a path finding algorithm in
// order to explore the reachability of another node based off the source node.
func sourceNodeWithTx(nodes kvdb.RBucket) (*models.Node, error) {
	selfPub := nodes.Get(sourceKey)
	if selfPub == nil {
		return nil, ErrSourceNodeNotSet
	}

	// With the pubKey of the source node retrieved, we're able to
	// fetch the full node information.
	return fetchLightningNode(nodes, selfPub)
}

// SetSourceNode sets the source node within the graph database. The source
// node is to be used as the center of a star-graph within path finding
// algorithms.
func (c *KVStore) SetSourceNode(_ context.Context,
	node *models.Node) error {

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

// AddNode adds a vertex/node to the graph database. If the node is not
// in the database from before, this will add a new, unconnected one to the
// graph. If it is present from before, this will update that node's
// information. Note that this method is expected to only be called to update an
// already present node from a node announcement, or to insert a node found in a
// channel update.
//
// TODO(roasbeef): also need sig of announcement.
func (c *KVStore) AddNode(ctx context.Context,
	node *models.Node, opts ...batch.SchedulerOption) error {

	r := &batch.Request[kvdb.RwTx]{
		Opts: batch.NewSchedulerOptions(opts...),
		Do: func(tx kvdb.RwTx) error {
			return addLightningNode(tx, node)
		},
	}

	return c.nodeScheduler.Execute(ctx, r)
}

func addLightningNode(tx kvdb.RwTx, node *models.Node) error {
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
func (c *KVStore) LookupAlias(_ context.Context,
	pub *btcec.PublicKey) (string, error) {

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

// DeleteNode starts a new database transaction to remove a vertex/node
// from the database according to the node's public key.
func (c *KVStore) DeleteNode(_ context.Context,
	nodePub route.Vertex) error {

	// TODO(roasbeef): ensure dangling edges are removed...
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		nodes := tx.ReadWriteBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodeNotFound
		}

		return c.deleteLightningNode(nodes, nodePub[:])
	}, func() {})
}

// deleteLightningNode uses an existing database transaction to remove a
// vertex/node from the database according to the node's public key.
func (c *KVStore) deleteLightningNode(nodes kvdb.RwBucket,
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
func (c *KVStore) AddChannelEdge(ctx context.Context,
	edge *models.ChannelEdgeInfo, opts ...batch.SchedulerOption) error {

	var alreadyExists bool
	r := &batch.Request[kvdb.RwTx]{
		Opts: batch.NewSchedulerOptions(opts...),
		Reset: func() {
			alreadyExists = false
		},
		Do: func(tx kvdb.RwTx) error {
			err := c.addChannelEdge(tx, edge)

			// Silence ErrEdgeAlreadyExist so that the batch can
			// succeed, but propagate the error via local state.
			if errors.Is(err, ErrEdgeAlreadyExist) {
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

	return c.chanScheduler.Execute(ctx, r)
}

// addChannelEdge is the private form of AddChannelEdge that allows callers to
// utilize an existing db transaction.
func (c *KVStore) addChannelEdge(tx kvdb.RwTx,
	edge *models.ChannelEdgeInfo) error {

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

	// Before we insert the channel into the database, we'll ensure that
	// both nodes already exist in the channel graph. If either node
	// doesn't, then we'll insert a "shell" node that just includes its
	// public key, so subsequent validation and queries can work properly.
	_, node1Err := fetchLightningNode(nodes, edge.NodeKey1Bytes[:])
	switch {
	case errors.Is(node1Err, ErrGraphNodeNotFound):
		err := addLightningNode(
			tx, models.NewV1ShellNode(edge.NodeKey1Bytes),
		)
		if err != nil {
			return fmt.Errorf("unable to create shell node "+
				"for: %x: %w", edge.NodeKey1Bytes, err)
		}
	case node1Err != nil:
		return node1Err
	}

	_, node2Err := fetchLightningNode(nodes, edge.NodeKey2Bytes[:])
	switch {
	case errors.Is(node2Err, ErrGraphNodeNotFound):
		err := addLightningNode(
			tx, models.NewV1ShellNode(edge.NodeKey2Bytes),
		)
		if err != nil {
			return fmt.Errorf("unable to create shell node "+
				"for: %x: %w", edge.NodeKey2Bytes, err)
		}
	case node2Err != nil:
		return node2Err
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
	if err := WriteOutpoint(&b, &edge.ChannelPoint); err != nil {
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
func (c *KVStore) HasChannelEdge(
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

		e1, e2, err := fetchChanEdgePolicies(
			edgeIndex, edges, channelID[:],
		)
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

// AddEdgeProof sets the proof of an existing edge in the graph database.
func (c *KVStore) AddEdgeProof(chanID lnwire.ShortChannelID,
	proof *models.ChannelAuthProof) error {

	// Construct the channel's primary key which is the 8-byte channel ID.
	var chanKey [8]byte
	binary.BigEndian.PutUint64(chanKey[:], chanID.ToUint64())

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		if edges == nil {
			return ErrEdgeNotFound
		}

		edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrEdgeNotFound
		}

		edge, err := fetchChanEdgeInfo(edgeIndex, chanKey[:])
		if err != nil {
			return err
		}

		edge.AuthProof = proof

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
// the target block along with any pruned nodes are returned if the function
// succeeds without error.
func (c *KVStore) PruneGraph(spentOutputs []*wire.OutPoint,
	blockHash *chainhash.Hash, blockHeight uint32) (
	[]*models.ChannelEdgeInfo, []route.Vertex, error) {

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	var (
		chansClosed []*models.ChannelEdgeInfo
		prunedNodes []route.Vertex
	)

	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		// First grab the edges bucket which houses the information
		// we'd like to delete
		edges, err := tx.CreateTopLevelBucket(edgeBucket)
		if err != nil {
			return err
		}

		// Next grab the two edge indexes which will also need to be
		// updated.
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(
			channelPointBucket,
		)
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
			err := WriteOutpoint(&opBytes, chanPoint)
			if err != nil {
				return err
			}

			// First attempt to see if the channel exists within
			// the database, if not, then we can exit early.
			chanID := chanIndex.Get(opBytes.Bytes())
			if chanID == nil {
				continue
			}

			// Attempt to delete the channel, an ErrEdgeNotFound
			// will be returned if that outpoint isn't known to be
			// a channel. If no error is returned, then a channel
			// was successfully pruned.
			edgeInfo, err := c.delChannelEdgeUnsafe(
				edges, edgeIndex, chanIndex, zombieIndex,
				chanID, false, false,
			)
			if err != nil && !errors.Is(err, ErrEdgeNotFound) {
				return err
			}

			chansClosed = append(chansClosed, edgeInfo)
		}

		metaBucket, err := tx.CreateTopLevelBucket(graphMetaBucket)
		if err != nil {
			return err
		}

		pruneBucket, err := metaBucket.CreateBucketIfNotExists(
			pruneLogBucket,
		)
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
		prunedNodes, err = c.pruneGraphNodes(nodes, edgeIndex)

		return err
	}, func() {
		chansClosed = nil
		prunedNodes = nil
	})
	if err != nil {
		return nil, nil, err
	}

	for _, channel := range chansClosed {
		c.rejectCache.remove(channel.ChannelID)
		c.chanCache.remove(channel.ChannelID)
	}

	return chansClosed, prunedNodes, nil
}

// PruneGraphNodes is a garbage collection method which attempts to prune out
// any nodes from the channel graph that are currently unconnected. This ensure
// that we only maintain a graph of reachable nodes. In the event that a pruned
// node gains more channels, it will be re-added back to the graph.
func (c *KVStore) PruneGraphNodes() ([]route.Vertex, error) {
	var prunedNodes []route.Vertex
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
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

		var err error
		prunedNodes, err = c.pruneGraphNodes(nodes, edgeIndex)
		if err != nil {
			return err
		}

		return nil
	}, func() {
		prunedNodes = nil
	})

	return prunedNodes, err
}

// pruneGraphNodes attempts to remove any nodes from the graph who have had a
// channel closed within the current block. If the node still has existing
// channels in the graph, this will act as a no-op.
func (c *KVStore) pruneGraphNodes(nodes kvdb.RwBucket,
	edgeIndex kvdb.RwBucket) ([]route.Vertex, error) {

	log.Trace("Pruning nodes from graph with no open channels")

	// We'll retrieve the graph's source node to ensure we don't remove it
	// even if it no longer has any open channels.
	sourceNode, err := sourceNodeWithTx(nodes)
	if err != nil {
		return nil, err
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
		return nil, err
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
		return nil, err
	}

	// Finally, we'll make a second pass over the set of nodes, and delete
	// any nodes that have a ref count of zero.
	var pruned []route.Vertex
	for nodePubKey, refCount := range nodeRefCounts {
		// If the ref count of the node isn't zero, then we can safely
		// skip it as it still has edges to or from it within the
		// graph.
		if refCount != 0 {
			continue
		}

		// If we reach this point, then there are no longer any edges
		// that connect this node, so we can delete it.
		err := c.deleteLightningNode(nodes, nodePubKey[:])
		if err != nil {
			if errors.Is(err, ErrGraphNodeNotFound) ||
				errors.Is(err, ErrGraphNodesNotFound) {

				log.Warnf("Unable to prune node %x from the "+
					"graph: %v", nodePubKey, err)
				continue
			}

			return nil, err
		}

		log.Infof("Pruned unconnected node %x from channel graph",
			nodePubKey[:])

		pruned = append(pruned, nodePubKey)
	}

	if len(pruned) > 0 {
		log.Infof("Pruned %v unconnected nodes from the channel graph",
			len(pruned))
	}

	return pruned, err
}

// DisconnectBlockAtHeight is used to indicate that the block specified
// by the passed height has been disconnected from the main chain. This
// will "rewind" the graph back to the height below, deleting channels
// that are no longer confirmed from the graph. The prune log will be
// set to the last prune height valid for the remaining chain.
// Channels that were removed from the graph resulting from the
// disconnected block are returned.
func (c *KVStore) DisconnectBlockAtHeight(height uint32) (
	[]*models.ChannelEdgeInfo, error) {

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
	var removedChans []*models.ChannelEdgeInfo

	if err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		edges, err := tx.CreateTopLevelBucket(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}
		chanIndex, err := edges.CreateBucketIfNotExists(
			channelPointBucket,
		)
		if err != nil {
			return err
		}
		zombieIndex, err := edges.CreateBucketIfNotExists(zombieBucket)
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

		//nolint:ll
		for k, _ := cursor.Seek(chanIDStart[:]); k != nil &&
			bytes.Compare(k, chanIDEnd[:]) < 0; k, _ = cursor.Next() {
			keys = append(keys, k)
		}

		for _, k := range keys {
			edgeInfo, err := c.delChannelEdgeUnsafe(
				edges, edgeIndex, chanIndex, zombieIndex,
				k, false, false,
			)
			if err != nil && !errors.Is(err, ErrEdgeNotFound) {
				return err
			}

			removedChans = append(removedChans, edgeInfo)
		}

		// Delete all the entries in the prune log having a height
		// greater or equal to the block disconnected.
		metaBucket, err := tx.CreateTopLevelBucket(graphMetaBucket)
		if err != nil {
			return err
		}

		pruneBucket, err := metaBucket.CreateBucketIfNotExists(
			pruneLogBucket,
		)
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
		//nolint:ll
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
func (c *KVStore) PruneTip() (*chainhash.Hash, uint32, error) {
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
		copy(tipHash[:], v)
		tipHeight = byteOrder.Uint32(k)

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
func (c *KVStore) DeleteChannelEdges(strictZombiePruning, markZombie bool,
	chanIDs ...uint64) ([]*models.ChannelEdgeInfo, error) {

	// TODO(roasbeef): possibly delete from node bucket if node has no more
	// channels
	// TODO(roasbeef): don't delete both edges?

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	var infos []*models.ChannelEdgeInfo
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
			edgeInfo, err := c.delChannelEdgeUnsafe(
				edges, edgeIndex, chanIndex, zombieIndex,
				rawChanID[:], markZombie, strictZombiePruning,
			)
			if err != nil {
				return err
			}

			infos = append(infos, edgeInfo)
		}

		return nil
	}, func() {
		infos = nil
	})
	if err != nil {
		return nil, err
	}

	for _, chanID := range chanIDs {
		c.rejectCache.remove(chanID)
		c.chanCache.remove(chanID)
	}

	return infos, nil
}

// ChannelID attempt to lookup the 8-byte compact channel ID which maps to the
// passed channel point (outpoint). If the passed channel doesn't exist within
// the database, then ErrEdgeNotFound is returned.
func (c *KVStore) ChannelID(chanPoint *wire.OutPoint) (uint64, error) {
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
	if err := WriteOutpoint(&b, chanPoint); err != nil {
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
func (c *KVStore) HighestChanID(_ context.Context) (uint64, error) {
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
	if err != nil && !errors.Is(err, ErrGraphNoEdgesFound) {
		return 0, err
	}

	return cid, nil
}

// ChannelEdge represents the complete set of information for a channel edge in
// the known channel graph. This struct couples the core information of the
// edge as well as each of the known advertised edge policies.
type ChannelEdge struct {
	// Info contains all the static information describing the channel.
	Info *models.ChannelEdgeInfo

	// Policy1 points to the "first" edge policy of the channel containing
	// the dynamic information required to properly route through the edge.
	Policy1 *models.ChannelEdgePolicy

	// Policy2 points to the "second" edge policy of the channel containing
	// the dynamic information required to properly route through the edge.
	Policy2 *models.ChannelEdgePolicy

	// Node1 is "node 1" in the channel. This is the node that would have
	// produced Policy1 if it exists.
	Node1 *models.Node

	// Node2 is "node 2" in the channel. This is the node that would have
	// produced Policy2 if it exists.
	Node2 *models.Node
}

// updateChanCacheBatch updates the channel cache with multiple edges at once.
// This method acquires the cache lock only once for the entire batch.
func (c *KVStore) updateChanCacheBatch(edgesToCache map[uint64]ChannelEdge) {
	if len(edgesToCache) == 0 {
		return
	}

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	for cid, edge := range edgesToCache {
		c.chanCache.insert(cid, edge)
	}
}

// isEmptyGraphError returns true if the error indicates the graph database
// is empty (no edges or nodes exist). These errors are expected when the
// graph is first created or has no data.
func isEmptyGraphError(err error) bool {
	return errors.Is(err, ErrGraphNoEdgesFound) ||
		errors.Is(err, ErrGraphNodesNotFound)
}

// chanUpdatesIterator holds the state for chunked channel update iteration.
type chanUpdatesIterator struct {
	// batchSize is the amount of channel updates to read at a single time.
	batchSize int

	// startTime is the start time of the iteration request.
	startTime time.Time

	// endTime is the end time of the iteration request.
	endTime time.Time

	// edgesSeen is used to dedup edges.
	edgesSeen map[uint64]struct{}

	// edgesToCache houses all the edges that we read from the disk which
	// aren't yet cached. This is used to update the cache after a batch
	// chunk.
	edgesToCache map[uint64]ChannelEdge

	// lastSeenKey is the last index key seen. This is used to resume
	// iteration.
	lastSeenKey []byte

	// hits is the number of cache hits.
	hits int

	// total is the total number of edges requested.
	total int
}

// newChanUpdatesIterator makes a new chan updates iterator.
func newChanUpdatesIterator(batchSize int,
	startTime, endTime time.Time) *chanUpdatesIterator {

	return &chanUpdatesIterator{
		batchSize:    batchSize,
		startTime:    startTime,
		endTime:      endTime,
		edgesSeen:    make(map[uint64]struct{}),
		edgesToCache: make(map[uint64]ChannelEdge),
		lastSeenKey:  nil,
	}
}

// fetchNextChanUpdateBatch retrieves the next batch of channel edges within the
// horizon. Returns the batch, whether there are more edges, and any error.
func (c *KVStore) fetchNextChanUpdateBatch(
	state *chanUpdatesIterator) ([]ChannelEdge, bool, error) {

	var (
		batch   []ChannelEdge
		hasMore bool
	)

	// Acquire read lock before starting transaction to ensure
	// consistent lock ordering (cacheMu -> DB) and prevent
	// deadlock with write operations.
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()

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

		// With all the relevant buckets read, we'll now create a fresh
		// read cursor.
		updateCursor := edgeUpdateIndex.ReadCursor()

		// We'll now use the start and end time to create the keys that
		// we'll use to seek.
		var startTimeBytes, endTimeBytes [8 + 8]byte
		byteOrder.PutUint64(
			startTimeBytes[:8], uint64(state.startTime.Unix()),
		)
		byteOrder.PutUint64(
			endTimeBytes[:8], uint64(state.endTime.Unix()),
		)

		var indexKey []byte

		// If we left off earlier, then we'll use that key as the
		// starting point.
		switch {
		case state.lastSeenKey != nil:
			// Seek to the last seen key, moving to the key right
			// after it.
			indexKey, _ = updateCursor.Seek(state.lastSeenKey)

			if bytes.Equal(indexKey, state.lastSeenKey) {
				indexKey, _ = updateCursor.Next()
			}

		// Otherwise, we'll move to the very start of the time range.
		default:
			indexKey, _ = updateCursor.Seek(startTimeBytes[:])
		}

		// TODO(roasbeef): iterate the channel graph cache instead w/ a
		// treap ordering?

		// Now we'll read items up to the batch size, exiting early if
		// we exceed the ending time.
		for len(batch) < state.batchSize && indexKey != nil {
			// If we're at the end, then we'll break out now.
			if bytes.Compare(indexKey, endTimeBytes[:]) > 0 {
				break
			}

			chanID := indexKey[8:]
			chanIDInt := byteOrder.Uint64(chanID)

			if state.lastSeenKey == nil {
				state.lastSeenKey = make([]byte, len(indexKey))
			}
			copy(state.lastSeenKey, indexKey)

			// If we've seen this channel ID already, then we'll
			// skip it.
			if _, ok := state.edgesSeen[chanIDInt]; ok {
				indexKey, _ = updateCursor.Next()
				continue
			}

			// Check cache (we already hold shared read lock).
			if channel, ok := c.chanCache.get(chanIDInt); ok {
				state.edgesSeen[chanIDInt] = struct{}{}

				batch = append(batch, channel)

				state.hits++
				state.total++

				indexKey, _ = updateCursor.Next()

				continue
			}

			// The edge wasn't in the cache, so we'll fetch it along
			// w/ the edge policies and nodes.
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				return fmt.Errorf("unable to fetch info "+
					"for edge with chan_id=%v: %v",
					chanIDInt, err)
			}
			edge1, edge2, err := fetchChanEdgePolicies(
				edgeIndex, edges, chanID,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch "+
					"policies for edge with chan_id=%v: %v",
					chanIDInt, err)
			}
			node1, err := fetchLightningNode(
				nodes, edgeInfo.NodeKey1Bytes[:],
			)
			if err != nil {
				return err
			}
			node2, err := fetchLightningNode(
				nodes, edgeInfo.NodeKey2Bytes[:],
			)
			if err != nil {
				return err
			}

			// Now we have all the information we need to build the
			// channel edge.
			channel := ChannelEdge{
				Info:    edgeInfo,
				Policy1: edge1,
				Policy2: edge2,
				Node1:   node1,
				Node2:   node2,
			}

			state.edgesSeen[chanIDInt] = struct{}{}
			state.edgesToCache[chanIDInt] = channel

			batch = append(batch, channel)

			state.total++

			// Advance the iterator to the next entry.
			indexKey, _ = updateCursor.Next()
		}

		// If we haven't yet crossed the endTimeBytes, then we still
		// have more entries to deliver.
		if indexKey != nil &&
			bytes.Compare(indexKey, endTimeBytes[:]) <= 0 {

			hasMore = true
		}

		return nil
	}, func() {
		batch = nil
		hasMore = false
	})
	if err != nil {
		return nil, false, err
	}

	return batch, hasMore, nil
}

// ChanUpdatesInHorizon returns all the known channel edges which have at least
// one edge that has an update timestamp within the specified horizon.
func (c *KVStore) ChanUpdatesInHorizon(startTime, endTime time.Time,
	opts ...IteratorOption) iter.Seq2[ChannelEdge, error] {

	cfg := defaultIteratorConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	return func(yield func(ChannelEdge, error) bool) {
		iterState := newChanUpdatesIterator(
			cfg.chanUpdateIterBatchSize, startTime, endTime,
		)

		for {
			// At the top of the loop, we'll read the next batch
			// chunk from disk. We'll also determine if we have any
			// more entries after this or not.
			batch, hasMore, err := c.fetchNextChanUpdateBatch(
				iterState,
			)
			if err != nil {
				// These errors just mean the graph is empty,
				// which is OK.
				if !isEmptyGraphError(err) {
					log.Errorf("ChanUpdatesInHorizon "+
						"batch error: %v", err)

					yield(ChannelEdge{}, err)

					return
				}
				// Continue with empty batch
			}

			// We'll now yield each edge that we just read. If yield
			// returns false, then that means that we'll exit early.
			for _, edge := range batch {
				if !yield(edge, nil) {
					return
				}
			}

			// Update cache after successful batch yield.
			c.updateChanCacheBatch(iterState.edgesToCache)
			iterState.edgesToCache = make(map[uint64]ChannelEdge)

			// If we we're done, then we can just break out here
			// now.
			if !hasMore || len(batch) == 0 {
				break
			}
		}

		if iterState.total > 0 {
			log.Tracef("ChanUpdatesInHorizon hit percentage: "+
				"%.2f (%d/%d)", float64(iterState.hits)*100/
				float64(iterState.total), iterState.hits,
				iterState.total)
		} else {
			log.Tracef("ChanUpdatesInHorizon returned no edges "+
				"in horizon (%s, %s)", startTime, endTime)
		}
	}
}

// nodeUpdatesIterator maintains state for iterating through node updates.
//
// Iterator Lifecycle:
// 1. Initialize state with start/end time, batch size, and filtering options.
// 2. Fetch batch using pagination cursor (lastSeenKey).
// 3. Filter nodes if publicNodesOnly is set.
// 4. Update lastSeenKey to the last processed node's index key.
// 5. Repeat until we exceed endTime or no more nodes exist.
type nodeUpdatesIterator struct {
	// batchSize is the amount of node updates to read at a single time.
	batchSize int

	// startTime is the start time of the iteration request.
	startTime time.Time

	// endTime is the end time of the iteration request.
	endTime time.Time

	// lastSeenKey is the last index key seen. This is used to resume
	// iteration.
	lastSeenKey []byte

	// publicNodesOnly filters to only return public nodes if true.
	publicNodesOnly bool

	// total tracks total nodes processed.
	total int
}

// newNodeUpdatesIterator makes a new node updates iterator.
func newNodeUpdatesIterator(batchSize int, startTime, endTime time.Time,
	publicNodesOnly bool) *nodeUpdatesIterator {

	return &nodeUpdatesIterator{
		batchSize:       batchSize,
		startTime:       startTime,
		endTime:         endTime,
		lastSeenKey:     nil,
		publicNodesOnly: publicNodesOnly,
	}
}

// fetchNextNodeBatch fetches the next batch of node announcements using the
// iterator state.
func (c *KVStore) fetchNextNodeBatch(
	state *nodeUpdatesIterator) ([]*models.Node, bool, error) {

	var (
		nodeBatch []*models.Node
		hasMore   bool
	)

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}
		ourPubKey := nodes.Get(sourceKey)
		if ourPubKey == nil && state.publicNodesOnly {
			// If we're filtering for public nodes only but don't
			// have a source node set, we can't determine if nodes
			// are public. A node is considered public if it has at
			// least one channel with our node (the source node).
			return ErrSourceNodeNotSet
		}
		nodeUpdateIndex := nodes.NestedReadBucket(nodeUpdateIndexBucket)
		if nodeUpdateIndex == nil {
			return ErrGraphNodesNotFound
		}

		// We'll now obtain a cursor to perform a range query within the
		// index to find all node announcements within the horizon. The
		// nodeUpdateIndex key format is: [8 bytes timestamp][33 bytes
		// node pubkey] This allows efficient range queries by time
		// while maintaining a stable sort order for nodes with the same
		// timestamp.
		updateCursor := nodeUpdateIndex.ReadCursor()

		var startTimeBytes, endTimeBytes [8 + 33]byte
		byteOrder.PutUint64(
			startTimeBytes[:8], uint64(state.startTime.Unix()),
		)
		byteOrder.PutUint64(
			endTimeBytes[:8], uint64(state.endTime.Unix()),
		)

		// If we have a last seen key (existing iteration), then that'll
		// be our starting point. Otherwise, we'll seek to the start
		// time.
		var indexKey []byte
		if state.lastSeenKey != nil {
			indexKey, _ = updateCursor.Seek(state.lastSeenKey)

			if bytes.Equal(indexKey, state.lastSeenKey) {
				indexKey, _ = updateCursor.Next()
			}
		} else {
			indexKey, _ = updateCursor.Seek(startTimeBytes[:])
		}

		// Now we'll read items up to the batch size, exiting early if
		// we exceed the ending time.
		var lastProcessedKey []byte
		for len(nodeBatch) < state.batchSize && indexKey != nil {
			// Extract the timestamp from the index key (first 8
			// bytes). Only compare timestamps, not the full key
			// with pubkey.
			keyTimestamp := byteOrder.Uint64(indexKey[:8])
			endTimestamp := uint64(state.endTime.Unix())
			if keyTimestamp > endTimestamp {
				break
			}

			nodePub := indexKey[8:]
			node, err := fetchLightningNode(nodes, nodePub)
			if err != nil {
				return err
			}

			if state.publicNodesOnly {
				nodeIsPublic, err := c.isPublic(
					tx, node.PubKeyBytes, ourPubKey,
				)
				if err != nil {
					return err
				}
				if !nodeIsPublic {
					indexKey, _ = updateCursor.Next()
					continue
				}
			}

			nodeBatch = append(nodeBatch, node)
			state.total++

			// Remember the last key we actually processed. We'll
			// use this to update the last seen key below.
			if lastProcessedKey == nil {
				lastProcessedKey = make([]byte, len(indexKey))
			}
			copy(lastProcessedKey, indexKey)

			// Advance the iterator to the next entry.
			indexKey, _ = updateCursor.Next()
		}

		// If we haven't yet crossed the endTime, then we still
		// have more entries to deliver.
		if indexKey != nil {
			keyTimestamp := byteOrder.Uint64(indexKey[:8])
			endTimestamp := uint64(state.endTime.Unix())
			if keyTimestamp <= endTimestamp {
				hasMore = true
			}
		}

		// Update the cursor to the last key we actually processed.
		if lastProcessedKey != nil {
			if state.lastSeenKey == nil {
				state.lastSeenKey = make(
					[]byte, len(lastProcessedKey),
				)
			}
			copy(state.lastSeenKey, lastProcessedKey)
		}

		return nil
	}, func() {
		nodeBatch = nil
	})
	switch {
	case errors.Is(err, ErrGraphNoEdgesFound):
		fallthrough
	case errors.Is(err, ErrGraphNodesNotFound):
		break

	case err != nil:
		return nil, false, err
	}

	return nodeBatch, hasMore, nil
}

// NodeUpdatesInHorizon returns all the known lightning node which have an
// update timestamp within the passed range.
func (c *KVStore) NodeUpdatesInHorizon(startTime,
	endTime time.Time,
	opts ...IteratorOption) iter.Seq2[*models.Node, error] {

	cfg := defaultIteratorConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	return func(yield func(*models.Node, error) bool) {
		// Initialize iterator state.
		state := newNodeUpdatesIterator(
			cfg.nodeUpdateIterBatchSize,
			startTime, endTime,
			cfg.iterPublicNodes,
		)

		for {
			nodeAnns, hasMore, err := c.fetchNextNodeBatch(state)
			if err != nil {
				log.Errorf("unable to read node updates in "+
					"horizon: %v", err)

				yield(&models.Node{}, err)

				return
			}

			for _, node := range nodeAnns {
				if !yield(node, nil) {
					return
				}
			}

			// If we we're done, then we can just break out here
			// now.
			if !hasMore || len(nodeAnns) == 0 {
				break
			}
		}
	}
}

// FilterKnownChanIDs takes a set of channel IDs and return the subset of chan
// ID's that we don't know and are not known zombies of the passed set. In other
// words, we perform a set difference of our set of chan ID's and the ones
// passed in. This method can be used by callers to determine the set of
// channels another peer knows of that we don't. The ChannelUpdateInfos for the
// known zombies is also returned.
func (c *KVStore) FilterKnownChanIDs(chansInfo []ChannelUpdateInfo) ([]uint64,
	[]ChannelUpdateInfo, error) {

	var (
		newChanIDs   []uint64
		knownZombies []ChannelUpdateInfo
	)

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

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
		for _, info := range chansInfo {
			scid := info.ShortChannelID.ToUint64()
			byteOrder.PutUint64(cidBytes[:], scid)

			// If the edge is already known, skip it.
			if v := edgeIndex.Get(cidBytes[:]); v != nil {
				continue
			}

			// If the edge is a known zombie, skip it.
			if zombieIndex != nil {
				isZombie, _, _ := isZombieEdge(
					zombieIndex, scid,
				)

				if isZombie {
					knownZombies = append(
						knownZombies, info,
					)

					continue
				}
			}

			newChanIDs = append(newChanIDs, scid)
		}

		return nil
	}, func() {
		newChanIDs = nil
		knownZombies = nil
	})
	switch {
	// If we don't know of any edges yet, then we'll return the entire set
	// of chan IDs specified.
	case errors.Is(err, ErrGraphNoEdgesFound):
		ogChanIDs := make([]uint64, len(chansInfo))
		for i, info := range chansInfo {
			ogChanIDs[i] = info.ShortChannelID.ToUint64()
		}

		return ogChanIDs, nil, nil

	case err != nil:
		return nil, nil, err
	}

	return newChanIDs, knownZombies, nil
}

// ChannelUpdateInfo couples the SCID of a channel with the timestamps of the
// latest received channel updates for the channel.
type ChannelUpdateInfo struct {
	// ShortChannelID is the SCID identifier of the channel.
	ShortChannelID lnwire.ShortChannelID

	// Node1UpdateTimestamp is the timestamp of the latest received update
	// from the node 1 channel peer. This will be set to zero time if no
	// update has yet been received from this node.
	Node1UpdateTimestamp time.Time

	// Node2UpdateTimestamp is the timestamp of the latest received update
	// from the node 2 channel peer. This will be set to zero time if no
	// update has yet been received from this node.
	Node2UpdateTimestamp time.Time
}

// NewChannelUpdateInfo is a constructor which makes sure we initialize the
// timestamps with zero seconds unix timestamp which equals
// `January 1, 1970, 00:00:00 UTC` in case the value is `time.Time{}`.
func NewChannelUpdateInfo(scid lnwire.ShortChannelID, node1Timestamp,
	node2Timestamp time.Time) ChannelUpdateInfo {

	chanInfo := ChannelUpdateInfo{
		ShortChannelID:       scid,
		Node1UpdateTimestamp: node1Timestamp,
		Node2UpdateTimestamp: node2Timestamp,
	}

	if node1Timestamp.IsZero() {
		chanInfo.Node1UpdateTimestamp = time.Unix(0, 0)
	}

	if node2Timestamp.IsZero() {
		chanInfo.Node2UpdateTimestamp = time.Unix(0, 0)
	}

	return chanInfo
}

// BlockChannelRange represents a range of channels for a given block height.
type BlockChannelRange struct {
	// Height is the height of the block all of the channels below were
	// included in.
	Height uint32

	// Channels is the list of channels identified by their short ID
	// representation known to us that were included in the block height
	// above. The list may include channel update timestamp information if
	// requested.
	Channels []ChannelUpdateInfo
}

// FilterChannelRange returns the channel ID's of all known channels which were
// mined in a block height within the passed range. The channel IDs are grouped
// by their common block height. This method can be used to quickly share with a
// peer the set of channels we know of within a particular range to catch them
// up after a period of time offline. If withTimestamps is true then the
// timestamp info of the latest received channel update messages of the channel
// will be included in the response.
func (c *KVStore) FilterChannelRange(startHeight,
	endHeight uint32, withTimestamps bool) ([]BlockChannelRange, error) {

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

	var channelsPerBlock map[uint32][]ChannelUpdateInfo
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
		//
		//nolint:ll
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

			chanInfo := NewChannelUpdateInfo(
				cid, time.Time{}, time.Time{},
			)

			if !withTimestamps {
				channelsPerBlock[cid.BlockHeight] = append(
					channelsPerBlock[cid.BlockHeight],
					chanInfo,
				)

				continue
			}

			node1Key, node2Key := computeEdgePolicyKeys(edgeInfo)

			rawPolicy := edges.Get(node1Key)
			if len(rawPolicy) != 0 {
				r := bytes.NewReader(rawPolicy)

				edge, err := deserializeChanEdgePolicyRaw(r)
				if err != nil && !errors.Is(
					err, ErrEdgePolicyOptionalFieldNotFound,
				) && !errors.Is(err, ErrParsingExtraTLVBytes) {

					return err
				}

				chanInfo.Node1UpdateTimestamp = edge.LastUpdate
			}

			rawPolicy = edges.Get(node2Key)
			if len(rawPolicy) != 0 {
				r := bytes.NewReader(rawPolicy)

				edge, err := deserializeChanEdgePolicyRaw(r)
				if err != nil && !errors.Is(
					err, ErrEdgePolicyOptionalFieldNotFound,
				) && !errors.Is(err, ErrParsingExtraTLVBytes) {

					return err
				}

				chanInfo.Node2UpdateTimestamp = edge.LastUpdate
			}

			channelsPerBlock[cid.BlockHeight] = append(
				channelsPerBlock[cid.BlockHeight], chanInfo,
			)
		}

		return nil
	}, func() {
		channelsPerBlock = make(map[uint32][]ChannelUpdateInfo)
	})

	switch {
	// If we don't know of any channels yet, then there's nothing to
	// filter, so we'll return an empty slice.
	case errors.Is(err, ErrGraphNoEdgesFound) || len(channelsPerBlock) == 0:
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
func (c *KVStore) FetchChanInfos(chanIDs []uint64) ([]ChannelEdge, error) {
	return c.fetchChanInfos(nil, chanIDs)
}

// fetchChanInfos returns the set of channel edges that correspond to the passed
// channel ID's. If an edge is the query is unknown to the database, it will
// skipped and the result will contain only those edges that exist at the time
// of the query. This can be used to respond to peer queries that are seeking to
// fill in gaps in their view of the channel graph.
//
// NOTE: An optional transaction may be provided. If none is provided, then a
// new one will be created.
func (c *KVStore) fetchChanInfos(tx kvdb.RTx, chanIDs []uint64) (
	[]ChannelEdge, error) {
	// TODO(roasbeef): sort cids?

	var (
		chanEdges []ChannelEdge
		cidBytes  [8]byte
	)

	fetchChanInfos := func(tx kvdb.RTx) error {
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
			case errors.Is(err, ErrEdgeNotFound):
				continue
			case err != nil:
				return err
			}

			// With the static information obtained, we'll now
			// fetch the dynamic policy info.
			edge1, edge2, err := fetchChanEdgePolicies(
				edgeIndex, edges, cidBytes[:],
			)
			if err != nil {
				return err
			}

			node1, err := fetchLightningNode(
				nodes, edgeInfo.NodeKey1Bytes[:],
			)
			if err != nil {
				return err
			}

			node2, err := fetchLightningNode(
				nodes, edgeInfo.NodeKey2Bytes[:],
			)
			if err != nil {
				return err
			}

			chanEdges = append(chanEdges, ChannelEdge{
				Info:    edgeInfo,
				Policy1: edge1,
				Policy2: edge2,
				Node1:   node1,
				Node2:   node2,
			})
		}

		return nil
	}

	if tx == nil {
		err := kvdb.View(c.db, fetchChanInfos, func() {
			chanEdges = nil
		})
		if err != nil {
			return nil, err
		}

		return chanEdges, nil
	}

	err := fetchChanInfos(tx)
	if err != nil {
		return nil, err
	}

	return chanEdges, nil
}

func delEdgeUpdateIndexEntry(edgesBucket kvdb.RwBucket, chanID uint64,
	edge1, edge2 *models.ChannelEdgePolicy) error {

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
		byteOrder.PutUint64(
			indexKey[:8], uint64(edge1.LastUpdate.Unix()),
		)
		if err := updateIndex.Delete(indexKey[:]); err != nil {
			return err
		}
	}

	// We'll also attempt to delete the entry that may have been created by
	// the second edge.
	if edge2 != nil {
		byteOrder.PutUint64(
			indexKey[:8], uint64(edge2.LastUpdate.Unix()),
		)
		if err := updateIndex.Delete(indexKey[:]); err != nil {
			return err
		}
	}

	return nil
}

// delChannelEdgeUnsafe deletes the edge with the given chanID from the graph
// cache. It then goes on to delete any policy info and edge info for this
// channel from the DB and finally, if isZombie is true, it will add an entry
// for this channel in the zombie index.
//
// NOTE: this method MUST only be called if the cacheMu has already been
// acquired.
func (c *KVStore) delChannelEdgeUnsafe(edges, edgeIndex, chanIndex,
	zombieIndex kvdb.RwBucket, chanID []byte, isZombie,
	strictZombie bool) (*models.ChannelEdgeInfo, error) {

	edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
	if err != nil {
		return nil, err
	}

	// We'll also remove the entry in the edge update index bucket before
	// we delete the edges themselves so we can access their last update
	// times.
	cid := byteOrder.Uint64(chanID)
	edge1, edge2, err := fetchChanEdgePolicies(edgeIndex, edges, chanID)
	if err != nil {
		return nil, err
	}
	err = delEdgeUpdateIndexEntry(edges, cid, edge1, edge2)
	if err != nil {
		return nil, err
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
			return nil, err
		}
	}
	copy(edgeKey[:33], edgeInfo.NodeKey2Bytes[:])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return nil, err
		}
	}

	// As part of deleting the edge we also remove all disabled entries
	// from the edgePolicyDisabledIndex bucket. We do that for both
	// directions.
	err = updateEdgePolicyDisabledIndex(edges, cid, false, false)
	if err != nil {
		return nil, err
	}
	err = updateEdgePolicyDisabledIndex(edges, cid, true, false)
	if err != nil {
		return nil, err
	}

	// With the edge data deleted, we can purge the information from the two
	// edge indexes.
	if err := edgeIndex.Delete(chanID); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := WriteOutpoint(&b, &edgeInfo.ChannelPoint); err != nil {
		return nil, err
	}
	if err := chanIndex.Delete(b.Bytes()); err != nil {
		return nil, err
	}

	// Finally, we'll mark the edge as a zombie within our index if it's
	// being removed due to the channel becoming a zombie. We do this to
	// ensure we don't store unnecessary data for spent channels.
	if !isZombie {
		return edgeInfo, nil
	}

	nodeKey1, nodeKey2 := edgeInfo.NodeKey1Bytes, edgeInfo.NodeKey2Bytes
	if strictZombie {
		var e1UpdateTime, e2UpdateTime *time.Time
		if edge1 != nil {
			e1UpdateTime = &edge1.LastUpdate
		}
		if edge2 != nil {
			e2UpdateTime = &edge2.LastUpdate
		}

		nodeKey1, nodeKey2 = makeZombiePubkeys(
			edgeInfo.NodeKey1Bytes, edgeInfo.NodeKey2Bytes,
			e1UpdateTime, e2UpdateTime,
		)
	}

	return edgeInfo, markEdgeZombie(
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
func makeZombiePubkeys(node1, node2 [33]byte, e1, e2 *time.Time) ([33]byte,
	[33]byte) {

	switch {
	// If we don't have either edge policy, we'll return both pubkeys so
	// that the channel can be resurrected by either party.
	case e1 == nil && e2 == nil:
		return node1, node2

	// If we're missing edge1, or if both edges are present but edge1 is
	// older, we'll return edge1's pubkey and a blank pubkey for edge2. This
	// means that only an update from edge1 will be able to resurrect the
	// channel.
	case e1 == nil || (e2 != nil && e1.Before(*e2)):
		return node1, [33]byte{}

	// Otherwise, we're missing edge2 or edge2 is the older side, so we
	// return a blank pubkey for edge1. In this case, only an update from
	// edge2 can resurect the channel.
	default:
		return [33]byte{}, node1
	}
}

// UpdateEdgePolicy updates the edge routing policy for a single directed edge
// within the database for the referenced channel. The `flags` attribute within
// the ChannelEdgePolicy determines which of the directed edges are being
// updated. If the flag is 1, then the first node's information is being
// updated, otherwise it's the second node's information. The node ordering is
// determined by the lexicographical ordering of the identity public keys of the
// nodes on either side of the channel.
func (c *KVStore) UpdateEdgePolicy(ctx context.Context,
	edge *models.ChannelEdgePolicy,
	opts ...batch.SchedulerOption) (route.Vertex, route.Vertex, error) {

	var (
		isUpdate1    bool
		edgeNotFound bool
		from, to     route.Vertex
	)

	r := &batch.Request[kvdb.RwTx]{
		Opts: batch.NewSchedulerOptions(opts...),
		Reset: func() {
			isUpdate1 = false
			edgeNotFound = false
		},
		Do: func(tx kvdb.RwTx) error {
			// Validate that the ExtraOpaqueData is in fact a valid
			// TLV stream. This is done here instead of within
			// updateEdgePolicy so that updateEdgePolicy can be used
			// by unit tests to recreate the case where we already
			// have nodes persisted with invalid TLV data.
			err := edge.ExtraOpaqueData.ValidateTLV()
			if err != nil {
				return fmt.Errorf("%w: %w",
					ErrParsingExtraTLVBytes, err)
			}

			from, to, isUpdate1, err = updateEdgePolicy(tx, edge)
			if err != nil {
				log.Errorf("UpdateEdgePolicy faild: %v", err)
			}

			// Silence ErrEdgeNotFound so that the batch can
			// succeed, but propagate the error via local state.
			if errors.Is(err, ErrEdgeNotFound) {
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

	err := c.chanScheduler.Execute(ctx, r)

	return from, to, err
}

func (c *KVStore) updateEdgeCache(e *models.ChannelEdgePolicy,
	isUpdate1 bool) {

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
func updateEdgePolicy(tx kvdb.RwTx, edge *models.ChannelEdgePolicy) (
	route.Vertex, route.Vertex, bool, error) {

	var noVertex route.Vertex

	edges := tx.ReadWriteBucket(edgeBucket)
	if edges == nil {
		return noVertex, noVertex, false, ErrEdgeNotFound
	}
	edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
	if edgeIndex == nil {
		return noVertex, noVertex, false, ErrEdgeNotFound
	}

	// Create the channelID key be converting the channel ID
	// integer into a byte slice.
	var chanID [8]byte
	byteOrder.PutUint64(chanID[:], edge.ChannelID)

	// With the channel ID, we then fetch the value storing the two
	// nodes which connect this channel edge.
	nodeInfo := edgeIndex.Get(chanID[:])
	if nodeInfo == nil {
		return noVertex, noVertex, false, ErrEdgeNotFound
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
	err := putChanEdgePolicy(edges, edge, fromNode, toNode)
	if err != nil {
		return noVertex, noVertex, false, err
	}

	var (
		fromNodePubKey route.Vertex
		toNodePubKey   route.Vertex
	)
	copy(fromNodePubKey[:], fromNode)
	copy(toNodePubKey[:], toNode)

	return fromNodePubKey, toNodePubKey, isUpdate1, nil
}

// isPublic determines whether the node is seen as public within the graph from
// the source node's point of view. An existing database transaction can also be
// specified.
func (c *KVStore) isPublic(tx kvdb.RTx, nodePub route.Vertex,
	sourcePubKey []byte) (bool, error) {

	// In order to determine whether this node is publicly advertised within
	// the graph, we'll need to look at all of its edges and check whether
	// they extend to any other node than the source node. errDone will be
	// used to terminate the check early.
	nodeIsPublic := false
	errDone := errors.New("done")
	err := c.forEachNodeChannelTx(tx, nodePub, func(tx kvdb.RTx,
		info *models.ChannelEdgeInfo, _ *models.ChannelEdgePolicy,
		_ *models.ChannelEdgePolicy) error {

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
	}, func() {
		nodeIsPublic = false
	})
	if err != nil && !errors.Is(err, errDone) {
		return false, err
	}

	return nodeIsPublic, nil
}

// fetchNodeTx attempts to look up a target node by its identity
// public key. If the node isn't found in the database, then
// ErrGraphNodeNotFound is returned. An optional transaction may be provided.
// If none is provided, then a new one will be created.
func (c *KVStore) fetchNodeTx(tx kvdb.RTx, nodePub route.Vertex) (*models.Node,
	error) {

	return c.fetchLightningNode(tx, nodePub)
}

// FetchNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then ErrGraphNodeNotFound is
// returned.
func (c *KVStore) FetchNode(_ context.Context,
	nodePub route.Vertex) (*models.Node, error) {

	return c.fetchLightningNode(nil, nodePub)
}

// fetchLightningNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then ErrGraphNodeNotFound is
// returned. An optional transaction may be provided. If none is provided, then
// a new one will be created.
func (c *KVStore) fetchLightningNode(tx kvdb.RTx,
	nodePub route.Vertex) (*models.Node, error) {

	var node *models.Node
	fetch := func(tx kvdb.RTx) error {
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

		node = n

		return nil
	}

	if tx == nil {
		err := kvdb.View(
			c.db, fetch, func() {
				node = nil
			},
		)
		if err != nil {
			return nil, err
		}

		return node, nil
	}

	err := fetch(tx)
	if err != nil {
		return nil, err
	}

	return node, nil
}

// HasNode determines if the graph has a vertex identified by the target node
// identity public key. If the node exists in the database, a timestamp of when
// the data for the node was lasted updated is returned along with a true
// boolean. Otherwise, an empty time.Time is returned with a false boolean.
func (c *KVStore) HasNode(_ context.Context,
	nodePub [33]byte) (time.Time, bool, error) {

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
//
// NOTE: the reset param is only meaningful if the tx param is nil. If it is
// not nil, the caller is expected to have passed in a reset to the parent
// function's View/Update call which will then apply to the whole transaction.
func nodeTraversal(tx kvdb.RTx, nodePub []byte, db kvdb.Backend,
	cb func(kvdb.RTx, *models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	traversal := func(tx kvdb.RTx) error {
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
		for nodeEdge, _ := edgeCursor.Seek(nodeStart[:]); bytes.HasPrefix(nodeEdge, nodePub); nodeEdge, _ = edgeCursor.Next() { //nolint:ll
			// If the prefix still matches, the channel id is
			// returned in nodeEdge. Channel id is used to lookup
			// the node at the other end of the channel and both
			// edge policies.
			chanID := nodeEdge[33:]
			edgeInfo, err := fetchChanEdgeInfo(edgeIndex, chanID)
			if err != nil {
				return err
			}

			outgoingPolicy, err := fetchChanEdgePolicy(
				edges, chanID, nodePub,
			)
			if err != nil {
				return err
			}

			otherNode, err := edgeInfo.OtherNodeKeyBytes(nodePub)
			if err != nil {
				return err
			}

			incomingPolicy, err := fetchChanEdgePolicy(
				edges, chanID, otherNode[:],
			)
			if err != nil {
				return err
			}

			// Finally, we execute the callback.
			err = cb(tx, edgeInfo, outgoingPolicy, incomingPolicy)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// If no transaction was provided, then we'll create a new transaction
	// to execute the transaction within.
	if tx == nil {
		return kvdb.View(db, traversal, reset)
	}

	// Otherwise, we re-use the existing transaction to execute the graph
	// traversal.
	return traversal(tx)
}

// ForEachNodeChannel iterates through all channels of the given node,
// executing the passed callback with an edge info structure and the policies
// of each end of the channel. The first edge policy is the outgoing edge *to*
// the connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
func (c *KVStore) ForEachNodeChannel(_ context.Context, nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	return nodeTraversal(
		nil, nodePub[:], c.db, func(_ kvdb.RTx,
			info *models.ChannelEdgeInfo, policy,
			policy2 *models.ChannelEdgePolicy) error {

			return cb(info, policy, policy2)
		}, reset,
	)
}

// ForEachSourceNodeChannel iterates through all channels of the source node,
// executing the passed callback on each. The callback is provided with the
// channel's outpoint, whether we have a policy for the channel and the channel
// peer's node information.
func (c *KVStore) ForEachSourceNodeChannel(_ context.Context,
	cb func(chanPoint wire.OutPoint, havePolicy bool,
		otherNode *models.Node) error, reset func()) error {

	return kvdb.View(c.db, func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		node, err := sourceNodeWithTx(nodes)
		if err != nil {
			return err
		}

		return nodeTraversal(
			tx, node.PubKeyBytes[:], c.db, func(tx kvdb.RTx,
				info *models.ChannelEdgeInfo,
				policy, _ *models.ChannelEdgePolicy) error {

				peer, err := c.fetchOtherNode(
					tx, info, node.PubKeyBytes[:],
				)
				if err != nil {
					return err
				}

				return cb(
					info.ChannelPoint, policy != nil, peer,
				)
			}, reset,
		)
	}, reset)
}

// forEachNodeChannelTx iterates through all channels of the given node,
// executing the passed callback with an edge info structure and the policies
// of each end of the channel. The first edge policy is the outgoing edge *to*
// the connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
//
// If the caller wishes to re-use an existing boltdb transaction, then it
// should be passed as the first argument.  Otherwise, the first argument should
// be nil and a fresh transaction will be created to execute the graph
// traversal.
//
// NOTE: the reset function is only meaningful if the tx param is nil.
func (c *KVStore) forEachNodeChannelTx(tx kvdb.RTx,
	nodePub route.Vertex, cb func(kvdb.RTx, *models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy) error,
	reset func()) error {

	return nodeTraversal(tx, nodePub[:], c.db, cb, reset)
}

// fetchOtherNode attempts to fetch the full Node that's opposite of
// the target node in the channel. This is useful when one knows the pubkey of
// one of the nodes, and wishes to obtain the full Node for the other
// end of the channel.
func (c *KVStore) fetchOtherNode(tx kvdb.RTx,
	channel *models.ChannelEdgeInfo, thisNodeKey []byte) (
	*models.Node, error) {

	// Ensure that the node passed in is actually a member of the channel.
	var targetNodeBytes [33]byte
	switch {
	case bytes.Equal(channel.NodeKey1Bytes[:], thisNodeKey):
		targetNodeBytes = channel.NodeKey2Bytes
	case bytes.Equal(channel.NodeKey2Bytes[:], thisNodeKey):
		targetNodeBytes = channel.NodeKey1Bytes
	default:
		return nil, fmt.Errorf("node not participating in this channel")
	}

	var targetNode *models.Node
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

		targetNode = node

		return nil
	}

	// If the transaction is nil, then we'll need to create a new one,
	// otherwise we can use the existing db transaction.
	var err error
	if tx == nil {
		err = kvdb.View(c.db, fetchNodeFunc, func() {
			targetNode = nil
		})
	} else {
		err = fetchNodeFunc(tx)
	}

	return targetNode, err
}

// computeEdgePolicyKeys is a helper function that can be used to compute the
// keys used to index the channel edge policy info for the two nodes of the
// edge. The keys for node 1 and node 2 are returned respectively.
func computeEdgePolicyKeys(info *models.ChannelEdgeInfo) ([]byte, []byte) {
	var (
		node1Key [33 + 8]byte
		node2Key [33 + 8]byte
	)

	copy(node1Key[:], info.NodeKey1Bytes[:])
	copy(node2Key[:], info.NodeKey2Bytes[:])

	byteOrder.PutUint64(node1Key[33:], info.ChannelID)
	byteOrder.PutUint64(node2Key[33:], info.ChannelID)

	return node1Key[:], node2Key[:]
}

// FetchChannelEdgesByOutpoint attempts to lookup the two directed edges for
// the channel identified by the funding outpoint. If the channel can't be
// found, then ErrEdgeNotFound is returned. A struct which houses the general
// information for the channel itself is returned as well as two structs that
// contain the routing policies for the channel in either direction.
func (c *KVStore) FetchChannelEdgesByOutpoint(op *wire.OutPoint) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	var (
		edgeInfo *models.ChannelEdgeInfo
		policy1  *models.ChannelEdgePolicy
		policy2  *models.ChannelEdgePolicy
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
		if err := WriteOutpoint(&b, op); err != nil {
			return err
		}
		chanID := chanIndex.Get(b.Bytes())
		if chanID == nil {
			return fmt.Errorf("%w: op=%v", ErrEdgeNotFound, op)
		}

		// If the channel is found to exists, then we'll first retrieve
		// the general information for the channel.
		edge, err := fetchChanEdgeInfo(edgeIndex, chanID)
		if err != nil {
			return fmt.Errorf("%w: chanID=%x", err, chanID)
		}
		edgeInfo = edge

		// Once we have the information about the channels' parameters,
		// we'll fetch the routing policies for each for the directed
		// edges.
		e1, e2, err := fetchChanEdgePolicies(edgeIndex, edges, chanID)
		if err != nil {
			return fmt.Errorf("failed to find policy: %w", err)
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
func (c *KVStore) FetchChannelEdgesByID(chanID uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	var (
		edgeInfo  *models.ChannelEdgeInfo
		policy1   *models.ChannelEdgePolicy
		policy2   *models.ChannelEdgePolicy
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
		if errors.Is(err, ErrEdgeNotFound) {
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
			edgeInfo = &models.ChannelEdgeInfo{
				NodeKey1Bytes: pubKey1,
				NodeKey2Bytes: pubKey2,
			}

			return ErrZombieEdge
		}

		// Otherwise, we'll just return the error if any.
		if err != nil {
			return err
		}

		edgeInfo = edge

		// Then we'll attempt to fetch the accompanying policies of this
		// edge.
		e1, e2, err := fetchChanEdgePolicies(
			edgeIndex, edges, channelID[:],
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
	if errors.Is(err, ErrZombieEdge) {
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
func (c *KVStore) IsPublicNode(pubKey [33]byte) (bool, error) {
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

		nodeIsPublic, err = c.isPublic(tx, node.PubKeyBytes, ourPubKey)

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
func (c *KVStore) ChannelView() ([]EdgePoint, error) {
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
		return chanIndex.ForEach(
			func(chanPointBytes, chanID []byte) error {
				chanPointReader := bytes.NewReader(
					chanPointBytes,
				)

				var chanPoint wire.OutPoint
				err := ReadOutpoint(chanPointReader, &chanPoint)
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
			},
		)
	}, func() {
		edgePoints = nil
	}); err != nil {
		return nil, err
	}

	return edgePoints, nil
}

// MarkEdgeZombie attempts to mark a channel identified by its channel ID as a
// zombie. This method is used on an ad-hoc basis, when channels need to be
// marked as zombies outside the normal pruning cycle.
func (c *KVStore) MarkEdgeZombie(chanID uint64,
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
func (c *KVStore) MarkEdgeLive(chanID uint64) error {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	return c.markEdgeLiveUnsafe(nil, chanID)
}

// markEdgeLiveUnsafe clears an edge from the zombie index. This method can be
// called with an existing kvdb.RwTx or the argument can be set to nil in which
// case a new transaction will be created.
//
// NOTE: this method MUST only be called if the cacheMu has already been
// acquired.
func (c *KVStore) markEdgeLiveUnsafe(tx kvdb.RwTx, chanID uint64) error {
	dbFn := func(tx kvdb.RwTx) error {
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

		if len(zombieIndex.Get(k[:])) == 0 {
			return ErrZombieEdgeNotFound
		}

		return zombieIndex.Delete(k[:])
	}

	// If the transaction is nil, we'll create a new one. Otherwise, we use
	// the existing transaction
	var err error
	if tx == nil {
		err = kvdb.Update(c.db, dbFn, func() {})
	} else {
		err = dbFn(tx)
	}
	if err != nil {
		return err
	}

	c.rejectCache.remove(chanID)
	c.chanCache.remove(chanID)

	return nil
}

// IsZombieEdge returns whether the edge is considered zombie. If it is a
// zombie, then the two node public keys corresponding to this edge are also
// returned.
func (c *KVStore) IsZombieEdge(chanID uint64) (bool, [33]byte, [33]byte,
	error) {

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
		return false, [33]byte{}, [33]byte{}, fmt.Errorf("%w: %w "+
			"(chanID=%d)", ErrCantCheckIfZombieEdgeStr, err, chanID)
	}

	return isZombie, pubKey1, pubKey2, nil
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
func (c *KVStore) NumZombies() (uint64, error) {
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

// PutClosedScid stores a SCID for a closed channel in the database. This is so
// that we can ignore channel announcements that we know to be closed without
// having to validate them and fetch a block.
func (c *KVStore) PutClosedScid(scid lnwire.ShortChannelID) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		closedScids, err := tx.CreateTopLevelBucket(closedScidBucket)
		if err != nil {
			return err
		}

		var k [8]byte
		byteOrder.PutUint64(k[:], scid.ToUint64())

		return closedScids.Put(k[:], []byte{})
	}, func() {})
}

// IsClosedScid checks whether a channel identified by the passed in scid is
// closed. This helps avoid having to perform expensive validation checks.
// TODO: Add an LRU cache to cut down on disc reads.
func (c *KVStore) IsClosedScid(scid lnwire.ShortChannelID) (bool, error) {
	var isClosed bool
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		closedScids := tx.ReadBucket(closedScidBucket)
		if closedScids == nil {
			return ErrClosedScidsNotFound
		}

		var k [8]byte
		byteOrder.PutUint64(k[:], scid.ToUint64())

		if closedScids.Get(k[:]) != nil {
			isClosed = true
			return nil
		}

		return nil
	}, func() {
		isClosed = false
	})
	if err != nil {
		return false, err
	}

	return isClosed, nil
}

// GraphSession will provide the call-back with access to a NodeTraverser
// instance which can be used to perform queries against the channel graph.
func (c *KVStore) GraphSession(cb func(graph NodeTraverser) error,
	reset func()) error {

	return c.db.View(func(tx walletdb.ReadTx) error {
		return cb(&nodeTraverserSession{
			db: c,
			tx: tx,
		})
	}, reset)
}

// nodeTraverserSession implements the NodeTraverser interface but with a
// backing read only transaction for a consistent view of the graph.
type nodeTraverserSession struct {
	tx kvdb.RTx
	db *KVStore
}

// ForEachNodeDirectedChannel calls the callback for every channel of the given
// node.
//
// NOTE: Part of the NodeTraverser interface.
func (c *nodeTraverserSession) ForEachNodeDirectedChannel(nodePub route.Vertex,
	cb func(channel *DirectedChannel) error, _ func()) error {

	return c.db.forEachNodeDirectedChannel(c.tx, nodePub, cb, func() {})
}

// FetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the NodeTraverser interface.
func (c *nodeTraverserSession) FetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	return c.db.fetchNodeFeatures(c.tx, nodePub)
}

func putLightningNode(nodeBucket, aliasBucket, updateIndex kvdb.RwBucket,
	node *models.Node) error {

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
	if !node.HaveAnnouncement() {
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

	nodeColor := node.Color.UnwrapOr(color.RGBA{})

	if err := binary.Write(&b, byteOrder, nodeColor.R); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, nodeColor.G); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, nodeColor.B); err != nil {
		return err
	}

	err = wire.WriteVarString(&b, 0, node.Alias.UnwrapOr(""))
	if err != nil {
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
		if err := SerializeAddr(&b, address); err != nil {
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

	err = aliasBucket.Put(nodePub, []byte(node.Alias.UnwrapOr("")))
	if err != nil {
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
	nodePub []byte) (*models.Node, error) {

	nodeBytes := nodeBucket.Get(nodePub)
	if nodeBytes == nil {
		return nil, ErrGraphNodeNotFound
	}

	nodeReader := bytes.NewReader(nodeBytes)

	return deserializeLightningNode(nodeReader)
}

func deserializeLightningNodeCacheable(r io.Reader) (route.Vertex,
	*lnwire.FeatureVector, error) {

	var (
		pubKey      route.Vertex
		features    = lnwire.EmptyFeatureVector()
		nodeScratch [8]byte
	)

	// Skip ahead:
	// - LastUpdate (8 bytes)
	if _, err := r.Read(nodeScratch[:]); err != nil {
		return pubKey, nil, err
	}

	if _, err := io.ReadFull(r, pubKey[:]); err != nil {
		return pubKey, nil, err
	}

	// Read the node announcement flag.
	if _, err := r.Read(nodeScratch[:2]); err != nil {
		return pubKey, nil, err
	}
	hasNodeAnn := byteOrder.Uint16(nodeScratch[:2])

	// The rest of the data is optional, and will only be there if we got a
	// node announcement for this node.
	if hasNodeAnn == 0 {
		return pubKey, features, nil
	}

	// We did get a node announcement for this node, so we'll have the rest
	// of the data available.
	var rgb uint8
	if err := binary.Read(r, byteOrder, &rgb); err != nil {
		return pubKey, nil, err
	}
	if err := binary.Read(r, byteOrder, &rgb); err != nil {
		return pubKey, nil, err
	}
	if err := binary.Read(r, byteOrder, &rgb); err != nil {
		return pubKey, nil, err
	}

	if _, err := wire.ReadVarString(r, 0); err != nil {
		return pubKey, nil, err
	}

	if err := features.Decode(r); err != nil {
		return pubKey, nil, err
	}

	return pubKey, features, nil
}

func deserializeLightningNode(r io.Reader) (*models.Node, error) {
	var (
		scratch [8]byte
		err     error
		pubKey  [33]byte
	)

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}

	unix := int64(byteOrder.Uint64(scratch[:]))
	lastUpdate := time.Unix(unix, 0)

	if _, err := io.ReadFull(r, pubKey[:]); err != nil {
		return nil, err
	}

	node := models.NewV1ShellNode(pubKey)
	node.LastUpdate = lastUpdate

	if _, err := r.Read(scratch[:2]); err != nil {
		return nil, err
	}

	hasNodeAnn := byteOrder.Uint16(scratch[:2])
	// The rest of the data is optional, and will only be there if we got a
	// node announcement for this node.
	if hasNodeAnn == 0 {
		return node, nil
	}

	// We did get a node announcement for this node, so we'll have the rest
	// of the data available.
	var nodeColor color.RGBA
	if err := binary.Read(r, byteOrder, &nodeColor.R); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &nodeColor.G); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &nodeColor.B); err != nil {
		return nil, err
	}
	node.Color = fn.Some(nodeColor)

	alias, err := wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}
	node.Alias = fn.Some(alias)

	err = node.Features.Decode(r)
	if err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:2]); err != nil {
		return nil, err
	}
	numAddresses := int(byteOrder.Uint16(scratch[:2]))

	var addresses []net.Addr
	for i := 0; i < numAddresses; i++ {
		address, err := DeserializeAddr(r)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	node.Addresses = addresses

	node.AuthSigBytes, err = wire.ReadVarBytes(r, 0, 80, "sig")
	if err != nil {
		return nil, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the node as is.
	extraBytes, err := wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, err
	}

	if len(extraBytes) > 0 {
		node.ExtraOpaqueData = extraBytes
	}

	return node, nil
}

func putChanEdgeInfo(edgeIndex kvdb.RwBucket,
	edgeInfo *models.ChannelEdgeInfo, chanID [8]byte) error {

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

	var featureBuf bytes.Buffer
	if err := edgeInfo.Features.Encode(&featureBuf); err != nil {
		return fmt.Errorf("unable to encode features: %w", err)
	}

	if err := wire.WriteVarBytes(&b, 0, featureBuf.Bytes()); err != nil {
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

	if err := WriteOutpoint(&b, &edgeInfo.ChannelPoint); err != nil {
		return err
	}
	err := binary.Write(&b, byteOrder, uint64(edgeInfo.Capacity))
	if err != nil {
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
	err = wire.WriteVarBytes(&b, 0, edgeInfo.ExtraOpaqueData)
	if err != nil {
		return err
	}

	return edgeIndex.Put(chanID[:], b.Bytes())
}

func fetchChanEdgeInfo(edgeIndex kvdb.RBucket,
	chanID []byte) (*models.ChannelEdgeInfo, error) {

	edgeInfoBytes := edgeIndex.Get(chanID)
	if edgeInfoBytes == nil {
		return nil, ErrEdgeNotFound
	}

	edgeInfoReader := bytes.NewReader(edgeInfoBytes)

	return deserializeChanEdgeInfo(edgeInfoReader)
}

func deserializeChanEdgeInfo(r io.Reader) (*models.ChannelEdgeInfo, error) {
	var (
		err      error
		edgeInfo models.ChannelEdgeInfo
	)

	if _, err := io.ReadFull(r, edgeInfo.NodeKey1Bytes[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, edgeInfo.NodeKey2Bytes[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey1Bytes[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, edgeInfo.BitcoinKey2Bytes[:]); err != nil {
		return nil, err
	}

	featureBytes, err := wire.ReadVarBytes(r, 0, 900, "features")
	if err != nil {
		return nil, err
	}

	features := lnwire.NewRawFeatureVector()
	err = features.Decode(bytes.NewReader(featureBytes))
	if err != nil {
		return nil, fmt.Errorf("unable to decode "+
			"features: %w", err)
	}
	edgeInfo.Features = lnwire.NewFeatureVector(features, lnwire.Features)

	proof := &models.ChannelAuthProof{}

	proof.NodeSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}
	proof.NodeSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}
	proof.BitcoinSig1Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}
	proof.BitcoinSig2Bytes, err = wire.ReadVarBytes(r, 0, 80, "sigs")
	if err != nil {
		return nil, err
	}

	if !proof.IsEmpty() {
		edgeInfo.AuthProof = proof
	}

	edgeInfo.ChannelPoint = wire.OutPoint{}
	if err := ReadOutpoint(r, &edgeInfo.ChannelPoint); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.Capacity); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edgeInfo.ChannelID); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, edgeInfo.ChainHash[:]); err != nil {
		return nil, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edgeInfo.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, err
	}

	return &edgeInfo, nil
}

func putChanEdgePolicy(edges kvdb.RwBucket, edge *models.ChannelEdgePolicy,
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
		!bytes.Equal(edgeBytes, unknownPolicy) {

		// In order to delete the old entry, we'll need to obtain the
		// *prior* update time in order to delete it. To do this, we'll
		// need to deserialize the existing policy within the database
		// (now outdated by the new one), and delete its corresponding
		// entry within the update index. We'll ignore any
		// ErrEdgePolicyOptionalFieldNotFound or ErrParsingExtraTLVBytes
		// errors, as we only need the channel ID and update time to
		// delete the entry.
		//
		// TODO(halseth): get rid of these invalid policies in a
		// migration.
		//
		// NOTE: the above TODO was completed in the SQL migration and
		// so such edge cases no longer need to be handled there.
		oldEdgePolicy, err := deserializeChanEdgePolicy(
			bytes.NewReader(edgeBytes),
		)
		if err != nil &&
			!errors.Is(err, ErrEdgePolicyOptionalFieldNotFound) &&
			!errors.Is(err, ErrParsingExtraTLVBytes) {

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

	err = updateEdgePolicyDisabledIndex(
		edges, edge.ChannelID,
		edge.ChannelFlags&lnwire.ChanUpdateDirection > 0,
		edge.IsDisabled(),
	)
	if err != nil {
		return err
	}

	return edges.Put(edgeKey[:], b.Bytes())
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
	nodePub []byte) (*models.ChannelEdgePolicy, error) {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], nodePub)
	copy(edgeKey[33:], chanID)

	edgeBytes := edges.Get(edgeKey[:])
	if edgeBytes == nil {
		return nil, ErrEdgeNotFound
	}

	// No need to deserialize unknown policy.
	if bytes.Equal(edgeBytes, unknownPolicy) {
		return nil, nil
	}

	edgeReader := bytes.NewReader(edgeBytes)

	ep, err := deserializeChanEdgePolicy(edgeReader)
	switch {
	// If the db policy was missing an expected optional field, we return
	// nil as if the policy was unknown.
	case errors.Is(err, ErrEdgePolicyOptionalFieldNotFound):
		return nil, nil

	// If the policy contains invalid TLV bytes, we return nil as if
	// the policy was unknown.
	case errors.Is(err, ErrParsingExtraTLVBytes):
		return nil, nil

	case err != nil:
		return nil, err
	}

	return ep, nil
}

func fetchChanEdgePolicies(edgeIndex kvdb.RBucket, edges kvdb.RBucket,
	chanID []byte) (*models.ChannelEdgePolicy, *models.ChannelEdgePolicy,
	error) {

	edgeInfo := edgeIndex.Get(chanID)
	if edgeInfo == nil {
		return nil, nil, fmt.Errorf("%w: chanID=%x", ErrEdgeNotFound,
			chanID)
	}

	// The first node is contained within the first half of the edge
	// information. We only propagate the error here and below if it's
	// something other than edge non-existence.
	node1Pub := edgeInfo[:33]
	edge1, err := fetchChanEdgePolicy(edges, chanID, node1Pub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: node1Pub=%x", ErrEdgeNotFound,
			node1Pub)
	}

	// Similarly, the second node is contained within the latter
	// half of the edge information.
	node2Pub := edgeInfo[33:66]
	edge2, err := fetchChanEdgePolicy(edges, chanID, node2Pub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: node2Pub=%x", ErrEdgeNotFound,
			node2Pub)
	}

	return edge1, edge2, nil
}

func serializeChanEdgePolicy(w io.Writer, edge *models.ChannelEdgePolicy,
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
	err = binary.Write(w, byteOrder, uint64(edge.FeeBaseMSat))
	if err != nil {
		return err
	}
	err = binary.Write(
		w, byteOrder, uint64(edge.FeeProportionalMillionths),
	)
	if err != nil {
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

func deserializeChanEdgePolicy(r io.Reader) (*models.ChannelEdgePolicy, error) {
	// Deserialize the policy. Note that in case an optional field is not
	// found or if the edge has invalid TLV data, then both an error and a
	// populated policy object are returned so that the caller can decide
	// if it still wants to use the edge or not.
	edge, err := deserializeChanEdgePolicyRaw(r)
	if err != nil &&
		!errors.Is(err, ErrEdgePolicyOptionalFieldNotFound) &&
		!errors.Is(err, ErrParsingExtraTLVBytes) {

		return nil, err
	}

	return edge, err
}

func deserializeChanEdgePolicyRaw(r io.Reader) (*models.ChannelEdgePolicy,
	error) {

	edge := &models.ChannelEdgePolicy{}

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

	if _, err := r.Read(edge.ToNode[:]); err != nil {
		return nil, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edge.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
	case errors.Is(err, io.EOF):
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

	// Attempt to extract the inbound fee from the opaque data. If we fail
	// to parse the TLV here, we return an error we also return the edge
	// so that the caller can still use it. This is for backwards
	// compatibility in case we have already persisted some policies that
	// have invalid TLV data.
	var inboundFee lnwire.Fee
	typeMap, err := edge.ExtraOpaqueData.ExtractRecords(&inboundFee)
	if err != nil {
		return edge, fmt.Errorf("%w: %w", ErrParsingExtraTLVBytes, err)
	}

	val, ok := typeMap[lnwire.FeeRecordType]
	if ok && val == nil {
		edge.InboundFee = fn.Some(inboundFee)
	}

	return edge, nil
}
