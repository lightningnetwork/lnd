package autopilot

import (
	"bytes"
	"math/big"
	"net"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	testSig = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	chanIDCounter uint64 // To be used atomically.
)

// databaseChannelGraph wraps a channeldb.ChannelGraph instance with the
// necessary API to properly implement the autopilot.ChannelGraph interface.
//
// TODO(roasbeef): move inmpl to main package?
type databaseChannelGraph struct {
	db *channeldb.ChannelGraph
}

// A compile time assertion to ensure databaseChannelGraph meets the
// autopilot.ChannelGraph interface.
var _ ChannelGraph = (*databaseChannelGraph)(nil)

// ChannelGraphFromDatabase returns an instance of the autopilot.ChannelGraph
// backed by a live, open channeldb instance.
func ChannelGraphFromDatabase(db *channeldb.ChannelGraph) ChannelGraph {
	return &databaseChannelGraph{
		db: db,
	}
}

// type dbNode is a wrapper struct around a database transaction an
// channeldb.LightningNode. The wrapper method implement the autopilot.Node
// interface.
type dbNode struct {
	tx *bbolt.Tx

	node *channeldb.LightningNode
}

// A compile time assertion to ensure dbNode meets the autopilot.Node
// interface.
var _ Node = (*dbNode)(nil)

// PubKey is the identity public key of the node. This will be used to attempt
// to target a node for channel opening by the main autopilot agent. The key
// will be returned in serialized compressed format.
//
// NOTE: Part of the autopilot.Node interface.
func (d dbNode) PubKey() [33]byte {
	return d.node.PubKeyBytes
}

// Addrs returns a slice of publicly reachable public TCP addresses that the
// peer is known to be listening on.
//
// NOTE: Part of the autopilot.Node interface.
func (d dbNode) Addrs() []net.Addr {
	return d.node.Addresses
}

// ForEachChannel is a higher-order function that will be used to iterate
// through all edges emanating from/to the target node. For each active
// channel, this function should be called with the populated ChannelEdge that
// describes the active channel.
//
// NOTE: Part of the autopilot.Node interface.
func (d dbNode) ForEachChannel(cb func(ChannelEdge) error) error {
	return d.node.ForEachChannel(d.tx, func(tx *bbolt.Tx,
		ei *channeldb.ChannelEdgeInfo, ep, _ *channeldb.ChannelEdgePolicy) error {

		// Skip channels for which no outgoing edge policy is available.
		//
		// TODO(joostjager): Ideally the case where channels have a nil
		// policy should be supported, as autopilot is not looking at
		// the policies. For now, it is not easily possible to get a
		// reference to the other end LightningNode object without
		// retrieving the policy.
		if ep == nil {
			return nil
		}

		edge := ChannelEdge{
			Channel: Channel{
				ChanID:    lnwire.NewShortChanIDFromInt(ep.ChannelID),
				Capacity:  ei.Capacity,
				FundedAmt: ei.Capacity,
				Node:      NodeID(ep.Node.PubKeyBytes),
			},
			Peer: dbNode{
				tx:   tx,
				node: ep.Node,
			},
		}

		return cb(edge)
	})
}

// ForEachNode is a higher-order function that should be called once for each
// connected node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (d *databaseChannelGraph) ForEachNode(cb func(Node) error) error {
	return d.db.ForEachNode(nil, func(tx *bbolt.Tx, n *channeldb.LightningNode) error {

		// We'll skip over any node that doesn't have any advertised
		// addresses. As we won't be able to reach them to actually
		// open any channels.
		if len(n.Addresses) == 0 {
			return nil
		}

		node := dbNode{
			tx:   tx,
			node: n,
		}
		return cb(node)
	})
}

// addRandChannel creates a new channel two target nodes. This function is
// meant to aide in the generation of random graphs for use within test cases
// the exercise the autopilot package.
func (d *databaseChannelGraph) addRandChannel(node1, node2 *btcec.PublicKey,
	capacity btcutil.Amount) (*ChannelEdge, *ChannelEdge, error) {

	fetchNode := func(pub *btcec.PublicKey) (*channeldb.LightningNode, error) {
		if pub != nil {
			dbNode, err := d.db.FetchLightningNode(pub)
			switch {
			case err == channeldb.ErrGraphNodeNotFound:
				fallthrough
			case err == channeldb.ErrGraphNotFound:
				graphNode := &channeldb.LightningNode{
					HaveNodeAnnouncement: true,
					Addresses: []net.Addr{
						&net.TCPAddr{
							IP: bytes.Repeat([]byte("a"), 16),
						},
					},
					Features: lnwire.NewFeatureVector(nil,
						lnwire.GlobalFeatures),
					AuthSigBytes: testSig.Serialize(),
				}
				graphNode.AddPubKey(pub)
				if err := d.db.AddLightningNode(graphNode); err != nil {
					return nil, err
				}
			case err != nil:
				return nil, err
			}

			return dbNode, nil
		}

		nodeKey, err := randKey()
		if err != nil {
			return nil, err
		}
		dbNode := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			Addresses: []net.Addr{
				&net.TCPAddr{
					IP: bytes.Repeat([]byte("a"), 16),
				},
			},
			Features:     lnwire.NewFeatureVector(nil, lnwire.GlobalFeatures),
			AuthSigBytes: testSig.Serialize(),
		}
		dbNode.AddPubKey(nodeKey)
		if err := d.db.AddLightningNode(dbNode); err != nil {
			return nil, err
		}

		return dbNode, nil
	}

	vertex1, err := fetchNode(node1)
	if err != nil {
		return nil, nil, err
	}

	vertex2, err := fetchNode(node2)
	if err != nil {
		return nil, nil, err
	}

	var lnNode1, lnNode2 *btcec.PublicKey
	if bytes.Compare(vertex1.PubKeyBytes[:], vertex2.PubKeyBytes[:]) == -1 {
		lnNode1, _ = vertex1.PubKey()
		lnNode2, _ = vertex2.PubKey()
	} else {
		lnNode1, _ = vertex2.PubKey()
		lnNode2, _ = vertex1.PubKey()
	}

	chanID := randChanID()
	edge := &channeldb.ChannelEdgeInfo{
		ChannelID: chanID.ToUint64(),
		Capacity:  capacity,
	}
	edge.AddNodeKeys(lnNode1, lnNode2, lnNode1, lnNode2)
	if err := d.db.AddChannelEdge(edge); err != nil {
		return nil, nil, err
	}
	edgePolicy := &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID.ToUint64(),
		LastUpdate:                time.Now(),
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
		Flags:                     0,
	}

	if err := d.db.UpdateEdgePolicy(edgePolicy); err != nil {
		return nil, nil, err
	}
	edgePolicy = &channeldb.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID.ToUint64(),
		LastUpdate:                time.Now(),
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
		Flags:                     1,
	}
	if err := d.db.UpdateEdgePolicy(edgePolicy); err != nil {
		return nil, nil, err
	}

	return &ChannelEdge{
			Channel: Channel{
				ChanID:   chanID,
				Capacity: capacity,
			},
			Peer: dbNode{
				node: vertex1,
			},
		},
		&ChannelEdge{
			Channel: Channel{
				ChanID:   chanID,
				Capacity: capacity,
			},
			Peer: dbNode{
				node: vertex2,
			},
		},
		nil
}

func (d *databaseChannelGraph) addRandNode() (*btcec.PublicKey, error) {
	nodeKey, err := randKey()
	if err != nil {
		return nil, err
	}
	dbNode := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		Addresses: []net.Addr{
			&net.TCPAddr{
				IP: bytes.Repeat([]byte("a"), 16),
			},
		},
		Features:     lnwire.NewFeatureVector(nil, lnwire.GlobalFeatures),
		AuthSigBytes: testSig.Serialize(),
	}
	dbNode.AddPubKey(nodeKey)
	if err := d.db.AddLightningNode(dbNode); err != nil {
		return nil, err
	}

	return nodeKey, nil

}

// memChannelGraph is an implementation of the autopilot.ChannelGraph backed by
// an in-memory graph.
type memChannelGraph struct {
	graph map[NodeID]memNode
}

// A compile time assertion to ensure memChannelGraph meets the
// autopilot.ChannelGraph interface.
var _ ChannelGraph = (*memChannelGraph)(nil)

// newMemChannelGraph creates a new blank in-memory channel graph
// implementation.
func newMemChannelGraph() *memChannelGraph {
	return &memChannelGraph{
		graph: make(map[NodeID]memNode),
	}
}

// ForEachNode is a higher-order function that should be called once for each
// connected node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (m memChannelGraph) ForEachNode(cb func(Node) error) error {
	for _, node := range m.graph {
		if err := cb(node); err != nil {
			return err
		}
	}

	return nil
}

// randChanID generates a new random channel ID.
func randChanID() lnwire.ShortChannelID {
	id := atomic.AddUint64(&chanIDCounter, 1)
	return lnwire.NewShortChanIDFromInt(id)
}

// randKey returns a random public key.
func randKey() (*btcec.PublicKey, error) {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	return priv.PubKey(), nil
}

// addRandChannel creates a new channel two target nodes. This function is
// meant to aide in the generation of random graphs for use within test cases
// the exercise the autopilot package.
func (m *memChannelGraph) addRandChannel(node1, node2 *btcec.PublicKey,
	capacity btcutil.Amount) (*ChannelEdge, *ChannelEdge, error) {

	var (
		vertex1, vertex2 memNode
		ok               bool
	)

	if node1 != nil {
		vertex1, ok = m.graph[NewNodeID(node1)]
		if !ok {
			vertex1 = memNode{
				pub: node1,
				addrs: []net.Addr{
					&net.TCPAddr{
						IP: bytes.Repeat([]byte("a"), 16),
					},
				},
			}
		}
	} else {
		newPub, err := randKey()
		if err != nil {
			return nil, nil, err
		}
		vertex1 = memNode{
			pub: newPub,
			addrs: []net.Addr{
				&net.TCPAddr{
					IP: bytes.Repeat([]byte("a"), 16),
				},
			},
		}
	}

	if node2 != nil {
		vertex2, ok = m.graph[NewNodeID(node2)]
		if !ok {
			vertex2 = memNode{
				pub: node2,
				addrs: []net.Addr{
					&net.TCPAddr{
						IP: bytes.Repeat([]byte("a"), 16),
					},
				},
			}
		}
	} else {
		newPub, err := randKey()
		if err != nil {
			return nil, nil, err
		}
		vertex2 = memNode{
			pub: newPub,
			addrs: []net.Addr{
				&net.TCPAddr{
					IP: bytes.Repeat([]byte("a"), 16),
				},
			},
		}
	}

	channel := Channel{
		ChanID:   randChanID(),
		Capacity: capacity,
	}

	edge1 := ChannelEdge{
		Channel: channel,
		Peer:    vertex2,
	}
	vertex1.chans = append(vertex1.chans, edge1)

	edge2 := ChannelEdge{
		Channel: channel,
		Peer:    vertex1,
	}
	vertex2.chans = append(vertex2.chans, edge2)

	m.graph[NewNodeID(vertex1.pub)] = vertex1
	m.graph[NewNodeID(vertex2.pub)] = vertex2

	return &edge1, &edge2, nil
}

func (m *memChannelGraph) addRandNode() (*btcec.PublicKey, error) {
	newPub, err := randKey()
	if err != nil {
		return nil, err
	}
	vertex := memNode{
		pub: newPub,
		addrs: []net.Addr{
			&net.TCPAddr{
				IP: bytes.Repeat([]byte("a"), 16),
			},
		},
	}
	m.graph[NewNodeID(newPub)] = vertex

	return newPub, nil
}

// memNode is a purely in-memory implementation of the autopilot.Node
// interface.
type memNode struct {
	pub *btcec.PublicKey

	chans []ChannelEdge

	addrs []net.Addr
}

// A compile time assertion to ensure memNode meets the autopilot.Node
// interface.
var _ Node = (*memNode)(nil)

// PubKey is the identity public key of the node. This will be used to attempt
// to target a node for channel opening by the main autopilot agent.
//
// NOTE: Part of the autopilot.Node interface.
func (m memNode) PubKey() [33]byte {
	var n [33]byte
	copy(n[:], m.pub.SerializeCompressed())

	return n
}

// Addrs returns a slice of publicly reachable public TCP addresses that the
// peer is known to be listening on.
//
// NOTE: Part of the autopilot.Node interface.
func (m memNode) Addrs() []net.Addr {
	return m.addrs
}

// ForEachChannel is a higher-order function that will be used to iterate
// through all edges emanating from/to the target node. For each active
// channel, this function should be called with the populated ChannelEdge that
// describes the active channel.
//
// NOTE: Part of the autopilot.Node interface.
func (m memNode) ForEachChannel(cb func(ChannelEdge) error) error {
	for _, channel := range m.chans {
		if err := cb(channel); err != nil {
			return err
		}
	}

	return nil
}
