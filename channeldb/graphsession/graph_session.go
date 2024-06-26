package graphsession

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Factory implements the routing.GraphSessionFactory and can be used to start
// a session with a ReadOnlyGraph.
type Factory struct {
	graph ReadOnlyGraph
}

// NewFactory constructs a new Factory which can then be used to start a new
// Session.
func NewFactory(graph ReadOnlyGraph) routing.GraphSessionFactory {
	return &Factory{
		graph: graph,
	}
}

// NewSession will produce a new GraphSession to use for a path-finding session.
// The caller MUST call Close once it has finished using the session.
//
// This is part of the routing.GraphSessionFactory interface.
func (g *Factory) NewSession() (routing.GraphSession, error) {
	tx, err := g.graph.NewPathFindTx()
	if err != nil {
		return nil, err
	}

	return &Session{
		graph: g.graph,
		tx:    tx,
	}, nil
}

// A compile-time check to ensure that Factory implements the
// routing.GraphSessionFactory interface.
var _ routing.GraphSessionFactory = (*Factory)(nil)

// Session is an implementation of the routing.GraphSession interface and can
// be used to access the backing channel graph.
type Session struct {
	graph graph
	tx    kvdb.RTx
}

// NewRoutingGraph constructs a Session that which does not first start a
// read-only transaction and so each call on the routing.Graph will create a
// new transaction.
func NewRoutingGraph(graph ReadOnlyGraph) routing.Graph {
	return &Session{
		graph: graph,
	}
}

// Graph returns the backing graph.
//
// NOTE: this is part of the routing.GraphSession interface.
func (g *Session) Graph() routing.Graph {
	return g
}

// Close closes the read-only transaction being used to access the backing
// graph. If no transaction was started then this is a no-op.
//
// NOTE: this is part of the routing.GraphSession interface.
func (g *Session) Close() error {
	if g.tx == nil {
		return nil
	}

	err := g.tx.Rollback()
	if err != nil {
		return fmt.Errorf("error closing db tx: %w", err)
	}

	return nil
}

// ForEachNodeChannel calls the callback for every channel of the given node.
//
// NOTE: Part of the routing.Graph interface.
func (g *Session) ForEachNodeChannel(nodePub route.Vertex,
	cb func(channel *channeldb.DirectedChannel) error) error {

	return g.graph.ForEachNodeDirectedChannel(g.tx, nodePub, cb)
}

// FetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the routing.Graph interface.
func (g *Session) FetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	return g.graph.FetchNodeFeatures(nodePub)
}

// A compile-time check to ensure that *Session implements the
// routing.GraphSession interface.
var _ routing.GraphSession = (*Session)(nil)

// ReadOnlyGraph is a graph extended with a call to create a new read-only
// transaction that can then be used to make further queries to the graph.
type ReadOnlyGraph interface {
	// NewPathFindTx returns a new read transaction that can be used for a
	// single path finding session. Will return nil if the graph cache is
	// enabled.
	NewPathFindTx() (kvdb.RTx, error)

	graph
}

// graph describes the API necessary for a graph source to have in order to be
// used by the Router for pathfinding.
type graph interface {
	// ForEachNodeDirectedChannel iterates through all channels of a given
	// node, executing the passed callback on the directed edge representing
	// the channel and its incoming policy. If the callback returns an
	// error, then the iteration is halted with the error propagated back
	// up to the caller.
	//
	// Unknown policies are passed into the callback as nil values.
	ForEachNodeDirectedChannel(tx kvdb.RTx, node route.Vertex,
		cb func(channel *channeldb.DirectedChannel) error) error

	// FetchNodeFeatures returns the features of a given node. If no
	// features are known for the node, an empty feature vector is returned.
	FetchNodeFeatures(node route.Vertex) (*lnwire.FeatureVector, error)
}

// A compile-time check to ensure that *channeldb.ChannelGraph implements the
// graph interface.
var _ graph = (*channeldb.ChannelGraph)(nil)
