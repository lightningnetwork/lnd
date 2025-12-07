package migration1

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"io"
	"net"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
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
}

// A compile-time assertion to ensure that the KVStore struct implements the
// V1Store interface.
var _ V1Store = (*KVStore)(nil)

// NewKVStore allocates a new KVStore backed by a DB instance. The
// returned instance has its own unique reject cache and channel cache.
func NewKVStore(db kvdb.Backend) (*KVStore, error) {
	if err := initKVStore(db); err != nil {
		return nil, err
	}

	g := &KVStore{
		db: db,
	}

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

// SourceNode returns the source node of the graph. The source node is treated
// as the center node within a star-graph. This method may be used to kick off
// a path finding algorithm in order to explore the reachability of another
// node based off the source node.
func (c *KVStore) SourceNode(_ context.Context) (*models.Node, error) {
	return sourceNode(c.db)
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
func (c *KVStore) AddNode(_ context.Context,
	node *models.Node, _ ...batch.SchedulerOption) error {

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		return addLightningNode(tx, node)
	}, func() {})
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
func (c *KVStore) AddChannelEdge(_ context.Context,
	edge *models.ChannelEdgeInfo, _ ...batch.SchedulerOption) error {

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		return c.addChannelEdge(tx, edge)
	}, func() {})
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

	return chansClosed, prunedNodes, nil
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
func (c *KVStore) UpdateEdgePolicy(_ context.Context,
	edge *models.ChannelEdgePolicy,
	_ ...batch.SchedulerOption) (route.Vertex, route.Vertex, error) {

	var from, to route.Vertex
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
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

		from, to, _, err = updateEdgePolicy(tx, edge)

		return err
	}, func() {})

	return from, to, err
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

// MarkEdgeZombie attempts to mark a channel identified by its channel ID as a
// zombie. This method is used on an ad-hoc basis, when channels need to be
// marked as zombies outside the normal pruning cycle.
func (c *KVStore) MarkEdgeZombie(chanID uint64,
	pubKey1, pubKey2 [33]byte) error {

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
