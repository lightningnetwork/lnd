package graphdb

import (
	"context"
	"errors"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// GraphSource is a read-only interface for accessing channel graph data. It is
// version-agnostic: implementations return the best available data across all
// gossip protocol versions. This allows consumers (routing, RPC, autopilot,
// etc.) to work with a unified view of the network without caring about gossip
// protocol versions or whether the data comes from a local database or a
// remote source.
//
//nolint:interfacebloat
type GraphSource interface {
	NodeTraverser

	// ForEachNode iterates through all the stored vertices/nodes in the
	// graph, executing the passed callback with each node encountered. If
	// the callback returns an error, then the transaction is aborted and
	// the iteration stops early.
	ForEachNode(ctx context.Context,
		cb func(*models.Node) error, reset func()) error

	// ForEachChannel iterates through all the channel edges stored within
	// the graph and invokes the passed callback for each edge. The callback
	// receives the edge info and both directed edge policies. If the
	// callback returns an error, then the iteration is halted with the
	// error propagated back up to the caller. Channels from all gossip
	// versions are included.
	ForEachChannel(ctx context.Context,
		cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error,
		reset func()) error

	// ForEachNodeChannel iterates through all channels of the given node,
	// executing the passed callback with an edge info structure and the
	// policies of each end of the channel. Channels from all gossip
	// versions are included.
	ForEachNodeChannel(ctx context.Context,
		nodePub route.Vertex,
		cb func(*models.ChannelEdgeInfo,
			*models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error,
		reset func()) error

	// ForEachNodeCached is similar to ForEachNode, but it returns
	// DirectedChannel data to the callback. Uses the graph cache if
	// available.
	ForEachNodeCached(ctx context.Context, withAddrs bool,
		cb func(ctx context.Context, node route.Vertex,
			addrs []net.Addr,
			chans map[uint64]*DirectedChannel) error,
		reset func()) error

	// FetchChannelEdgesByID attempts to look up the two directed edges for
	// the channel identified by the channel ID. If the channel can't be
	// found, then ErrEdgeNotFound is returned.
	FetchChannelEdgesByID(ctx context.Context, chanID uint64) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchChannelEdgesByOutpoint attempts to look up the two directed
	// edges for the channel identified by the funding outpoint.
	FetchChannelEdgesByOutpoint(ctx context.Context,
		op *wire.OutPoint) (
		*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy, error)

	// FetchNode looks up a target node by its identity public key. If the
	// node isn't found in the database, then ErrGraphNodeNotFound is
	// returned.
	FetchNode(ctx context.Context,
		nodePub route.Vertex) (*models.Node, error)

	// LookupAlias attempts to return the alias as advertised by the target
	// node.
	LookupAlias(ctx context.Context,
		pub *btcec.PublicKey) (string, error)

	// IsPublicNode determines whether the node with the given public key
	// is seen as a public node in the graph from the graph's source node's
	// point of view.
	IsPublicNode(ctx context.Context, pubKey [33]byte) (bool, error)

	// AddrsForNode returns all known addresses for the target node public
	// key. The returned boolean indicates if the given node is known to
	// the graph.
	AddrsForNode(ctx context.Context,
		nodePub *btcec.PublicKey) (bool, []net.Addr, error)

	// GraphSession provides the callback with access to a NodeTraverser
	// which can be used to perform queries against the channel graph with
	// read consistency.
	GraphSession(ctx context.Context,
		cb func(graph NodeTraverser) error, reset func()) error

	// SourceNode returns the source node of the graph. The source node is
	// treated as the center node within a star-graph.
	SourceNode(ctx context.Context) (*models.Node, error)

	// ForEachSourceNodeChannel iterates through all channels of the source
	// node.
	ForEachSourceNodeChannel(ctx context.Context,
		cb func(chanPoint wire.OutPoint, havePolicy bool,
			otherNode *models.Node) error,
		reset func()) error
}

// Compile-time check that DBGraphSource implements GraphSource.
var _ GraphSource = (*DBGraphSource)(nil)

// DBGraphSource implements GraphSource backed by a ChannelGraph database. It
// provides a version-agnostic view: single-item lookups prefer the highest
// available gossip version, while iteration methods currently cover v1 with
// cross-version iteration planned.
type DBGraphSource struct {
	cg *ChannelGraph
}

// NewDBGraphSource creates a new DBGraphSource backed by the given
// ChannelGraph.
func NewDBGraphSource(cg *ChannelGraph) *DBGraphSource {
	return &DBGraphSource{cg: cg}
}

// ForEachNodeDirectedChannel calls the callback for every channel of the given
// node, using the graph cache if available.
//
// NOTE: this is part of the NodeTraverser interface.
func (d *DBGraphSource) ForEachNodeDirectedChannel(ctx context.Context,
	nodePub route.Vertex,
	cb func(channel *DirectedChannel) error, reset func()) error {

	return d.cg.ForEachNodeDirectedChannel(ctx, nodePub, cb, reset)
}

// FetchNodeFeatures returns the features of the given node.
//
// NOTE: this is part of the NodeTraverser interface.
func (d *DBGraphSource) FetchNodeFeatures(ctx context.Context,
	nodePub route.Vertex) (*lnwire.FeatureVector, error) {

	return d.cg.FetchNodeFeatures(ctx, nodePub)
}

// ForEachNode iterates through all the stored vertices/nodes in the graph.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) ForEachNode(ctx context.Context,
	cb func(*models.Node) error, reset func()) error {

	return d.cg.ForEachNode(ctx, cb, reset)
}

// ForEachChannel iterates through all channel edges stored within the graph.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	reset func()) error {

	// TODO(elle): add cross-version iteration support once the store
	// provides a unified API for iterating across gossip versions. For
	// now we iterate v1 only, matching the existing behavior.
	return d.cg.db.ForEachChannel(ctx, gossipV1, cb, reset)
}

// ForEachNodeChannel iterates through all channels of the given node.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	reset func()) error {

	// TODO(elle): add cross-version iteration support once the store
	// provides a unified API for iterating across gossip versions. For
	// now we iterate v1 only, matching the existing behavior.
	return d.cg.db.ForEachNodeChannel(ctx, gossipV1, nodePub, cb, reset)
}

// ForEachNodeCached iterates through all nodes using the cache if available.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) ForEachNodeCached(ctx context.Context, withAddrs bool,
	cb func(ctx context.Context, node route.Vertex, addrs []net.Addr,
		chans map[uint64]*DirectedChannel) error,
	reset func()) error {

	return d.cg.ForEachNodeCached(ctx, withAddrs, cb, reset)
}

// FetchChannelEdgesByID looks up the two directed edges for the channel
// identified by channel ID, preferring the highest gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) FetchChannelEdgesByID(ctx context.Context,
	chanID uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return d.cg.FetchChannelEdgesByID(ctx, chanID)
}

// FetchChannelEdgesByOutpoint looks up the two directed edges for the channel
// identified by the funding outpoint, preferring the highest gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) FetchChannelEdgesByOutpoint(ctx context.Context,
	op *wire.OutPoint) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return d.cg.FetchChannelEdgesByOutpoint(ctx, op)
}

// FetchNode looks up a target node by its identity public key, preferring
// the highest gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) FetchNode(ctx context.Context,
	nodePub route.Vertex) (*models.Node, error) {

	return fetchPreferHighest(
		func(v lnwire.GossipVersion) (*models.Node, error) {
			return d.cg.db.FetchNode(ctx, v, nodePub)
		},
		ErrGraphNodeNotFound,
	)
}

// LookupAlias returns the alias as advertised by the target node, preferring
// the highest gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	return fetchPreferHighest(
		func(v lnwire.GossipVersion) (string, error) {
			return d.cg.db.LookupAlias(ctx, v, pub)
		},
		ErrNodeAliasNotFound,
	)
}

// IsPublicNode determines whether the node with the given public key is seen
// as a public node in the graph. A node is considered public if it has
// announced channels in any gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) IsPublicNode(ctx context.Context,
	pubKey [33]byte) (bool, error) {

	for _, v := range gossipVersionsDescending {
		public, err := d.cg.db.IsPublicNode(ctx, v, pubKey)
		if err != nil {
			if errors.Is(err, ErrVersionNotSupportedForKVDB) {
				continue
			}

			return false, err
		}

		if public {
			return true, nil
		}
	}

	return false, nil
}

// AddrsForNode returns all known addresses for the target node public key,
// preferring the highest gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	type result struct {
		known bool
		addrs []net.Addr
	}

	r, err := fetchPreferHighest(
		func(v lnwire.GossipVersion) (result, error) {
			known, addrs, err := d.cg.db.AddrsForNode(
				ctx, v, nodePub,
			)
			if err != nil {
				return result{}, err
			}

			if !known {
				return result{}, ErrGraphNodeNotFound
			}

			return result{known: known, addrs: addrs}, nil
		},
		ErrGraphNodeNotFound,
	)
	if err != nil {
		// If the node wasn't found in any version, return (false, nil)
		// rather than an error, matching the original AddrsForNode
		// semantics.
		if errors.Is(err, ErrGraphNodeNotFound) {
			return false, nil, nil
		}

		return false, nil, err
	}

	return r.known, r.addrs, nil
}

// GraphSession provides the callback with access to a NodeTraverser.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) GraphSession(ctx context.Context,
	cb func(graph NodeTraverser) error, reset func()) error {

	return d.cg.GraphSession(ctx, cb, reset)
}

// SourceNode returns the source node of the graph, preferring the highest
// gossip version.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) SourceNode(ctx context.Context) (*models.Node,
	error) {

	return fetchPreferHighest(
		func(v lnwire.GossipVersion) (*models.Node, error) {
			return d.cg.db.SourceNode(ctx, v)
		},
		ErrGraphNodeNotFound,
	)
}

// ForEachSourceNodeChannel iterates through all channels of the source node.
//
// NOTE: this is part of the GraphSource interface.
func (d *DBGraphSource) ForEachSourceNodeChannel(ctx context.Context,
	cb func(chanPoint wire.OutPoint, havePolicy bool,
		otherNode *models.Node) error,
	reset func()) error {

	// TODO(elle): add cross-version iteration support once the store
	// provides a unified API for iterating across gossip versions. For
	// now we iterate v1 only, matching the existing behavior.
	return d.cg.db.ForEachSourceNodeChannel(ctx, gossipV1, cb, reset)
}

// gossipVersionsDescending lists gossip versions in descending order so that
// the highest (newest) version is tried first.
var gossipVersionsDescending = []lnwire.GossipVersion{
	gossipV2, gossipV1,
}

// fetchPreferHighest is a helper that tries a versioned fetch function against
// gossip versions in descending order. It returns the result from the highest
// version that succeeds. If a version returns the given notFoundErr (or
// ErrVersionNotSupportedForKVDB), the next version is tried. Any other error
// is returned immediately.
func fetchPreferHighest[T any](fetch func(lnwire.GossipVersion) (T, error),
	notFoundErr error) (T, error) {

	var zero T

	for _, v := range gossipVersionsDescending {
		result, err := fetch(v)
		if err == nil {
			return result, nil
		}

		if errors.Is(err, notFoundErr) ||
			errors.Is(err, ErrVersionNotSupportedForKVDB) {

			continue
		}

		return zero, err
	}

	return zero, notFoundErr
}
