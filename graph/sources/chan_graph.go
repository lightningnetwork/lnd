package sources

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/discovery"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/graph/session"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// DBSource is an implementation of the GraphSource interface backed by a local
// persistence layer holding graph related data.
type DBSource struct {
	db *graphdb.ChannelGraph
}

// A compile-time check to ensure that sources.DBSource implements the
// GraphSource interface.
var _ GraphSource = (*DBSource)(nil)

// NewDBGSource returns a new instance of the DBSource backed by a
// graphdb.ChannelGraph instance.
func NewDBGSource(db *graphdb.ChannelGraph) *DBSource {
	return &DBSource{
		db: db,
	}
}

// NewPathFindTx returns a new read transaction that can be used for a single
// path finding session. Will return nil if the graph cache is enabled for the
// underlying graphdb.ChannelGraph.
//
// NOTE: this is part of the session.ReadOnlyGraph interface.
func (s *DBSource) NewPathFindTx(_ context.Context) (session.RTx, error) {
	tx, err := s.db.NewPathFindTx()
	if err != nil {
		return nil, err
	}

	return newKVDBRTx(tx), nil
}

// ForEachNodeDirectedChannel iterates through all channels of a given node,
// executing the passed callback on the directed edge representing the channel
// and its incoming policy. If the callback returns an error, then the
// iteration is halted with the error propagated back up to the caller. An
// optional read transaction may be provided. If it is, then it will be cast
// into a kvdb.RTx and passed into the callback.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: this is part of the session.ReadOnlyGraph interface.
func (s *DBSource) ForEachNodeDirectedChannel(_ context.Context, tx session.RTx,
	node route.Vertex,
	cb func(channel *graphdb.DirectedChannel) error) error {

	kvdbTx, err := extractKVDBRTx(tx)
	if err != nil {
		return err
	}

	return s.db.ForEachNodeDirectedChannel(kvdbTx, node, cb)
}

// FetchNodeFeatures returns the features of a given node. If no features are
// known for the node, an empty feature vector is returned. An optional read
// transaction may be provided. If it is, then it will be cast into a kvdb.RTx
// and passed into the callback.
//
// NOTE: this is part of the graphsession.ReadOnlyGraph interface.
func (s *DBSource) FetchNodeFeatures(_ context.Context, tx session.RTx,
	node route.Vertex) (*lnwire.FeatureVector, error) {

	kvdbTx, err := extractKVDBRTx(tx)
	if err != nil {
		return nil, err
	}

	return s.db.FetchNodeFeatures(kvdbTx, node)
}

// FetchChannelEdgesByID attempts to look up the two directed edges for the
// channel identified by the channel ID. If the channel can't be found, then
// graphdb.ErrEdgeNotFound is returned.
//
// NOTE: this is part of the invoicesrpc.GraphSource interface.
func (s *DBSource) FetchChannelEdgesByID(_ context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return s.db.FetchChannelEdgesByID(chanID)
}

// IsPublicNode determines whether the node with the given public key is seen as
// a public node in the graph from the graph's source node's point of view.
//
// NOTE: this is part of the invoicesrpc.GraphSource interface.
func (s *DBSource) IsPublicNode(_ context.Context,
	pubKey [33]byte) (bool, error) {

	return s.db.IsPublicNode(pubKey)
}

// FetchChannelEdgesByOutpoint returns the channel edge info and most recent
// channel edge policies for a given outpoint.
//
// NOTE: this is part of the netann.ChannelGraph interface.
func (s *DBSource) FetchChannelEdgesByOutpoint(_ context.Context,
	point *wire.OutPoint) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	return s.db.FetchChannelEdgesByOutpoint(point)
}

// AddrsForNode returns all known addresses for the target node public key. The
// returned boolean indicatex if the given node is unknown to the backing
// source.
//
// NOTE: this is part of the channeldb.AddrSource interface.
func (s *DBSource) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	return s.db.AddrsForNode(ctx, nodePub)
}

// ForEachChannel iterates through all the channel edges stored within the graph
// and invokes the passed callback for each edge. If the callback returns an
// error, then the transaction is aborted and the iteration stops early. An
// edge's policy structs may be nil if the ChannelUpdate in question has not yet
// been received for the channel.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) ForEachChannel(_ context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	return s.db.ForEachChannel(cb)
}

// ForEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) ForEachNode(_ context.Context,
	cb func(*models.LightningNode) error) error {

	return s.db.ForEachNode(func(_ kvdb.RTx,
		node *models.LightningNode) error {

		return cb(node)
	})
}

// HasLightningNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) HasLightningNode(_ context.Context,
	nodePub [33]byte) (time.Time, bool, error) {

	return s.db.HasLightningNode(nodePub)
}

// LookupAlias attempts to return the alias as advertised by the target node.
// graphdb.ErrNodeAliasNotFound is returned if the alias is not found.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) LookupAlias(_ context.Context,
	pub *btcec.PublicKey) (string, error) {

	return s.db.LookupAlias(pub)
}

// ForEachNodeChannel iterates through all channels of the given node, executing
// the passed callback with an edge info structure and the policies of each end
// of the channel. The first edge policy is the outgoing edge *to* the
// connecting node, while the second is the incoming edge *from* the connecting
// node. If the callback returns an error, then the iteration is halted with the
// error propagated back up to the caller. Unknown policies are passed into the
// callback as nil values.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) ForEachNodeChannel(_ context.Context,
	nodePub route.Vertex, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	return s.db.ForEachNodeChannel(nodePub, func(_ kvdb.RTx,
		info *models.ChannelEdgeInfo, policy *models.ChannelEdgePolicy,
		policy2 *models.ChannelEdgePolicy) error {

		return cb(info, policy, policy2)
	})
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then
// graphdb.ErrGraphNodeNotFound is returned.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) FetchLightningNode(_ context.Context,
	nodePub route.Vertex) (*models.LightningNode, error) {

	return s.db.FetchLightningNode(nodePub)
}

// GraphBootstrapper returns a NetworkPeerBootstrapper instance backed by the
// ChannelGraph instance.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) GraphBootstrapper(_ context.Context) (
	discovery.NetworkPeerBootstrapper, error) {

	chanGraph := autopilot.ChannelGraphFromDatabase(s.db)

	return discovery.NewGraphBootstrapper(chanGraph)
}

// kvdbRTx is an implementation of graphdb.RTx backed by a KVDB database read
// transaction.
type kvdbRTx struct {
	kvdb.RTx
}

// newKVDBRTx constructs a kvdbRTx instance backed by the given kvdb.RTx.
func newKVDBRTx(tx kvdb.RTx) *kvdbRTx {
	return &kvdbRTx{tx}
}

// Close closes the underlying transaction.
//
// NOTE: this is part of the graphdb.RTx interface.
func (t *kvdbRTx) Close() error {
	if t.RTx == nil {
		return nil
	}

	return t.RTx.Rollback()
}

// MustImplementRTx is a helper method that ensures that the kvdbRTx type
// implements the RTx interface.
//
// NOTE: this is part of the graphdb.RTx interface.
func (t *kvdbRTx) MustImplementRTx() {}

// A compile-time assertion to ensure that kvdbRTx implements the RTx interface.
var _ session.RTx = (*kvdbRTx)(nil)

// extractKVDBRTx is a helper function that casts an RTx into a kvdbRTx and
// errors if the cast fails.
func extractKVDBRTx(tx session.RTx) (kvdb.RTx, error) {
	if tx == nil {
		return nil, nil
	}

	kvdbTx, ok := tx.(*kvdbRTx)
	if !ok {
		return nil, fmt.Errorf("expected a graphdb.kvdbRTx, got %T", tx)
	}

	return kvdbTx, nil
}
