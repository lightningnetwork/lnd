package autopilot

import (
	"context"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// DefaultConfTarget is the default confirmation target for autopilot channels.
// TODO(halseth): possibly make dynamic, going aggressive->lax as more channels
// are opened.
const DefaultConfTarget = 3

// Node is an interface which represents n abstract vertex within the
// channel graph. All nodes should have at least a single edge to/from them
// within the graph.
//
// TODO(roasbeef): combine with routing.ChannelGraphSource
type Node interface {
	// PubKey is the identity public key of the node. This will be used to
	// attempt to target a node for channel opening by the main autopilot
	// agent. The key will be returned in serialized compressed format.
	PubKey() [33]byte

	// Addrs returns a slice of publicly reachable public TCP addresses
	// that the peer is known to be listening on.
	Addrs() []net.Addr
}

// LocalChannel is a simple struct which contains relevant details of a
// particular channel the local node has. The fields in this struct may be used
// as signals for various AttachmentHeuristic implementations.
type LocalChannel struct {
	// ChanID is the short channel ID for this channel as defined within
	// BOLT-0007.
	ChanID lnwire.ShortChannelID

	// Balance is the local balance of the channel expressed in satoshis.
	Balance btcutil.Amount

	// Node is the peer that this channel has been established with.
	Node NodeID

	// TODO(roasbeef): also add other traits?
	//  * fee, timelock, etc
}

// ChannelEdge is a struct that holds details concerning a channel, but also
// contains a reference to the Node that this channel connects to as a directed
// edge within the graph. The existence of this reference to the connected node
// will allow callers to traverse the graph in an object-oriented manner.
type ChannelEdge struct {
	// ChanID is the short channel ID for this channel as defined within
	// BOLT-0007.
	ChanID lnwire.ShortChannelID

	// Capacity is the capacity of the channel expressed in satoshis.
	Capacity btcutil.Amount

	// Peer is the peer that this channel creates an edge to in the channel
	// graph.
	Peer route.Vertex
}

// ChannelGraph in an interface that represents a traversable channel graph.
// The autopilot agent will use this interface as its source of graph traits in
// order to make decisions concerning which channels should be opened, and to
// whom.
//
// TODO(roasbeef): abstract??
type ChannelGraph interface {
	// ForEachNode is a higher-order function that should be called once
	// for each connected node within the channel graph. If the passed
	// callback returns an error, then execution should be terminated.
	ForEachNode(context.Context, func(context.Context, Node) error,
		func()) error

	// ForEachNodesChannels iterates through all connected nodes, and for
	// each node, all the channels that connect to it. The passed callback
	// will be called with the context, the Node itself, and a slice of
	// ChannelEdge that connect to the node.
	ForEachNodesChannels(ctx context.Context,
		cb func(context.Context, Node, []*ChannelEdge) error,
		reset func()) error
}

// NodeScore is a tuple mapping a NodeID to a score indicating the preference
// of opening a channel with it.
type NodeScore struct {
	// NodeID is the serialized compressed pubkey of the node that is being
	// scored.
	NodeID NodeID

	// Score is the score given by the heuristic for opening a channel of
	// the given size to this node.
	Score float64
}

// AttachmentDirective describes a channel attachment proscribed by an
// AttachmentHeuristic. It details to which node a channel should be created
// to, and also the parameters which should be used in the channel creation.
type AttachmentDirective struct {
	// NodeID is the serialized compressed pubkey of the target node for
	// this attachment directive. It can be identified by its public key,
	// and therefore can be used along with a ChannelOpener implementation
	// to execute the directive.
	NodeID NodeID

	// ChanAmt is the size of the channel that should be opened, expressed
	// in satoshis.
	ChanAmt btcutil.Amount

	// Addrs is a list of addresses that the target peer may be reachable
	// at.
	Addrs []net.Addr
}

// AttachmentHeuristic is one of the primary interfaces within this package.
// Implementations of this interface will be used to implement a control system
// which automatically regulates channels of a particular agent, attempting to
// optimize channels opened/closed based on various heuristics. The purpose of
// the interface is to allow an auto-pilot agent to decide if it needs more
// channels, and if so, which exact channels should be opened.
type AttachmentHeuristic interface {
	// Name returns the name of this heuristic.
	Name() string

	// NodeScores is a method that given the current channel graph and
	// current set of local channels, scores the given nodes according to
	// the preference of opening a channel of the given size with them. The
	// returned channel candidates maps the NodeID to a NodeScore for the
	// node.
	//
	// The returned scores will be in the range [0, 1.0], where 0 indicates
	// no improvement in connectivity if a channel is opened to this node,
	// while 1.0 is the maximum possible improvement in connectivity. The
	// implementation of this interface must return scores in this range to
	// properly allow the autopilot agent to make a reasonable choice based
	// on the score from multiple heuristics.
	//
	// NOTE: A NodeID not found in the returned map is implicitly given a
	// score of 0.
	NodeScores(ctx context.Context, g ChannelGraph, chans []LocalChannel,
		chanSize btcutil.Amount, nodes map[NodeID]struct{}) (
		map[NodeID]*NodeScore, error)
}

// NodeMetric is a common interface for all graph metrics that are not
// directly used as autopilot node scores but may be used in compositional
// heuristics or statistical information exposed to users.
type NodeMetric interface {
	// Name returns the unique name of this metric.
	Name() string

	// Refresh refreshes the metric values based on the current graph.
	Refresh(ctx context.Context, graph ChannelGraph) error

	// GetMetric returns the latest value of this metric. Values in the
	// map are per node and can be in arbitrary domain. If normalize is
	// set to true, then the returned values are normalized to either
	// [0, 1] or [-1, 1] depending on the metric.
	GetMetric(normalize bool) map[NodeID]float64
}

// ScoreSettable is an interface that indicates that the scores returned by the
// heuristic can be mutated by an external caller. The ExternalScoreAttachment
// currently implements this interface, and so should any heuristic that is
// using the ExternalScoreAttachment as a sub-heuristic, or keeps their own
// internal list of mutable scores, to allow access to setting the internal
// scores.
type ScoreSettable interface {
	// SetNodeScores is used to set the internal map from NodeIDs to
	// scores. The passed scores must be in the range [0, 1.0]. The first
	// parameter is the name of the targeted heuristic, to allow
	// recursively target specific sub-heuristics. The returned boolean
	// indicates whether the targeted heuristic was found.
	SetNodeScores(string, map[NodeID]float64) (bool, error)
}

var (
	// availableHeuristics holds all heuristics possible to combine for use
	// with the autopilot agent.
	availableHeuristics = []AttachmentHeuristic{
		NewPrefAttachment(),
		NewExternalScoreAttachment(),
		NewTopCentrality(),
	}

	// AvailableHeuristics is a map that holds the name of available
	// heuristics to the actual heuristic for easy lookup. It will be
	// filled during init().
	AvailableHeuristics = make(map[string]AttachmentHeuristic)
)

func init() {
	// Fill the map from heuristic names to available heuristics for easy
	// lookup.
	for _, h := range availableHeuristics {
		AvailableHeuristics[h.Name()] = h
	}
}

// ChannelController is a simple interface that allows an auto-pilot agent to
// open a channel within the graph to a target peer, close targeted channels,
// or add/remove funds from existing channels via a splice in/out mechanisms.
type ChannelController interface {
	// OpenChannel opens a channel to a target peer, using at most amt
	// funds. This means that the resulting channel capacity might be
	// slightly less to account for fees. This function should un-block
	// immediately after the funding transaction that marks the channel
	// open has been broadcast.
	OpenChannel(target *btcec.PublicKey, amt btcutil.Amount) error

	// CloseChannel attempts to close out the target channel.
	//
	// TODO(roasbeef): add force option?
	CloseChannel(chanPoint *wire.OutPoint) error
}

// GraphSource represents read access to the channel graph.
type GraphSource interface {
	// ForEachNode iterates through all the stored vertices/nodes in the
	// graph, executing the passed callback with each node encountered. If
	// the callback returns an error, then the transaction is aborted and
	// the iteration stops early.
	ForEachNode(context.Context, func(*models.Node) error,
		func()) error

	// ForEachNodeCached is similar to ForEachNode, but it utilizes the
	// channel graph cache if one is available. It is less consistent than
	// ForEachNode since any further calls are made across multiple
	// transactions.
	ForEachNodeCached(ctx context.Context, withAddrs bool,
		cb func(ctx context.Context, node route.Vertex,
			addrs []net.Addr,
			chans map[uint64]*graphdb.DirectedChannel) error,
		reset func()) error
}
