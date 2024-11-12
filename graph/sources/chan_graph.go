package sources

import (
	"context"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
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
