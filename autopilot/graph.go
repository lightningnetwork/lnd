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
// channeldb.LightningNode. The wrapper method implement the autopilot.Node
// interface.
type dbNode struct {
	tx graphdb.NodeRTx
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
	return d.tx.Node().PubKeyBytes
}

// Addrs returns a slice of publicly reachable public TCP addresses that the
// peer is known to be listening on.
//
// NOTE: Part of the autopilot.Node interface.
func (d *dbNode) Addrs() []net.Addr {
	return d.tx.Node().Addresses
}

// ForEachChannel is a higher-order function that will be used to iterate
// through all edges emanating from/to the target node. For each active
// channel, this function should be called with the populated ChannelEdge that
// describes the active channel.
//
// NOTE: Part of the autopilot.Node interface.
func (d *dbNode) ForEachChannel(ctx context.Context,
	cb func(context.Context, ChannelEdge) error) error {

	return d.tx.ForEachChannel(func(ei *models.ChannelEdgeInfo, ep,
		_ *models.ChannelEdgePolicy) error {

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

		node, err := d.tx.FetchNode(ep.ToNode)
		if err != nil {
			return err
		}

		edge := ChannelEdge{
			ChanID:   lnwire.NewShortChanIDFromInt(ep.ChannelID),
			Capacity: ei.Capacity,
			Peer: &dbNode{
				tx: node,
			},
		}

		return cb(ctx, edge)
	})
}

// ForEachNode is a higher-order function that should be called once for each
// connected node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (d *databaseChannelGraph) ForEachNode(ctx context.Context,
	cb func(context.Context, Node) error) error {

	return d.db.ForEachNode(ctx, func(nodeTx graphdb.NodeRTx) error {
		// We'll skip over any node that doesn't have any advertised
		// addresses. As we won't be able to reach them to actually
		// open any channels.
		if len(nodeTx.Node().Addresses) == 0 {
			return nil
		}

		node := &dbNode{
			tx: nodeTx,
		}

		return cb(ctx, node)
	})
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
// channeldb.LightningNode. The wrapper methods implement the autopilot.Node
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

// ForEachChannel is a higher-order function that will be used to iterate
// through all edges emanating from/to the target node. For each active
// channel, this function should be called with the populated ChannelEdge that
// describes the active channel.
//
// NOTE: Part of the autopilot.Node interface.
func (nc dbNodeCached) ForEachChannel(ctx context.Context,
	cb func(context.Context, ChannelEdge) error) error {

	for cid, channel := range nc.channels {
		edge := ChannelEdge{
			ChanID:   lnwire.NewShortChanIDFromInt(cid),
			Capacity: channel.Capacity,
			Peer: dbNodeCached{
				node: channel.OtherNode,
			},
		}

		if err := cb(ctx, edge); err != nil {
			return err
		}
	}

	return nil
}

// ForEachNode is a higher-order function that should be called once for each
// connected node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the autopilot.ChannelGraph interface.
func (dc *databaseChannelGraphCached) ForEachNode(ctx context.Context,
	cb func(context.Context, Node) error) error {

	return dc.db.ForEachNodeCached(func(n route.Vertex,
		channels map[uint64]*graphdb.DirectedChannel) error {

		if len(channels) > 0 {
			node := dbNodeCached{
				node:     n,
				channels: channels,
			}

			return cb(ctx, node)
		}
		return nil
	})
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
func (m memNode) ForEachChannel(ctx context.Context,
	cb func(context.Context, ChannelEdge) error) error {

	for _, channel := range m.chans {
		if err := cb(ctx, channel); err != nil {
			return err
		}
	}

	return nil
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
