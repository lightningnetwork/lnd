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

// NewGraphSessionFactory constructs a new Factory which can then be used to
// start a new session.
func NewGraphSessionFactory(graph ReadOnlyGraph) routing.GraphSessionFactory {
	return &Factory{
		graph: graph,
	}
}

// NewGraphSession will produce a new Graph to use for a path-finding session.
// It returns the Graph along with a call-back that must be called once Graph
// access is complete. This call-back will close any read-only transaction that
// was created at Graph construction time.
//
// NOTE: This is part of the routing.GraphSessionFactory interface.
func (g *Factory) NewGraphSession() (routing.Graph, func() error, error) {
	tx, err := g.graph.NewPathFindTx()
	if err != nil {
		return nil, nil, err
	}

	session := &session{
		graph: g.graph,
		tx:    tx,
	}

	return session, session.close, nil
}

// A compile-time check to ensure that Factory implements the
// routing.GraphSessionFactory interface.
var _ routing.GraphSessionFactory = (*Factory)(nil)

// session is an implementation of the routing.Graph interface where the same
// read-only transaction is held across calls to the graph and can be used to
// access the backing channel graph.
type session struct {
	graph graph
	tx    kvdb.RTx
}

// NewRoutingGraph constructs a session that which does not first start a
// read-only transaction and so each call on the routing.Graph will create a
// new transaction.
func NewRoutingGraph(graph ReadOnlyGraph) routing.Graph {
	return &session{
		graph: graph,
	}
}

// close closes the read-only transaction being used to access the backing
// graph. If no transaction was started then this is a no-op.
func (g *session) close() error {
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
func (g *session) ForEachNodeChannel(nodePub route.Vertex,
	cb func(channel *channeldb.DirectedChannel) error) error {

	return g.graph.ForEachNodeDirectedChannel(g.tx, nodePub, cb)
}

// FetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the routing.Graph interface.
func (g *session) FetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	return g.graph.FetchNodeFeatures(nodePub)
}

// A compile-time check to ensure that *session implements the
// routing.Graph interface.
var _ routing.Graph = (*session)(nil)

// ReadOnlyGraph is a graph extended with a call to create a new read-only
// transaction that can then be used to make further queries to the graph.
type ReadOnlyGraph interface {
	// NewPathFindTx returns a new read transaction that can be used for a
	// single path finding session. Will return nil if the graph cache is
	// enabled.
	NewPathFindTx() (kvdb.RTx, error)

	graph
}

// graph describes the API necessary for a graph source to have access to on a
// database implementation, like channeldb.ChannelGraph, in order to be used by
// the Router for pathfinding.
type graph interface {
	// ForEachNodeDirectedChannel iterates through all channels of a given
	// node, executing the passed callback on the directed edge representing
	// the channel and its incoming policy. If the callback returns an
	// error, then the iteration is halted with the error propagated back
	// up to the caller.
	//
	// Unknown policies are passed into the callback as nil values.
	//
	// NOTE: if a nil tx is provided, then it is expected that the
	// implementation create a read only tx.
	ForEachNodeDirectedChannel(tx kvdb.RTx, node route.Vertex,
		cb func(channel *channeldb.DirectedChannel) error) error

	// FetchNodeFeatures returns the features of a given node. If no
	// features are known for the node, an empty feature vector is returned.
	FetchNodeFeatures(node route.Vertex) (*lnwire.FeatureVector, error)
}

// A compile-time check to ensure that *channeldb.ChannelGraph implements the
// graph interface.
var _ graph = (*channeldb.ChannelGraph)(nil)
