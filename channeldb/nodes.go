package channeldb

import (
	"bytes"
	"io"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// nodeInfoBucket stores metadata pertaining to nodes that we've had
	// direct channel-based correspondence with. This bucket allows one to
	// query for all open channels pertaining to the node by exploring each
	// node's sub-bucket within the openChanBucket.
	nodeInfoBucket = []byte("nib")
)

// LinkNode stores metadata related to node's that we have/had a direct
// channel open with. Information such as the Bitcoin network the node
// advertised, and its identity public key are also stored. Additionally, this
// struct and the bucket its stored within have store data similar to that of
// Bitcoin's addrmanager. The TCP address information stored within the struct
// can be used to establish persistent connections will all channel
// counterparties on daemon startup.
//
// TODO(roasbeef): also add current OnionKey plus rotation schedule?
// TODO(roasbeef): add bitfield for supported services
//   - possibly add a wire.NetAddress type, type
type LinkNode struct {
	// Network indicates the Bitcoin network that the LinkNode advertises
	// for incoming channel creation.
	Network wire.BitcoinNet

	// IdentityPub is the node's current identity public key. Any
	// channel/topology related information received by this node MUST be
	// signed by this public key.
	IdentityPub *btcec.PublicKey

	// LastSeen tracks the last time this node was seen within the network.
	// A node should be marked as seen if the daemon either is able to
	// establish an outgoing connection to the node or receives a new
	// incoming connection from the node. This timestamp (stored in unix
	// epoch) may be used within a heuristic which aims to determine when a
	// channel should be unilaterally closed due to inactivity.
	//
	// TODO(roasbeef): replace with block hash/height?
	//  * possibly add a time-value metric into the heuristic?
	LastSeen time.Time

	// Addresses is a list of IP address in which either we were able to
	// reach the node over in the past, OR we received an incoming
	// authenticated connection for the stored identity public key.
	Addresses []net.Addr

	// db is the database instance this node was fetched from. This is used
	// to sync back the node's state if it is updated.
	db *LinkNodeDB
}

// NewLinkNode creates a new LinkNode from the provided parameters, which is
// backed by an instance of a link node DB.
func NewLinkNode(db *LinkNodeDB, bitNet wire.BitcoinNet, pub *btcec.PublicKey,
	addrs ...net.Addr) *LinkNode {

	return &LinkNode{
		Network:     bitNet,
		IdentityPub: pub,
		LastSeen:    time.Now(),
		Addresses:   addrs,
		db:          db,
	}
}

// UpdateLastSeen updates the last time this node was directly encountered on
// the Lightning Network.
func (l *LinkNode) UpdateLastSeen(lastSeen time.Time) error {
	l.LastSeen = lastSeen

	return l.Sync()
}

// AddAddress appends the specified TCP address to the list of known addresses
// this node is/was known to be reachable at.
func (l *LinkNode) AddAddress(addr net.Addr) error {
	for _, a := range l.Addresses {
		if a.String() == addr.String() {
			return nil
		}
	}

	l.Addresses = append(l.Addresses, addr)

	return l.Sync()
}

// Sync performs a full database sync which writes the current up-to-date data
// within the struct to the database.
func (l *LinkNode) Sync() error {
	// Finally update the database by storing the link node and updating
	// any relevant indexes.
	return kvdb.Update(l.db.backend, func(tx kvdb.RwTx) error {
		nodeMetaBucket := tx.ReadWriteBucket(nodeInfoBucket)
		if nodeMetaBucket == nil {
			return ErrLinkNodesNotFound
		}

		return putLinkNode(nodeMetaBucket, l)
	}, func() {})
}

// putLinkNode serializes then writes the encoded version of the passed link
// node into the nodeMetaBucket. This function is provided in order to allow
// the ability to re-use a database transaction across many operations.
func putLinkNode(nodeMetaBucket kvdb.RwBucket, l *LinkNode) error {
	// First serialize the LinkNode into its raw-bytes encoding.
	var b bytes.Buffer
	if err := serializeLinkNode(&b, l); err != nil {
		return err
	}

	// Finally insert the link-node into the node metadata bucket keyed
	// according to the its pubkey serialized in compressed form.
	nodePub := l.IdentityPub.SerializeCompressed()
	return nodeMetaBucket.Put(nodePub, b.Bytes())
}

// LinkNodeDB is a database that keeps track of all link nodes.
type LinkNodeDB struct {
	backend kvdb.Backend
}

// DeleteLinkNode removes the link node with the given identity from the
// database.
func (l *LinkNodeDB) DeleteLinkNode(identity *btcec.PublicKey) error {
	return kvdb.Update(l.backend, func(tx kvdb.RwTx) error {
		return deleteLinkNode(tx, identity)
	}, func() {})
}

func deleteLinkNode(tx kvdb.RwTx, identity *btcec.PublicKey) error {
	nodeMetaBucket := tx.ReadWriteBucket(nodeInfoBucket)
	if nodeMetaBucket == nil {
		return ErrLinkNodesNotFound
	}

	pubKey := identity.SerializeCompressed()
	return nodeMetaBucket.Delete(pubKey)
}

// FetchLinkNode attempts to lookup the data for a LinkNode based on a target
// identity public key. If a particular LinkNode for the passed identity public
// key cannot be found, then ErrNodeNotFound if returned.
func (l *LinkNodeDB) FetchLinkNode(identity *btcec.PublicKey) (*LinkNode, error) {
	var linkNode *LinkNode
	err := kvdb.View(l.backend, func(tx kvdb.RTx) error {
		node, err := fetchLinkNode(tx, identity)
		if err != nil {
			return err
		}

		linkNode = node
		return nil
	}, func() {
		linkNode = nil
	})

	return linkNode, err
}

func fetchLinkNode(tx kvdb.RTx, targetPub *btcec.PublicKey) (*LinkNode, error) {
	// First fetch the bucket for storing node metadata, bailing out early
	// if it hasn't been created yet.
	nodeMetaBucket := tx.ReadBucket(nodeInfoBucket)
	if nodeMetaBucket == nil {
		return nil, ErrLinkNodesNotFound
	}

	// If a link node for that particular public key cannot be located,
	// then exit early with an ErrNodeNotFound.
	pubKey := targetPub.SerializeCompressed()
	nodeBytes := nodeMetaBucket.Get(pubKey)
	if nodeBytes == nil {
		return nil, ErrNodeNotFound
	}

	// Finally, decode and allocate a fresh LinkNode object to be returned
	// to the caller.
	nodeReader := bytes.NewReader(nodeBytes)
	return deserializeLinkNode(nodeReader)
}

// TODO(roasbeef): update link node addrs in server upon connection

// FetchAllLinkNodes starts a new database transaction to fetch all nodes with
// whom we have active channels with.
func (l *LinkNodeDB) FetchAllLinkNodes() ([]*LinkNode, error) {
	var linkNodes []*LinkNode
	err := kvdb.View(l.backend, func(tx kvdb.RTx) error {
		nodes, err := fetchAllLinkNodes(tx)
		if err != nil {
			return err
		}

		linkNodes = nodes
		return nil
	}, func() {
		linkNodes = nil
	})
	if err != nil {
		return nil, err
	}

	return linkNodes, nil
}

// fetchAllLinkNodes uses an existing database transaction to fetch all nodes
// with whom we have active channels with.
func fetchAllLinkNodes(tx kvdb.RTx) ([]*LinkNode, error) {
	nodeMetaBucket := tx.ReadBucket(nodeInfoBucket)
	if nodeMetaBucket == nil {
		return nil, ErrLinkNodesNotFound
	}

	var linkNodes []*LinkNode
	err := nodeMetaBucket.ForEach(func(k, v []byte) error {
		if v == nil {
			return nil
		}

		nodeReader := bytes.NewReader(v)
		linkNode, err := deserializeLinkNode(nodeReader)
		if err != nil {
			return err
		}

		linkNodes = append(linkNodes, linkNode)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return linkNodes, nil
}

func serializeLinkNode(w io.Writer, l *LinkNode) error {
	var buf [8]byte

	byteOrder.PutUint32(buf[:4], uint32(l.Network))
	if _, err := w.Write(buf[:4]); err != nil {
		return err
	}

	serializedID := l.IdentityPub.SerializeCompressed()
	if _, err := w.Write(serializedID); err != nil {
		return err
	}

	seenUnix := uint64(l.LastSeen.Unix())
	byteOrder.PutUint64(buf[:], seenUnix)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}

	numAddrs := uint32(len(l.Addresses))
	byteOrder.PutUint32(buf[:4], numAddrs)
	if _, err := w.Write(buf[:4]); err != nil {
		return err
	}

	for _, addr := range l.Addresses {
		if err := graphdb.SerializeAddr(w, addr); err != nil {
			return err
		}
	}

	return nil
}

func deserializeLinkNode(r io.Reader) (*LinkNode, error) {
	var (
		err error
		buf [8]byte
	)

	node := &LinkNode{}

	if _, err := io.ReadFull(r, buf[:4]); err != nil {
		return nil, err
	}
	node.Network = wire.BitcoinNet(byteOrder.Uint32(buf[:4]))

	var pub [33]byte
	if _, err := io.ReadFull(r, pub[:]); err != nil {
		return nil, err
	}
	node.IdentityPub, err = btcec.ParsePubKey(pub[:])
	if err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	node.LastSeen = time.Unix(int64(byteOrder.Uint64(buf[:])), 0)

	if _, err := io.ReadFull(r, buf[:4]); err != nil {
		return nil, err
	}
	numAddrs := byteOrder.Uint32(buf[:4])

	node.Addresses = make([]net.Addr, numAddrs)
	for i := uint32(0); i < numAddrs; i++ {
		addr, err := graphdb.DeserializeAddr(r)
		if err != nil {
			return nil, err
		}
		node.Addresses[i] = addr
	}

	return node, nil
}
