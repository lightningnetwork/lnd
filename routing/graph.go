package routing

import (
	"bytes"
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/btcec"
)

// TODO(roasbeef): abstract out graph to interface
//  * add in-memory version of graph for tests

// TODO(nalinbhardwaj): move routing.ChannelGraphSource here.

// Vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type Vertex [33]byte

// NewVertex returns a new Vertex given a public key.
func NewVertex(pub *btcec.PublicKey) Vertex {
	var v Vertex
	copy(v[:], pub.SerializeCompressed())
	return v
}

// String returns a human readable version of the Vertex which is the
// hex-encoding of the serialized compressed public key.
func (v Vertex) String() string {
	return fmt.Sprintf("%x", v[:])
}

// Edge is a struct holding the edge info we need when performing path
// finding using a graphSource.
type Edge struct {
	edgeInfo *channeldb.ChannelEdgeInfo
	outEdge  *channeldb.ChannelEdgePolicy
	inEdge   *channeldb.ChannelEdgePolicy
}

// graphSource is a wrapper around a graph that can either be in memory or
// on disk, like the regular channeldb.ChannelGraph.
type graphSource interface {
	ForEachNode(func(Vertex) error) error
	ForEachChannel(Vertex, func(*Edge) error) error
}

// memChannelGraph is a implementation of the routing.graphSource backed by
// an in-memory graph.
type memChannelGraph struct {
	nodes []Vertex
	graph map[Vertex][]*Edge
}

// A compile time assertion to ensure memChannelGraph meets the
// routing.graphSource interface.
var _ graphSource = (*memChannelGraph)(nil)

// newMemChannelGraphFromDatabase returns an instance of memChannelGraph
// from an instance of channeldb.ChannelGraph.
func newMemChannelGraphFromDatabase(db *channeldb.ChannelGraph) (*memChannelGraph, error) {
	res := memChannelGraph{
		graph: make(map[Vertex][]*Edge),
	}

	tx, err := db.Database().Begin(false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Create entry for each node into graph.
	err = db.ForEachNode(tx,
		func(tx *bolt.Tx, node *channeldb.LightningNode) error {
			nodeVertex := Vertex(node.PubKeyBytes)
			res.nodes = append(res.nodes, nodeVertex)

			var adjList []*Edge

			err := node.ForEachChannel(tx, func(_ *bolt.Tx,
				edgeInfo *channeldb.ChannelEdgeInfo,
				outEdge, inEdge *channeldb.ChannelEdgePolicy) error {
				e := &Edge{
					edgeInfo: edgeInfo,
					outEdge:  outEdge,
					inEdge:   inEdge,
				}
				adjList = append(adjList, e)
				return nil
			})
			if err != nil {
				return err
			}
			res.graph[nodeVertex] = adjList

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &res, nil
}

// ForEachNode is a function that should be called once for each connected
// node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the routing.graphSource interface.
func (m memChannelGraph) ForEachNode(cb func(Vertex) error) error {
	for _, node := range m.nodes {
		if err := cb(node); err != nil {
			return err
		}
	}

	return nil
}

// ForEachChannel is a function that will be used to iterate through all edges
// emanating from/to the target node. For each active channel, this function
// should be called with the populated memEdge that
// describes the active channel.
//
// NOTE: Part of the routing.graphSource interface.
func (m memChannelGraph) ForEachChannel(target Vertex, cb func(*Edge) error) error {
	for _, channel := range m.graph[target] {
		if err := cb(channel); err != nil {
			return err
		}
	}

	return nil
}

// dbChannelGraph wraps a channeldb.ChannelGraph instance with the
// necessary API to properly implement the routing.graphSource interface.
type dbChannelGraph struct {
	db *channeldb.ChannelGraph
}

// A compile time assertion to ensure dbChannelGraph meets the
// routing.graphSource interface.
var _ graphSource = (*dbChannelGraph)(nil)

// newDbChannelGraphFromDatabase returns an instance of the routing.graphSource
// backed by a live, open channeldb instance.
func newDbChannelGraphFromDatabase(db *channeldb.ChannelGraph) (*dbChannelGraph, error) {
	return &dbChannelGraph{
		db: db,
	}, nil
}

// ForEachNode is a function that should be called once for each connected
// node within the channel graph. If the passed callback returns an
// error, then execution should be terminated.
//
// NOTE: Part of the routing.graphSource interface.
func (d dbChannelGraph) ForEachNode(cb func(Vertex) error) error {
	return d.db.ForEachNode(nil, func(tx *bolt.Tx, node *channeldb.LightningNode) error {
		return cb(Vertex(node.PubKeyBytes))
	})
}

// ForEachChannel is a function that will be used to iterate through all edges
// emanating from/to the target node. For each active channel, this function
// should be called with the populated memEdge that
// describes the active channel.
//
// NOTE: Part of the routing.graphSource interface.
func (d dbChannelGraph) ForEachChannel(target Vertex, cb func(*Edge) error) error {
	return d.db.ForEachNode(nil, func(tx *bolt.Tx, node *channeldb.LightningNode) error {
		if bytes.Equal(target[:], node.PubKeyBytes[:]) {
			return node.ForEachChannel(tx, func(_ *bolt.Tx,
				edgeInfo *channeldb.ChannelEdgeInfo,
				outEdge, inEdge *channeldb.ChannelEdgePolicy) error {
				e := &Edge{
					edgeInfo: edgeInfo,
					outEdge:  outEdge,
					inEdge:   inEdge,
				}
				return cb(e)
			})
		}
		return nil
	})
}
