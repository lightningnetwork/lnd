package sources

import (
	"context"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Compile-time check that Mux implements graphdb.GraphSource.
var _ graphdb.GraphSource = (*Mux)(nil)

// Mux is a GraphSource implementation that multiplexes between a local
// database source and a remote graph source. It provides a unified view of
// the network by combining data from both sources with the following strategy:
//
//   - Own node/channels: always served from local (authoritative)
//   - Other nodes/channels: remote first, supplemented by local
//   - Pathfinding (NodeTraverser): uses the local graph cache
//     (populated from both sources via subscription)
//
// The Mux deduplicates results when iterating both sources to avoid
// presenting the same node or channel twice.
type Mux struct {
	local  graphdb.GraphSource
	remote RemoteGraph

	selfPub route.Vertex
}

// NewMux creates a new Mux that combines the local and remote graph sources.
// The selfPub is the public key of this node, used to determine which data
// should be served from the local source.
func NewMux(local graphdb.GraphSource, remote RemoteGraph,
	selfPub route.Vertex) *Mux {

	return &Mux{
		local:   local,
		remote:  remote,
		selfPub: selfPub,
	}
}

// ForEachNodeDirectedChannel calls the callback for every channel of the given
// node. This is delegated to the local source which uses the graph cache
// (populated from both local and remote data via subscription).
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (m *Mux) ForEachNodeDirectedChannel(ctx context.Context,
	nodePub route.Vertex,
	cb func(channel *graphdb.DirectedChannel) error,
	reset func()) error {

	return m.local.ForEachNodeDirectedChannel(ctx, nodePub, cb, reset)
}

// FetchNodeFeatures returns the features of the given node. Delegated to local
// which uses the graph cache.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (m *Mux) FetchNodeFeatures(ctx context.Context,
	nodePub route.Vertex) (*lnwire.FeatureVector, error) {

	return m.local.FetchNodeFeatures(ctx, nodePub)
}

// ForEachNode iterates through all nodes, combining remote and local sources.
// Remote nodes are iterated first, then local nodes not already seen.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) ForEachNode(ctx context.Context,
	cb func(*models.Node) error, reset func()) error {

	seen := make(map[route.Vertex]struct{})

	var cbErr error
	err := m.remote.ForEachNode(ctx, func(node *models.Node) error {
		seen[node.PubKeyBytes] = struct{}{}

		if err := cb(node); err != nil {
			// Tag callback errors so we can distinguish them
			// from remote transport errors below.
			cbErr = err

			return err
		}

		return nil
	}, func() {
		clear(seen)
		reset()
	})
	if err != nil {
		// If the error came from the caller's callback, propagate
		// it directly rather than falling back to local.
		if cbErr != nil {
			return cbErr
		}

		// If the remote source itself failed, fall back to
		// local-only iteration. Reset accumulated state so the
		// caller starts fresh.
		log.Warnf("Remote ForEachNode failed, falling back to "+
			"local: %v", err)

		clear(seen)
		reset()

		return m.local.ForEachNode(ctx, cb, reset)
	}

	// NOTE: We intentionally do NOT pass the caller's reset to the local
	// iteration. The SQL transaction executor calls reset() at the start
	// of every transaction (not just retries), which would wipe out the
	// remote results already accumulated in the caller's state.
	return m.local.ForEachNode(ctx, func(node *models.Node) error {
		if _, ok := seen[node.PubKeyBytes]; ok {
			return nil
		}

		return cb(node)
	}, func() {})
}

// ForEachChannel iterates through all channel edges, combining remote and
// local sources. Remote channels come first, then local channels not already
// seen (e.g., private channels).
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	reset func()) error {

	seen := make(map[uint64]struct{})

	var cbErr error
	err := m.remote.ForEachChannel(ctx,
		func(info *models.ChannelEdgeInfo,
			p1, p2 *models.ChannelEdgePolicy) error {

			seen[info.ChannelID] = struct{}{}

			if err := cb(info, p1, p2); err != nil {
				cbErr = err

				return err
			}

			return nil
		}, func() {
			clear(seen)
			reset()
		},
	)
	if err != nil {
		if cbErr != nil {
			return cbErr
		}

		log.Warnf("Remote ForEachChannel failed, falling back to "+
			"local: %v", err)

		clear(seen)
		reset()

		return m.local.ForEachChannel(ctx, cb, reset)
	}

	// NOTE: We intentionally do NOT pass the caller's reset to the local
	// iteration. If the local DB retries, calling reset would wipe out
	// the remote edges already added to the caller's results. The local
	// retry only needs to clear the dedup set so remote edges don't
	// get re-added by the local iteration.
	return m.local.ForEachChannel(ctx,
		func(info *models.ChannelEdgeInfo,
			p1, p2 *models.ChannelEdgePolicy) error {

			if _, ok := seen[info.ChannelID]; ok {
				return nil
			}

			return cb(info, p1, p2)
		}, func() {},
	)
}

// ForEachNodeChannel iterates through all channels of the given node. For our
// own node, only local is used (authoritative for private channels). For other
// nodes, remote is queried first, then local supplements with any channels
// the remote doesn't know about.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error,
	reset func()) error {

	// For our own node, always use local (has private channels).
	if nodePub == m.selfPub {
		return m.local.ForEachNodeChannel(ctx, nodePub, cb, reset)
	}

	seen := make(map[uint64]struct{})

	var cbErr error
	err := m.remote.ForEachNodeChannel(ctx, nodePub,
		func(info *models.ChannelEdgeInfo,
			p1, p2 *models.ChannelEdgePolicy) error {

			seen[info.ChannelID] = struct{}{}

			if err := cb(info, p1, p2); err != nil {
				cbErr = err

				return err
			}

			return nil
		}, func() {
			clear(seen)
			reset()
		},
	)
	if err != nil {
		if cbErr != nil {
			return cbErr
		}

		log.Warnf("Remote ForEachNodeChannel failed, falling "+
			"back to local: %v", err)

		clear(seen)
		reset()

		return m.local.ForEachNodeChannel(
			ctx, nodePub, cb, reset,
		)
	}

	// NOTE: We intentionally do NOT pass the caller's reset to the local
	// iteration. See ForEachNode for the rationale.
	return m.local.ForEachNodeChannel(ctx, nodePub,
		func(info *models.ChannelEdgeInfo,
			p1, p2 *models.ChannelEdgePolicy) error {

			if _, ok := seen[info.ChannelID]; ok {
				return nil
			}

			return cb(info, p1, p2)
		}, func() {},
	)
}

// ForEachNodeCached delegates to local since the graph cache is populated
// from both local and remote data.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) ForEachNodeCached(ctx context.Context, withAddrs bool,
	cb func(ctx context.Context, node route.Vertex, addrs []net.Addr,
		chans map[uint64]*graphdb.DirectedChannel) error,
	reset func()) error {

	return m.local.ForEachNodeCached(ctx, withAddrs, cb, reset)
}

// FetchChannelEdgesByID looks up a channel by ID. Tries remote first (broader
// network view), falls back to local (has private channels).
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) FetchChannelEdgesByID(ctx context.Context, chanID uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	info, p1, p2, err := m.remote.FetchChannelEdgesByID(ctx, chanID)
	if err == nil {
		return info, p1, p2, nil
	}

	return m.local.FetchChannelEdgesByID(ctx, chanID)
}

// FetchChannelEdgesByOutpoint looks up a channel by funding outpoint. Tries
// remote first, falls back to local.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) FetchChannelEdgesByOutpoint(ctx context.Context,
	op *wire.OutPoint) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	info, p1, p2, err := m.remote.FetchChannelEdgesByOutpoint(ctx, op)
	if err == nil {
		return info, p1, p2, nil
	}

	return m.local.FetchChannelEdgesByOutpoint(ctx, op)
}

// FetchNode looks up a node. For self, uses local. Otherwise tries remote
// first, falls back to local.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) FetchNode(ctx context.Context,
	nodePub route.Vertex) (*models.Node, error) {

	if nodePub == m.selfPub {
		return m.local.FetchNode(ctx, nodePub)
	}

	node, err := m.remote.FetchNode(ctx, nodePub)
	if err == nil {
		return node, nil
	}

	return m.local.FetchNode(ctx, nodePub)
}

// LookupAlias returns the alias for a node. Tries remote first for non-self
// nodes.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	if route.NewVertex(pub) == m.selfPub {
		return m.local.LookupAlias(ctx, pub)
	}

	alias, err := m.remote.LookupAlias(ctx, pub)
	if err == nil {
		return alias, nil
	}

	return m.local.LookupAlias(ctx, pub)
}

// IsPublicNode checks whether a node is public. A node is public if either
// the remote or local source considers it public.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) IsPublicNode(ctx context.Context,
	pubKey [33]byte) (bool, error) {

	public, err := m.remote.IsPublicNode(ctx, pubKey)
	if err == nil && public {
		return true, nil
	}

	return m.local.IsPublicNode(ctx, pubKey)
}

// AddrsForNode returns addresses for a node. For non-self nodes, tries remote
// first (may have more up-to-date addresses).
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	if route.NewVertex(nodePub) == m.selfPub {
		return m.local.AddrsForNode(ctx, nodePub)
	}

	known, addrs, err := m.remote.AddrsForNode(ctx, nodePub)
	if err == nil && known {
		return known, addrs, nil
	}

	return m.local.AddrsForNode(ctx, nodePub)
}

// GraphSession delegates to local since the graph cache handles pathfinding.
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) GraphSession(ctx context.Context,
	cb func(graph graphdb.NodeTraverser) error, reset func()) error {

	return m.local.GraphSession(ctx, cb, reset)
}

// SourceNode returns the local source node (always from local, we are the
// authority on our own identity).
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) SourceNode(ctx context.Context) (*models.Node, error) {
	return m.local.SourceNode(ctx)
}

// ForEachSourceNodeChannel iterates through our own channels (always from
// local, has private channels).
//
// NOTE: this is part of the graphdb.GraphSource interface.
func (m *Mux) ForEachSourceNodeChannel(ctx context.Context,
	cb func(chanPoint wire.OutPoint, havePolicy bool,
		otherNode *models.Node) error,
	reset func()) error {

	return m.local.ForEachSourceNodeChannel(ctx, cb, reset)
}
