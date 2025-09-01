package autopilot

import (
	"context"
	"encoding/hex"
	"net"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar   = new(btcec.ModNScalar)
	testSScalar   = new(btcec.ModNScalar)
	_             = testRScalar.SetByteSlice(testRBytes)
	_             = testSScalar.SetByteSlice(testSBytes)
	testSig       = ecdsa.NewSignature(testRScalar, testSScalar)

	chanIDCounter uint64 // To be used atomically.
)

// databaseChannelGraph wraps a channeldb.ChannelGraph instance with the
// necessary API to properly implement the autopilot.ChannelGraph interface.
//
// TODO(roasbeef): move inmpl to main package?
type databaseChannelGraph struct {
	db GraphSource
}

// A compile time assertion to ensure databaseChannelGraph meets the
// autopilot.ChannelGraph interface.
var _ ChannelGraph = (*databaseChannelGraph)(nil)

// ChannelGraphFromDatabase returns an instance of the autopilot.ChannelGraph
// backed by a GraphSource.
func ChannelGraphFromDatabase(db GraphSource) ChannelGraph {
	return &databaseChannelGraph{
		db: db,
	}
}

// type dbNode is a wrapper struct around a database transaction an
// channeldb.Node. The wrapper method implement the autopilot.Node
// interface.
type dbNode struct {
	pub   [33]byte
	addrs []net.Addr
}

// A compile time assertion to ensure dbNode meets the autopilot.Node
// interface.
var _ Node = (*dbNode)(nil)

// PubKey is the identity public key of the node. This will be used to attempt
// to target a node for channel opening by the main autopilot agent. The key
// will be returned in serialized compressed format.
//
// NOTE: Part of the autopilot.Node interface.
func (d *dbNode) PubKey() [33]byte {
	return d.pub
}

// Addrs returns a slice of publicly reachable public TCP addresses that the
// peer is known to be listening on.
//
// NOTE: Part of the autopilot.Node interface.
func (d *dbNode) Addrs() []net.Addr {
	return d.addrs
}

// ForEachNode is a higher-order function that should be called once for each
// connected node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (d *databaseChannelGraph) ForEachNode(ctx context.Context,
	cb func(context.Context, Node) error, reset func()) error {

	return d.db.ForEachNode(ctx, func(n *models.Node) error {
		// We'll skip over any node that doesn't have any advertised
		// addresses. As we won't be able to reach them to actually
		// open any channels.
		if len(n.Addresses) == 0 {
			return nil
		}

		node := &dbNode{
			pub:   n.PubKeyBytes,
			addrs: n.Addresses,
		}

		return cb(ctx, node)
	}, reset)
}

// ForEachNodesChannels iterates through all connected nodes, and for each node,
// all the channels that connect to it. The passed callback will be called with
// the context, the Node itself, and a slice of ChannelEdge that connect to the
// node.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (d *databaseChannelGraph) ForEachNodesChannels(ctx context.Context,
	cb func(context.Context, Node, []*ChannelEdge) error,
	reset func()) error {

	return d.db.ForEachNodeCached(
		ctx, true, func(ctx context.Context, node route.Vertex,
			addrs []net.Addr,
			chans map[uint64]*graphdb.DirectedChannel) error {

			// We'll skip over any node that doesn't have any
			// advertised addresses. As we won't be able to reach
			// them to actually open any channels.
			if len(addrs) == 0 {
				return nil
			}

			edges := make([]*ChannelEdge, 0, len(chans))
			for _, channel := range chans {
				edges = append(edges, &ChannelEdge{
					ChanID: lnwire.NewShortChanIDFromInt(
						channel.ChannelID,
					),
					Capacity: channel.Capacity,
					Peer:     channel.OtherNode,
				})
			}

			return cb(ctx, &dbNode{
				pub:   node,
				addrs: addrs,
			}, edges)
		}, reset,
	)
}

// databaseChannelGraphCached wraps a channeldb.ChannelGraph instance with the
// necessary API to properly implement the autopilot.ChannelGraph interface.
type databaseChannelGraphCached struct {
	db GraphSource
}

// A compile time assertion to ensure databaseChannelGraphCached meets the
// autopilot.ChannelGraph interface.
var _ ChannelGraph = (*databaseChannelGraphCached)(nil)

// ChannelGraphFromCachedDatabase returns an instance of the
// autopilot.ChannelGraph backed by a live, open channeldb instance.
func ChannelGraphFromCachedDatabase(db GraphSource) ChannelGraph {
	return &databaseChannelGraphCached{
		db: db,
	}
}

// dbNodeCached is a wrapper struct around a database transaction for a
// channeldb.Node. The wrapper methods implement the autopilot.Node
// interface.
type dbNodeCached struct {
	node     route.Vertex
	channels map[uint64]*graphdb.DirectedChannel
}

// A compile time assertion to ensure dbNodeCached meets the autopilot.Node
// interface.
var _ Node = (*dbNodeCached)(nil)

// PubKey is the identity public key of the node.
//
// NOTE: Part of the autopilot.Node interface.
func (nc dbNodeCached) PubKey() [33]byte {
	return nc.node
}

// Addrs returns a slice of publicly reachable public TCP addresses that the
// peer is known to be listening on.
//
// NOTE: Part of the autopilot.Node interface.
func (nc dbNodeCached) Addrs() []net.Addr {
	// TODO: Add addresses to be usable by autopilot.
	return []net.Addr{}
}

// ForEachNode is a higher-order function that should be called once for each
// connected node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (dc *databaseChannelGraphCached) ForEachNode(ctx context.Context,
	cb func(context.Context, Node) error, reset func()) error {

	return dc.db.ForEachNodeCached(ctx, false, func(ctx context.Context,
		n route.Vertex, _ []net.Addr,
		channels map[uint64]*graphdb.DirectedChannel) error {

		if len(channels) > 0 {
			node := dbNodeCached{
				node:     n,
				channels: channels,
			}

			return cb(ctx, node)
		}

		return nil
	}, reset)
}

// ForEachNodesChannels iterates through all connected nodes, and for each node,
// all the channels that connect to it. The passed callback will be called with
// the context, the Node itself, and a slice of ChannelEdge that connect to the
// node.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (dc *databaseChannelGraphCached) ForEachNodesChannels(ctx context.Context,
	cb func(context.Context, Node, []*ChannelEdge) error,
	reset func()) error {

	return dc.db.ForEachNodeCached(ctx, false, func(ctx context.Context,
		n route.Vertex, _ []net.Addr,
		channels map[uint64]*graphdb.DirectedChannel) error {

		edges := make([]*ChannelEdge, 0, len(channels))
		for cid, channel := range channels {
			edges = append(edges, &ChannelEdge{
				ChanID:   lnwire.NewShortChanIDFromInt(cid),
				Capacity: channel.Capacity,
				Peer:     channel.OtherNode,
			})
		}

		if len(channels) > 0 {
			node := dbNodeCached{
				node:     n,
				channels: channels,
			}

			if err := cb(ctx, node, edges); err != nil {
				return err
			}
		}

		return nil
	}, reset)
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

// Median returns the median value in the slice of Amounts.
func Median(vals []btcutil.Amount) btcutil.Amount {
	sort.Slice(vals, func(i, j int) bool {
		return vals[i] < vals[j]
	})

	num := len(vals)
	switch {
	case num == 0:
		return 0

	case num%2 == 0:
		return (vals[num/2-1] + vals[num/2]) / 2

	default:
		return vals[num/2]
	}
}
