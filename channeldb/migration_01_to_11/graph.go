package migration_01_to_11

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/color"
	"io"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
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
	db *DB
}

// newChannelGraph allocates a new ChannelGraph backed by a DB instance. The
// returned instance has its own unique reject cache and channel cache.
func newChannelGraph(db *DB, rejectCacheSize, chanCacheSize int) *ChannelGraph {
	return &ChannelGraph{
		db: db,
	}
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

// updateEdgePolicy attempts to update an edge's policy within the relevant
// buckets using an existing database transaction. The returned boolean will be
// true if the updated policy belongs to node1, and false if the policy belonged
// to node2.
func updateEdgePolicy(tx kvdb.RwTx, edge *ChannelEdgePolicy) (bool, error) {
	edges, err := tx.CreateTopLevelBucket(edgeBucket)
	if err != nil {
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

	db *DB

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

	key, err := btcec.ParsePubKey(l.PubKeyBytes[:], btcec.S256())
	if err != nil {
		return nil, err
	}
	l.pubKey = key

	return key, nil
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

	// NodeKey2Bytes is the raw public key of the first node.
	NodeKey2Bytes [33]byte

	// BitcoinKey1Bytes is the raw public key of the first node.
	BitcoinKey1Bytes [33]byte

	// BitcoinKey2Bytes is the raw public key of the first node.
	BitcoinKey2Bytes [33]byte

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
}

// ChannelAuthProof is the authentication proof (the signature portion) for a
// channel. Using the four signatures contained in the struct, and some
// auxiliary knowledge (the funding script, node identities, and outpoint) nodes
// on the network are able to validate the authenticity and existence of a
// channel. Each of these signatures signs the following digest: chanID ||
// nodeID1 || nodeID2 || bitcoinKey1|| bitcoinKey2 || 2-byte-feature-len ||
// features.
type ChannelAuthProof struct {
	// NodeSig1Bytes are the raw bytes of the first node signature encoded
	// in DER format.
	NodeSig1Bytes []byte

	// NodeSig2Bytes are the raw bytes of the second node signature
	// encoded in DER format.
	NodeSig2Bytes []byte

	// BitcoinSig1Bytes are the raw bytes of the first bitcoin signature
	// encoded in DER format.
	BitcoinSig1Bytes []byte

	// BitcoinSig2Bytes are the raw bytes of the second bitcoin signature
	// encoded in DER format.
	BitcoinSig2Bytes []byte
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

	// MinHTLC is the smallest value HTLC this node will accept, expressed
	// in millisatoshi.
	MinHTLC lnwire.MilliSatoshi

	// MaxHTLC is the largest value HTLC this node will accept, expressed
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
}

// IsDisabled determines whether the edge has the disabled bit set.
func (c *ChannelEdgePolicy) IsDisabled() bool {
	return c.ChannelFlags&lnwire.ChanUpdateDisabled ==
		lnwire.ChanUpdateDisabled
}

func putLightningNode(nodeBucket kvdb.RwBucket, aliasBucket kvdb.RwBucket,
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

func deserializeLightningNode(r io.Reader) (LightningNode, error) {
	var (
		node    LightningNode
		scratch [8]byte
		err     error
	)

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

	fv := lnwire.NewFeatureVector(nil, nil)
	err = fv.Decode(r)
	if err != nil {
		return LightningNode{}, err
	}
	node.Features = fv

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
		return fmt.Errorf("Cannot write unknown policy for channel %v "+
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

	node, err := fetchLightningNode(nodes, pub[:])
	if err != nil {
		return nil, fmt.Errorf("unable to fetch node: %x, %v",
			pub[:], err)
	}
	edge.Node = &node

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
