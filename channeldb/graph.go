package channeldb

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"io"
	"net"
	"time"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
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
	// maps: pubKey -> nofInfo
	// maps: source -> selfPubKey
	nodeBucket = []byte("graph-node")

	// sourceKey is a special key that resides within the nodeBucket. The
	// sourceKey maps a key to the public key of the "self node".
	sourceKey = []byte("source")

	// aliasIndexBucket is a sub-bucket that's nested within the main
	// nodeBucket. This bucket maps the public key of a node to it's
	// current alias. This bucket is provided as it can be used within a
	// future UI layer to add an additional degree of confirmation.
	aliasIndexBucket = []byte("alias")

	// edgeBucket is a bucket which houses all of the edge or channel
	// information within the channel graph. This bucket essentially acts
	// as an adjacency list, which in conjunction with a range scan, can be
	// used to iterate over all the _outgoing_ edges for a particular node.
	// Key in the bucket use a prefix scheme which leads with the node's
	// public key and sends with the compact edge ID. For each edgeID,
	// there will be two entries within the bucket, as the graph is
	// directed: nodes may have different policies w.r.t to fees for their
	// respective directions.
	//
	// maps: pubKey || edgeID -> edge for node
	edgeBucket = []byte("graph-edge")

	// chanStart is an array of all zero bytes which is used to perform
	// range scans within the edgeBucket to obtain all of the outgoing
	// edges for a particular node.
	chanStart [8]byte

	// edgeIndexBucket is an index which can be used to iterate all edges
	// in the bucket, grouping them according to their in/out nodes. This
	// bucket resides within the edgeBucket above.  Creation of a edge
	// proceeds in two phases: first the edge is added to the edge index,
	// afterwards the edgeBucket can be updated with the latest details of
	// the edge as they are announced on the network.
	//
	// maps: chanID -> pub1 || pub2
	edgeIndexBucket = []byte("edge-index")

	// channelPointBucket maps a channel's full outpoint (txid:index) to
	// its short 8-byte channel ID. This bucket resides within the
	// edgeBucket above, and can be used to quickly remove an edge due to
	// the outpoint being spent, or to query for existence of a channel.
	//
	// maps: outPoint -> chanID
	channelPointBucket = []byte("chan-index")

	// graphMetaBucket is a top-level bucket which stores various meta-deta
	// related to the on-disk channel graph. Data strored in this bucket
	// includes the block to which the graph has been synced to, the total
	// number of channels, etc.
	graphMetaBucket = []byte("graph-meta")

	// pruneTipKey is a key within the above graphMetaBucket that stores
	// the best known blockhash+height that the channel graph has been
	// known to be pruned to. Once a new block is discovered, any channels
	// that have been closed (by spending the outpoint) can safely be
	// removed from the graph.
	pruneTipKey = []byte("prune-tip")

	edgeBloomKey = []byte("edge-bloom")
	nodeBloomKey = []byte("node-bloom")
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

	// TODO(roasbeef): store and update bloom filter to reduce disk access
	// due to current gossip model
	//  * LRU cache for edges?
}

// ForEachChannel iterates through all the channel edges stored within the
// graph and invokes the passed callback for each edge. The callback takes two
// edges as since this is a directed graph, both the in/out edges are visited.
// If the callback returns an error, then the transaction is aborted and the
// iteration stops early.
func (c *ChannelGraph) ForEachChannel(cb func(*ChannelEdge, *ChannelEdge) error) error {
	// TODO(roasbeef): ptr map to reduce # of allocs? no duplicates

	return c.db.View(func(tx *bolt.Tx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		// For each edge pair within the edge index, we fetch each edge
		// itself and also the node information in order to fully
		// populated the objecvt.
		return edgeIndex.ForEach(func(chanID, edgeInfo []byte) error {
			// The first node is contained within the first half of
			// the edge information.
			node1Pub := edgeInfo[:33]
			edge1, err := fetchChannelEdge(edges, chanID, node1Pub, nodes)
			if err != nil {
				return err
			}
			edge1.db = c.db
			edge1.Node.db = c.db

			// Similarly, the second node is contained within the
			// latter half of the edge information.
			node2Pub := edgeInfo[33:]
			edge2, err := fetchChannelEdge(edges, chanID, node2Pub, nodes)
			if err != nil {
				return err
			}
			edge2.db = c.db
			edge2.Node.db = c.db

			// TODO(roasbeef): second edge might not have
			// propagated through network yet (or possibly never
			// will be)

			// With both edges read, execute the call back. IF this
			// function returns an error then the transaction will
			// be aborted.
			return cb(edge1, edge2)
		})
	})
}

// ForEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early.
func (c *ChannelGraph) ForEachNode(cb func(*LightningNode) error) error {
	// TODO(roasbeef): need to also pass in a transaction? or reverse order
	// to get all in memory THEN execute callback?

	return c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
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
			return cb(node)
		})
	})
}

// SourceNode returns the source node of the graph. The source node is treated
// as the center node within a star-graph. This method may be used to kick off
// a path finding algorithm in order to explore the reachability of another
// node based off the source node.
func (c *ChannelGraph) SourceNode() (*LightningNode, error) {
	var source *LightningNode
	err := c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		selfPub := nodes.Get(sourceKey)
		if selfPub == nil {
			return ErrSourceNodeNotSet
		}

		// With the pubKey of the source node retrieved, we're able to
		// fetch the full node information.
		node, err := fetchLightningNode(nodes, selfPub)
		if err != nil {
			return err
		}

		source = node
		source.db = c.db
		return nil
	})
	if err != nil {
		return nil, err
	}

	return source, nil
}

// SetSourceNode sets the source node within the graph database. The source
// node is to be used as the center of a star-graph within path finding
// algorithms.
func (c *ChannelGraph) SetSourceNode(node *LightningNode) error {
	nodePub := node.PubKey.SerializeCompressed()
	return c.db.Update(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
		if err != nil {
			return err
		}

		// Next we create the mapping from source to the targeted
		// public key.
		if err := nodes.Put(sourceKey, nodePub); err != nil {
			return err
		}

		// Finally, we commit the information of the lightning node
		// itself.
		return addLightningNode(tx, node)
	})
}

// AddLightningNode adds a new (unconnected) vertex/node to the graph database.
// When adding an edge, each node must be added before the edge can be
// inserted. Afterwards the edge information can then be updated.
// TODO(roasbeef): also need sig of announcement
func (c *ChannelGraph) AddLightningNode(node *LightningNode) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return addLightningNode(tx, node)
	})
}

func addLightningNode(tx *bolt.Tx, node *LightningNode) error {
	nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
	if err != nil {
		return err
	}

	aliases, err := nodes.CreateBucketIfNotExists(aliasIndexBucket)
	if err != nil {
		return err
	}

	return putLightningNode(nodes, aliases, node)
}

// LookupAlias attempts to return the alias as advertised by the target node.
// TODO(roasbeef): currently assumes that aliases are unique...
func (c *ChannelGraph) LookupAlias(pub *btcec.PublicKey) (string, error) {
	var alias string

	err := c.db.View(func(tx *bolt.Tx) error {
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodesNotFound
		}

		aliases := nodes.Bucket(aliasIndexBucket)
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
	})
	if err != nil {
		return "", err
	}

	return alias, nil
}

// DeleteLightningNode removes a vertex/node from the database according to the
// node's public key.
func (c *ChannelGraph) DeleteLightningNode(nodePub *btcec.PublicKey) error {
	pub := nodePub.SerializeCompressed()

	// TODO(roasbeef): ensure dangling edges are removed...
	return c.db.Update(func(tx *bolt.Tx) error {
		nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
		if err != nil {
			return err
		}

		aliases, err := tx.CreateBucketIfNotExists(aliasIndexBucket)
		if err != nil {
			return err
		}

		if err := aliases.Delete(pub); err != nil {
			return err
		}
		return nodes.Delete(pub)
	})
}

// AddChannelEdge adds a new (undirected, blank) edge to the graph database. An
// undirected edge from the two target nodes are created. The chanPoint and
// chanID are used to uniquely identify the edge globally within the database.
func (c *ChannelGraph) AddChannelEdge(from, to *btcec.PublicKey,
	chanPoint *wire.OutPoint, chanID uint64) error {

	// Construct the channel's primary key which is the 8-byte channel ID.
	var chanKey [8]byte
	binary.BigEndian.PutUint64(chanKey[:], chanID)

	var (
		node1 []byte
		node2 []byte
	)

	fromBytes := from.SerializeCompressed()
	toBytes := to.SerializeCompressed()

	// On-disk, we order the value for the edge's key with the "smaller"
	// pubkey coming before the larger one. This ensures that all edges
	// have a deterministic ordering.
	if bytes.Compare(fromBytes, toBytes) == -1 {
		node1 = fromBytes
		node2 = toBytes
	} else {
		node1 = toBytes
		node2 = fromBytes
	}

	return c.db.Update(func(tx *bolt.Tx) error {
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
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

		// First, attempt to check if this edge has already been
		// created. If so, then we can exit early as this method is
		// meant to be idempotent.
		if edgeInfo := edgeIndex.Get(chanKey[:]); edgeInfo != nil {
			return nil
		}

		// If the edge hasn't been created yet, then we'll first add it
		// to the edge index in order to associate the edge between two
		// nodes.
		var edgeInfo [66]byte
		copy(edgeInfo[:33], node1)
		copy(edgeInfo[33:], node2)
		if err := edgeIndex.Put(chanKey[:], edgeInfo[:]); err != nil {
			return err
		}

		// Finally we add it to the channel index which maps channel
		// points (outpoints) to the shorter channel ID's.
		var b bytes.Buffer
		if err := writeOutpoint(&b, chanPoint); err != nil {
			return err
		}
		return chanIndex.Put(b.Bytes(), chanKey[:])
	})
}

// HasChannelEdge returns true if the database knows of a channel edge with the
// passed channel ID, and false otherwise. If the an edge with that ID is found
// within the graph, then two time stamps representing the last time the edge
// was updated for both directed edges are returned along with the boolean.
func (c *ChannelGraph) HasChannelEdge(chanID uint64) (time.Time, time.Time, bool, error) {
	// TODO(roasbeef): check internal bloom filter first

	var (
		node1UpdateTime time.Time
		node2UpdateTime time.Time
		exists          bool
	)

	if err := c.db.View(func(tx *bolt.Tx) error {
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		var channelID [8]byte
		byteOrder.PutUint64(channelID[:], chanID)
		if edgeIndex.Get(channelID[:]) == nil {
			exists = false
			return nil
		}

		exists = true

		// If the channel has been found in the graph, then retrieve
		// the edges itself so we can return the last updated
		// timestmaps.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNodeNotFound
		}

		e1, e2, err := fetchEdges(edgeIndex, edges, nodes,
			channelID[:], c.db)
		if err != nil {
			return err
		}

		// As we may have only one of the edges populated, only set the
		// update time if the edge was found in the database.
		if e1 != nil {
			node1UpdateTime = e1.LastUpdate
		}
		if e2 != nil {
			node2UpdateTime = e2.LastUpdate
		}

		return nil
	}); err != nil {
		return time.Time{}, time.Time{}, exists, err
	}

	return node1UpdateTime, node2UpdateTime, exists, nil
}

const (
	// pruneTipBytes is the total size of the value which stores the
	// current prune tip of the graph. The prune tip indicates if the
	// channel graph is in sync with the current UTXO state. The structure
	// is: blockHash || blockHeight, taking 36 bytes total.
	pruneTipBytes = 32 + 4
)

// PruneGraph prunes newly closed channels from the channel graph in response
// to a new block being solved on the network. Any transactions which spend the
// funding output of any known channels within he graph will be deleted.
// Additionally, the "prune tip", or the last block which has been used to
// prune the graph is stored so callers can ensure the graph is fully in sync
// with the current UTXO state. An integer is returned which reflects the
// number of channels pruned due to the new incoming block.
func (c *ChannelGraph) PruneGraph(spentOutputs []*wire.OutPoint,
	blockHash *chainhash.Hash, blockHeight uint32) (uint32, error) {

	var numChans uint32

	err := c.db.Update(func(tx *bolt.Tx) error {
		// First grab the edges bucket which houses the information
		// we'd like to delete
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
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

		// For each of the outpoints that've been spent within the
		// block, we attempt to delete them from the graph as if that
		// outpoint was a channel, then it has now been closed.
		for _, chanPoint := range spentOutputs {
			// TODO(roasbeef): load channel bloom filter, continue
			// if NOT if filter

			// Attempt to delete the channel, and ErrEdgeNotFound
			// will be returned if that outpoint isn't known to be
			// a channel. If no error is returned, then a channel
			// was successfully pruned.
			err := delChannelByEdge(edges, edgeIndex, chanIndex,
				chanPoint)
			if err != nil && err != ErrEdgeNotFound {
				return err
			} else if err == nil {
				numChans += 1
			}
		}

		metaBucket, err := tx.CreateBucketIfNotExists(graphMetaBucket)
		if err != nil {
			return err
		}

		// With the graph pruned, update the current "prune tip" which
		// can eb used to check if the graph is fully synced with the
		// current UTXO state.
		var newTip [pruneTipBytes]byte
		copy(newTip[:], blockHash[:])
		byteOrder.PutUint32(newTip[32:], uint32(blockHeight))

		return metaBucket.Put(pruneTipKey, newTip[:])
	})
	if err != nil {
		return 0, err
	}

	return numChans, nil
}

// PruneTip returns the block height and hash of the latest block that has been
// used to prune channels in the graph. Knowing the "prune tip" allows callers
// to tell if the graph is currently in sync with the current best known UTXO
// state.
func (c *ChannelGraph) PruneTip() (*chainhash.Hash, uint32, error) {
	var (
		currentTip [pruneTipBytes]byte
		tipHash    chainhash.Hash
		tipHeight  uint32
	)

	err := c.db.View(func(tx *bolt.Tx) error {
		graphMeta := tx.Bucket(graphMetaBucket)
		if graphMeta == nil {
			return ErrGraphNotFound
		}

		tipBytes := graphMeta.Get(pruneTipKey)
		if tipBytes == nil {
			return ErrGraphNeverPruned
		}
		copy(currentTip[:], tipBytes)

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Once we have the prune tip, the first 32 bytes are the block hash,
	// with the latter 4 bytes being the block height.
	copy(tipHash[:], currentTip[:32])
	tipHeight = byteOrder.Uint32(currentTip[32:])

	return &tipHash, tipHeight, nil
}

// DeleteChannelEdge removes an edge from the database as identified by it's
// funding outpoint. If the edge does not exist within the database, then this
func (c *ChannelGraph) DeleteChannelEdge(chanPoint *wire.OutPoint) error {
	// TODO(roasbeef): possibly delete from node bucket if node has no more
	// channels
	// TODO(roasbeef): don't delete both edges?

	return c.db.Update(func(tx *bolt.Tx) error {
		// First grab the edges bucket which houses the information
		// we'd like to delete
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
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

		return delChannelByEdge(edges, edgeIndex, chanIndex, chanPoint)
	})
}

// ChannelID attempt to lookup the 8-byte compact channel ID which maps to the
// passed channel point (outpoint). If the passed channel doesn't exist within
// the database, then ErrEdgeNotFound is returned.
func (c *ChannelGraph) ChannelID(chanPoint *wire.OutPoint) (uint64, error) {
	var chanID uint64

	var b bytes.Buffer
	if err := writeOutpoint(&b, chanPoint); err != nil {
		return 0, nil
	}

	if err := c.db.View(func(tx *bolt.Tx) error {
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		chanIndex := edges.Bucket(channelPointBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}

		chanIDBytes := chanIndex.Get(b.Bytes())
		if chanIDBytes == nil {
			return ErrEdgeNotFound
		}

		chanID = byteOrder.Uint64(chanIDBytes)

		return nil
	}); err != nil {
		return 0, err
	}

	return chanID, nil
}

func delChannelByEdge(edges *bolt.Bucket, edgeIndex *bolt.Bucket,
	chanIndex *bolt.Bucket, chanPoint *wire.OutPoint) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, chanPoint); err != nil {
		return err
	}

	// If the channel's outpoint doesn't exist within the outpoint
	// index, then the edge does not exist.
	chanID := chanIndex.Get(b.Bytes())
	if chanID == nil {
		return ErrEdgeNotFound
	}

	// Otherwise we obtain the two public keys from the mapping:
	// chanID -> pubKey1 || pubKey2. With this, we can construct
	// the keys which house both of the directed edges for this
	// channel.
	nodeKeys := edgeIndex.Get(chanID)

	// The edge key is of the format pubKey || chanID. First we
	// construct the latter half, populating the channel ID.
	var edgeKey [33 + 8]byte
	copy(edgeKey[33:], chanID)

	// With the latter half constructed, copy over the first public
	// key to delete the edge in this direction, then the second to
	// delete the edge in the opposite direction.
	copy(edgeKey[:33], nodeKeys[:33])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return err
		}
	}
	copy(edgeKey[:33], nodeKeys[33:])
	if edges.Get(edgeKey[:]) != nil {
		if err := edges.Delete(edgeKey[:]); err != nil {
			return err
		}
	}

	// Finally, with the edge data deleted, we can purge the
	// information from the two edge indexes.
	if err := edgeIndex.Delete(chanID); err != nil {
		return err
	}
	return chanIndex.Delete(b.Bytes())
}

// UpdateEdgeInfo updates the edge information for a single directed edge
// within the database for the referenced channel. The `flags` attribute within
// the ChannelEdge determines which of the directed edges are being updated. If
// the flag is 1, then the first node's information is being updated, otherwise
// it's the second node's information.
func (r *ChannelGraph) UpdateEdgeInfo(edge *ChannelEdge) error {

	return r.db.Update(func(tx *bolt.Tx) error {
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}

		// Create the channelID key be converting the channel ID
		// integer into a byte slice.
		var chanID [8]byte
		byteOrder.PutUint64(chanID[:], edge.ChannelID)

		// With the channel ID, we then fetch the value storing the two
		// nodes which connect this channel edge.
		nodeInfo := edgeIndex.Get(chanID[:])
		if nodeInfo == nil {
			return ErrEdgeNotFound
		}

		// Depending on the flags value parsed above, either the first
		// or second node is being updated.
		var fromNode, toNode []byte
		if edge.Flags == 0 {
			fromNode = nodeInfo[:33]
			toNode = nodeInfo[33:]
		} else {
			fromNode = nodeInfo[33:]
			toNode = nodeInfo[:33]
		}

		// Finally, with the direction of the edge being updated
		// identified, we update the on-disk edge representation.
		return putChannelEdge(edges, edge, fromNode, toNode)
	})
}

// LightningNode represents an individual vertex/node within the channel graph.
// A node is connected to other nodes by one or more channel edges emanating
// from it. As the graph is directed, a node will also have an incoming edge
// attached to it for each outgoing edge.
type LightningNode struct {
	// LastUpdate is the last time the vertex information for this node has
	// been updated.
	LastUpdate time.Time

	// Address is the TCP address this node is reachable over.
	Address *net.TCPAddr

	// PubKey is the node's long-term identity public key. This key will be
	// used to authenticated any advertisements/updates sent by the node.
	PubKey *btcec.PublicKey

	// Color is the selected color for the node.
	Color color.RGBA

	// Alias is a nick-name for the node. The alias can be used to confirm
	// a node's identity or to serve as a short ID for an address book.
	Alias string

	db *DB

	// TODO(roasbeef): discovery will need storage to keep it's last IP
	// address and re-announce if interface changes?

	// TODO(roasbeef): add update method and fetch?
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. If the node iwn't found in the database, then ErrGraphNodeNotFound is
// returned.
func (c *ChannelGraph) FetchLightningNode(pub *btcec.PublicKey) (*LightningNode, error) {
	var node *LightningNode
	nodePub := pub.SerializeCompressed()
	err := c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// If a key for this serialized public key isn't found, then
		// the target node doesn't exist within the database.
		nodeBytes := nodes.Get(nodePub)
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

		node = n

		return nil
	})
	if err != nil {
		return nil, err
	}

	return node, nil
}

// HasLightningNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean.
func (c *ChannelGraph) HasLightningNode(pub *btcec.PublicKey) (time.Time, bool, error) {
	var (
		updateTime time.Time
		exists     bool
	)

	nodePub := pub.SerializeCompressed()
	err := c.db.View(func(tx *bolt.Tx) error {
		// First grab the nodes bucket which stores the mapping from
		// pubKey to node information.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// If a key for this serialized public key isn't found, we can
		// exit early.
		nodeBytes := nodes.Get(nodePub)
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
	})
	if err != nil {
		return time.Time{}, exists, nil
	}

	return updateTime, exists, nil
}

// ForEachChannel iterates through all the outgoing channel edges from this
// node, executing the passed callback with each edge as its sole argument. If
// the callback returns an error, then the iteration is halted with the error
// propagated back up to the caller. If the caller wishes to re-use an existing
// boltdb transaction, then it should be passed as the first argument.
// Otherwise the first argument should be nil and a fresh transaction will be
// created to execute the graph traversal.
func (l *LightningNode) ForEachChannel(tx *bolt.Tx, cb func(*ChannelEdge) error) error {
	// TODO(roasbeef): remove the option to pass in a transaction after
	// all?
	nodePub := l.PubKey.SerializeCompressed()

	traversal := func(tx *bolt.Tx) error {
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNotFound
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
		edgeCursor := edges.Cursor()
		for nodeEdge, edgeInfo := edgeCursor.Seek(nodeStart[:]); bytes.HasPrefix(nodeEdge, nodePub); nodeEdge, edgeInfo = edgeCursor.Next() {
			// If the prefix still matches, then the value is the
			// raw edge information. So we can now serialize the
			// edge info and fetch the outgoing node in order to
			// retrieve the full channel edge.
			edgeReader := bytes.NewReader(edgeInfo)
			edge, err := deserializeChannelEdge(edgeReader, nodes)
			if err != nil {
				return err
			}
			edge.db = l.db
			edge.Node.db = l.db

			// Finally, we execute the callback.
			if err := cb(edge); err != nil {
				return err
			}
		}

		return nil
	}

	// If no transaction was provided, then we'll create a new transaction
	// to execute the transaction within.
	if tx == nil {
		return l.db.View(traversal)
	}

	// Otherwise, we re-use the existing transaction to execute the graph
	// traversal.
	return traversal(tx)
}

// ChannelEdge represents a *directed* edge within the channel graph. For each
// channel in the database, there are two distinct edges: one for each possible
// direction of travel along the channel. The edges themselves hold information
// concerning fees, and minimum time-lock information which is utilized during
// path finding.
type ChannelEdge struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	// TODO(roasbeef): spell out and use index of channel ID to do fast look
	// ups?
	ChannelID uint64

	// ChannelPoint is the funding outpoint of the channel. This can be
	// used to uniquely identify the channel within the channel graph.
	ChannelPoint wire.OutPoint

	// LastUpdate is the last time an authenticated edge for this channel
	// was received.
	LastUpdate time.Time

	// Flags is a bitfield which signals the capabilities of the channel as
	// well as the directe edge this update applies to.
	// TODO(roasbeef):  make into wire struct
	Flags uint16

	// Expiry is the number of blocks this node will subtract from the
	// expiry of an incoming HTLC. This value expresses the time buffer the
	// node would like to HTLC exchanges.
	Expiry uint16

	// MinHTLC is the smallest value HTLC this node will accept, expressed
	// in millisatoshi.
	MinHTLC btcutil.Amount

	// FeeBaseMSat is the base HTLC fee that will be charged for forwarding
	// ANY HTLC, expressed in mSAT's.
	FeeBaseMSat btcutil.Amount

	// FeeProportionalMillionths is the rate that the node will charge for
	// HTLCs for each millionth of a satoshi forwarded.
	FeeProportionalMillionths btcutil.Amount

	// Capacity is the total capacity of the channel, this is determined by
	// the value output in the outpoint that created this channel.
	Capacity btcutil.Amount

	// Node is the LightningNode that this directed edge leads to. Using
	// this pointer the channel graph can further be traversed.
	Node *LightningNode

	db *DB
}

// FetchChannelEdgesByOutpoint attempts to lookup the two directed edges for
// the channel identified by the funding outpoint. If the channel can't be
// found, then ErrEdgeNotFound is returned.
func (c *ChannelGraph) FetchChannelEdgesByOutpoint(op *wire.OutPoint) (*ChannelEdge, *ChannelEdge, error) {
	var (
		edge1 *ChannelEdge
		edge2 *ChannelEdge
	)

	err := c.db.Update(func(tx *bolt.Tx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes, err := tx.CreateBucketIfNotExists(nodeBucket)
		if err != nil {
			return err
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges, err := tx.CreateBucketIfNotExists(edgeBucket)
		if err != nil {
			return err
		}
		edgeIndex, err := edges.CreateBucketIfNotExists(edgeIndexBucket)
		if err != nil {
			return err
		}

		// If the channel's outpoint doesn't exist within the outpoint
		// index, then the edge does not exist.
		chanIndex, err := edges.CreateBucketIfNotExists(channelPointBucket)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := writeOutpoint(&b, op); err != nil {
			return err
		}
		chanID := chanIndex.Get(b.Bytes())
		if chanID == nil {
			return ErrEdgeNotFound
		}

		e1, e2, err := fetchEdges(edgeIndex, edges, nodes, chanID, c.db)
		if err != nil {
			return err
		}

		edge1 = e1
		edge2 = e2
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return edge1, edge2, nil
}

// FetchChannelEdgesByID attempts to lookup the two directed edges for the
// channel identified by the channel ID. If the channel can't be found, then
// ErrEdgeNotFound is returned.
func (c *ChannelGraph) FetchChannelEdgesByID(chanID uint64) (*ChannelEdge, *ChannelEdge, error) {
	var (
		edge1     *ChannelEdge
		edge2     *ChannelEdge
		channelID [8]byte
	)

	err := c.db.View(func(tx *bolt.Tx) error {
		// First, grab the node bucket. This will be used to populate
		// the Node pointers in each edge read from disk.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		// Next, grab the edge bucket which stores the edges, and also
		// the index itself so we can group the directed edges together
		// logically.
		edges := tx.Bucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		edgeIndex := edges.Bucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrGraphNoEdgesFound
		}

		byteOrder.PutUint64(channelID[:], chanID)
		e1, e2, err := fetchEdges(edgeIndex, edges, nodes,
			channelID[:], c.db)
		if err != nil {
			return err
		}

		edge1 = e1
		edge2 = e2
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return edge1, edge2, nil
}

// NewChannelEdge returns a new blank ChannelEdge.
func (c *ChannelGraph) NewChannelEdge() *ChannelEdge {
	return &ChannelEdge{db: c.db}
}

func putLightningNode(nodeBucket *bolt.Bucket, aliasBucket *bolt.Bucket, node *LightningNode) error {
	var (
		scratch [8]byte
		b       bytes.Buffer
	)

	nodePub := node.PubKey.SerializeCompressed()

	if err := aliasBucket.Put(nodePub, []byte(node.Alias)); err != nil {
		return err
	}

	updateUnix := uint64(node.LastUpdate.Unix())
	byteOrder.PutUint64(scratch[:], updateUnix)
	if _, err := b.Write(scratch[:]); err != nil {
		return err
	}

	addrString := node.Address.String()
	if err := wire.WriteVarString(&b, 0, addrString); err != nil {
		return err
	}

	if _, err := b.Write(nodePub); err != nil {
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

	return nodeBucket.Put(nodePub, b.Bytes())
}

func fetchLightningNode(nodeBucket *bolt.Bucket,
	nodePub []byte) (*LightningNode, error) {

	nodeBytes := nodeBucket.Get(nodePub)
	if nodeBytes == nil {
		return nil, ErrGraphNodesNotFound
	}

	nodeReader := bytes.NewReader(nodeBytes)
	return deserializeLightningNode(nodeReader)
}

func deserializeLightningNode(r io.Reader) (*LightningNode, error) {
	node := &LightningNode{}

	var scratch [8]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}

	unix := int64(byteOrder.Uint64(scratch[:]))
	node.LastUpdate = time.Unix(unix, 0)

	addrString, err := wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}
	node.Address, err = net.ResolveTCPAddr("tcp", addrString)
	if err != nil {
		return nil, err
	}

	var pub [33]byte
	if _, err := r.Read(pub[:]); err != nil {
		return nil, err
	}
	node.PubKey, err = btcec.ParsePubKey(pub[:], btcec.S256())
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &node.Color.R); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &node.Color.G); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &node.Color.B); err != nil {
		return nil, err
	}

	node.Alias, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	return node, nil
}

func putChannelEdge(edges *bolt.Bucket, edge *ChannelEdge, from, to []byte) error {
	var edgeKey [33 + 8]byte
	copy(edgeKey[:], from)
	byteOrder.PutUint64(edgeKey[33:], edge.ChannelID)

	var b bytes.Buffer

	if err := binary.Write(&b, byteOrder, edge.ChannelID); err != nil {
		return err
	}

	if err := writeOutpoint(&b, &edge.ChannelPoint); err != nil {
		return err
	}

	var scratch [8]byte
	updateUnix := uint64(edge.LastUpdate.Unix())
	byteOrder.PutUint64(scratch[:], updateUnix)
	if _, err := b.Write(scratch[:]); err != nil {
		return err
	}

	if err := binary.Write(&b, byteOrder, edge.Flags); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, edge.Expiry); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.MinHTLC)); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.FeeBaseMSat)); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.FeeProportionalMillionths)); err != nil {
		return err
	}
	if err := binary.Write(&b, byteOrder, uint64(edge.Capacity)); err != nil {
		return err
	}

	if _, err := b.Write(to); err != nil {
		return err
	}

	return edges.Put(edgeKey[:], b.Bytes()[:])
}

func fetchEdges(edgeIndex *bolt.Bucket, edges *bolt.Bucket, nodes *bolt.Bucket,
	chanID []byte, db *DB) (*ChannelEdge, *ChannelEdge, error) {

	edgeInfo := edgeIndex.Get(chanID)
	if edgeIndex == nil {
		return nil, nil, ErrEdgeNotFound
	}

	// The first node is contained within the first half of the
	// edge information. We only propgate the error here and below if it's
	// something other than edge non-existance.
	node1Pub := edgeInfo[:33]
	edge1, err := fetchChannelEdge(edges, chanID, node1Pub, nodes)
	if err != nil && err != ErrEdgeNotFound {
		return nil, nil, err
	}

	// As we may have a signle direction of the edge but not the other,
	// only fill in the datbase pointers if the edge is found.
	if edge1 != nil {
		edge1.db = db
		edge1.Node.db = db
	}

	// Similarly, the second node is contained within the latter
	// half of the edge information.
	node2Pub := edgeInfo[33:]
	edge2, err := fetchChannelEdge(edges, chanID, node2Pub, nodes)
	if err != nil && err != ErrEdgeNotFound {
		return nil, nil, err
	}

	if edge2 != nil {
		edge2.db = db
		edge2.Node.db = db
	}

	return edge1, edge2, nil
}

func fetchChannelEdge(edges *bolt.Bucket, chanID []byte,
	nodePub []byte, nodes *bolt.Bucket) (*ChannelEdge, error) {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], nodePub)
	copy(edgeKey[33:], chanID[:])

	edgeBytes := edges.Get(edgeKey[:])
	if edgeBytes == nil {
		return nil, ErrEdgeNotFound
	}

	edgeReader := bytes.NewReader(edgeBytes)

	return deserializeChannelEdge(edgeReader, nodes)
}

func deserializeChannelEdge(r io.Reader, nodes *bolt.Bucket) (*ChannelEdge, error) {
	edge := &ChannelEdge{}

	if err := binary.Read(r, byteOrder, &edge.ChannelID); err != nil {
		return nil, err
	}

	edge.ChannelPoint = wire.OutPoint{}
	if err := readOutpoint(r, &edge.ChannelPoint); err != nil {
		return nil, err
	}

	var scratch [8]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	unix := int64(byteOrder.Uint64(scratch[:]))
	edge.LastUpdate = time.Unix(unix, 0)

	if err := binary.Read(r, byteOrder, &edge.Flags); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edge.Expiry); err != nil {
		return nil, err
	}

	var n uint64
	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.MinHTLC = btcutil.Amount(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeBaseMSat = btcutil.Amount(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeProportionalMillionths = btcutil.Amount(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.Capacity = btcutil.Amount(n)

	var pub [33]byte
	if _, err := r.Read(pub[:]); err != nil {
		return nil, err
	}

	node, err := fetchLightningNode(nodes, pub[:])
	if err != nil {
		return nil, err
	}

	edge.Node = node
	return edge, nil
}
