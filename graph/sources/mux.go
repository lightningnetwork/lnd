package sources

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/graph/session"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Mux is an implementation of the GraphSource interface that multiplexes
// requests to both a local and remote graph source.
type Mux struct {
	// remote is the remote GraphSource which is expected to have the most
	// complete view of the graph.
	remote GraphSource

	// local is this nodes local GraphSource which is expected to have some
	// information that the remote source can not be queried for such as
	// information pertaining to private channels.
	local GraphSource

	// getLocalSource can be used to fetch the local nodes pub key.
	getLocalSource func() (route.Vertex, error)

	// srcPub is a cached version of the local nodes own pub key bytes. The
	// mu mutex should be used when accessing this field.
	srcPub *route.Vertex
	mu     sync.Mutex
}

// A compile-time check to ensure that Mux implements GraphSource.
var _ GraphSource = (*Mux)(nil)

// NewMux creates a new Mux instance.
func NewMux(local, remote GraphSource,
	getLocalSource func() (route.Vertex, error)) *Mux {

	return &Mux{
		local:          local,
		remote:         remote,
		getLocalSource: getLocalSource,
	}
}

// NetworkStats returns statistics concerning the current state of the known
// channel graph within the network. This method only queries the remote source
// and so will be mostly correct but may miss some information that the local
// source has regarding private channels.
//
// NOTE: this is part of the sources.GraphSource interface.
func (g *Mux) NetworkStats(ctx context.Context) (
	*models.NetworkStats, error) {

	return g.remote.NetworkStats(ctx)
}

// GraphBootstrapper returns a NetworkPeerBootstrapper instance backed by a
// remote graph source. It can be queried for a set of random node addresses
// that can be used to establish new connections.
//
// NOTE: this is part of the sources.GraphSource interface.
func (g *Mux) GraphBootstrapper(ctx context.Context) (
	discovery.NetworkPeerBootstrapper, error) {

	return g.remote.GraphBootstrapper(ctx)
}

// BetweennessCentrality queries the remote graph client for the normalised and
// non-normalised betweenness centrality for each node in the graph. Only the
// remote source is consulted for this information.
//
// NOTE: This is a part of the sources.GraphSource interface.
func (g *Mux) BetweennessCentrality(ctx context.Context) (
	map[autopilot.NodeID]*models.BetweennessCentrality, error) {

	return g.remote.BetweennessCentrality(ctx)
}

// NewPathFindTx returns a new read transaction that can be used with other read
// calls to the backing graph sources.
//
// NOTE: this is part of the session.ReadOnlyGraph interface.
func (g *Mux) NewPathFindTx(ctx context.Context) (session.RTx,
	error) {

	return newRTxSet(ctx, g.local, g.remote)
}

// ForEachNodeDirectedChannel iterates through all channels of a given
// node, executing the passed callback on the directed edge representing
// the channel and its incoming policy.
//
// If the node in question is the local node, then only the local node is
// queried since it will know all channels that it owns.
//
// Otherwise, we still query the local node in case the node in question is a
// peer with whom the local node has a private channel to. In that case we want
// to make sure to run the call-back on these directed channels since the remote
// node may not know of this channel. Finally, we call the remote node but skip
// any channels we have already handled.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) ForEachNodeDirectedChannel(ctx context.Context,
	tx session.RTx, node route.Vertex,
	cb func(channel *graphdb.DirectedChannel) error) error {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return err
	}

	lTx, rTx, err := extractRTxSet(tx)
	if err != nil {
		return err
	}

	// If we are the source node, we know all our channels, so just use
	// local DB.
	if bytes.Equal(srcPub[:], node[:]) {
		return g.local.ForEachNodeDirectedChannel(ctx, lTx, node, cb)
	}

	// Call our local DB to collect any private channels we have.
	handledPeerChans := make(map[uint64]bool)
	err = g.local.ForEachNodeDirectedChannel(ctx, lTx, node,
		func(channel *graphdb.DirectedChannel) error {
			// If the other node is not us, we don't need to handle
			// it here since the remote node will handle it later.
			if !bytes.Equal(channel.OtherNode[:], srcPub[:]) {
				return nil
			}

			// Else, we call the call back ourselves on this
			// channel and mark that we have handled it.
			handledPeerChans[channel.ChannelID] = true

			return cb(channel)
		})
	if err != nil {
		return err
	}

	return g.remote.ForEachNodeDirectedChannel(ctx, rTx, node,
		func(channel *graphdb.DirectedChannel) error {
			// Skip any we have already handled.
			if handledPeerChans[channel.ChannelID] {
				return nil
			}

			return cb(channel)
		},
	)
}

// FetchNodeFeatures returns the features of a given node. If no features are
// known for the node, an empty feature vector is returned. If the node in
// question is the local node, then only the local source is queried. Otherwise,
// the remote is queried first and only if it returns an empty feature vector
// is the local source queried.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) FetchNodeFeatures(ctx context.Context,
	tx session.RTx, node route.Vertex) (*lnwire.FeatureVector, error) {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return nil, err
	}

	lTx, rTx, err := extractRTxSet(tx)
	if err != nil {
		return nil, err
	}

	// If we are the source node, we know our own features, so just use
	// local DB.
	if bytes.Equal(srcPub[:], node[:]) {
		return g.local.FetchNodeFeatures(ctx, lTx, node)
	}

	// Otherwise, first query the remote source since it will have the most
	// up-to-date information. If it returns an empty feature vector, then
	// we also consult the local source.
	feats, err := g.remote.FetchNodeFeatures(ctx, lTx, node)
	if err != nil {
		return nil, err
	}

	if !feats.IsEmpty() {
		return feats, nil
	}

	return g.local.FetchNodeFeatures(ctx, rTx, node)
}

// ForEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early. No error is returned if no nodes are found. First, the remote source
// is queried since it is assumed to have the most up-to-date graph information.
// Since the local source may have information about private channel peers, it
// is also queried for any missing nodes. The local source node is handled only
// by the local source since it will have the most up-to-date information
// regarding the local node.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) ForEachNode(ctx context.Context,
	cb func(*models.LightningNode) error) error {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return err
	}

	handled := make(map[route.Vertex]struct{})
	err = g.remote.ForEachNode(ctx, func(node *models.LightningNode) error {
		// We leave the handling of the local node to the local source.
		if bytes.Equal(srcPub[:], node.PubKeyBytes[:]) {
			return nil
		}

		handled[node.PubKeyBytes] = struct{}{}

		return cb(node)
	})
	if err != nil {
		return err
	}

	return g.local.ForEachNode(ctx,
		func(node *models.LightningNode) error {
			if _, ok := handled[node.PubKeyBytes]; ok {
				return nil
			}

			return cb(node)
		},
	)
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then ErrGraphNodeNotFound is
// returned. An optional transaction may be provided. If none is provided, then
// a new one will be created. If the node is not found in either source, then
// graphdb.ErrGraphNodeNotFound is returned. If the node in question is the
// local node, then only the local source is queried since it will have the most
// up-to-date information regarding the local node. Otherwise, the remote is
// queried first and only if the node is not found there is the local source
// checked.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) FetchLightningNode(ctx context.Context,
	nodePub route.Vertex) (*models.LightningNode, error) {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return nil, err
	}

	// Special case if the node in question is the local node.
	if bytes.Equal(srcPub[:], nodePub[:]) {
		return g.local.FetchLightningNode(ctx, nodePub)
	}

	node, err := g.remote.FetchLightningNode(ctx, nodePub)
	if err == nil || !errors.Is(err, graphdb.ErrGraphNodeNotFound) {
		return node, err
	}

	return g.local.FetchLightningNode(ctx, nodePub)
}

// ForEachNodeChannel iterates through all channels of the given node,
// executing the passed callback with an edge info structure and the policies
// of each end of the channel. The first edge policy is the outgoing edge *to*
// the connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller. No error is returned
// if the node is not found. If the node in question is the local node, then
// only the local source is queried since it will have the most up-to-date
// information regarding the local node. Otherwise, the remote is queried
// first and only if the node is not found there is the local source checked.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return err
	}

	if bytes.Equal(srcPub[:], nodePub[:]) {
		return g.local.ForEachNodeChannel(ctx, nodePub, cb)
	}

	// First query our own db since we may have chan info that our remote
	// does not know of (regarding our selves or our channel peers).
	handledChans := make(map[uint64]bool)
	err = g.local.ForEachNodeChannel(ctx, nodePub, func(
		info *models.ChannelEdgeInfo, policy *models.ChannelEdgePolicy,
		policy2 *models.ChannelEdgePolicy) error {

		handledChans[info.ChannelID] = true

		return cb(info, policy, policy2)
	})
	if err != nil {
		return err
	}

	return g.remote.ForEachNodeChannel(
		ctx, nodePub, func(info *models.ChannelEdgeInfo,
			p1 *models.ChannelEdgePolicy,
			p2 *models.ChannelEdgePolicy) error {

			if handledChans[info.ChannelID] {
				return nil
			}

			return cb(info, p1, p2)
		},
	)
}

// FetchChannelEdgesByID attempts to look up the two directed edges for the
// channel identified by the channel ID. If the channel can't be found, then
// graphdb.ErrEdgeNotFound is returned. If the channel belongs to the local
// source, it is queried for the info. Otherwise, the remote source is queried,
// and we default to the local source if the remote source does not have the
// channel.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) FetchChannelEdgesByID(ctx context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return nil, nil, nil, err
	}

	// We query the local source first in case this channel belongs to the
	// local node.
	localInfo, localP1, localP2, localErr := g.local.FetchChannelEdgesByID(
		ctx, chanID,
	)
	if localErr != nil && !errors.Is(localErr, graphdb.ErrEdgeNotFound) {
		return nil, nil, nil, localErr
	}

	// If the local node is one of the channel owners, then it will have
	// up-to-date information regarding this channel.
	if !errors.Is(localErr, graphdb.ErrEdgeNotFound) &&
		(bytes.Equal(localInfo.NodeKey1Bytes[:], srcPub[:]) ||
			bytes.Equal(localInfo.NodeKey2Bytes[:], srcPub[:])) {

		return localInfo, localP1, localP2, nil
	}

	// Otherwise, we query the remote source since it is the most likely to
	// have the most up-to-date information.
	remoteInfo, remoteP1, remoteP2, err := g.remote.FetchChannelEdgesByID(
		ctx, chanID,
	)
	if err == nil || !errors.Is(err, graphdb.ErrEdgeNotFound) {
		return remoteInfo, remoteP1, remoteP2, err
	}

	// If the remote source does not have the channel, we return the local
	// information if it was found.
	return localInfo, localP1, localP2, localErr
}

// IsPublicNode is a helper method that determines whether the node with the
// given public key is seen as a public node in the graph from the graph's
// source node's point of view. This first checks the local node and then the
// remote if the node is not seen as public by the local node.
// If this node is unknown by both sources, then graphdb.ErrGraphNodeNotFound is
// returned.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) IsPublicNode(ctx context.Context, pubKey [33]byte) (
	bool, error) {

	isPublic, err := g.local.IsPublicNode(ctx, pubKey)
	if err == nil && isPublic {
		return isPublic, err
	}
	if err != nil && !errors.Is(err, graphdb.ErrGraphNodeNotFound) {
		return false, err
	}

	return g.remote.IsPublicNode(ctx, pubKey)
}

// FetchChannelEdgesByOutpoint returns the channel edge info and most recent
// channel edge policies for a given outpoint. If the channel can't be found by
// either source, then graphdb.ErrEdgeNotFound is returned. If the channel can't
// be found, then graphdb.ErrEdgeNotFound is returned. If the channel belongs to
// the local source, it is queried for the info. Otherwise, the remote source is
// queried, and we default to the local source if the remote source does not
// have the channel.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) FetchChannelEdgesByOutpoint(ctx context.Context,
	point *wire.OutPoint) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return nil, nil, nil, err
	}

	// We query the local source first in case this channel belongs to the
	// local node.
	localInfo, localP1, localP2, localErr := g.local.FetchChannelEdgesByOutpoint( //nolint:lll
		ctx, point,
	)
	if localErr != nil && !errors.Is(localErr, graphdb.ErrEdgeNotFound) {
		return nil, nil, nil, localErr
	}

	// If the local node is one of the channel owners, then it will have
	// up-to-date information regarding this channel.
	if !errors.Is(localErr, graphdb.ErrEdgeNotFound) &&
		(bytes.Equal(localInfo.NodeKey1Bytes[:], srcPub[:]) ||
			bytes.Equal(localInfo.NodeKey2Bytes[:], srcPub[:])) {

		return localInfo, localP1, localP2, nil
	}

	// Otherwise, we query the remote source since it is the most likely to
	// have the most up-to-date information.
	remoteInfo, remoteP1, remoteP2, err := g.remote.FetchChannelEdgesByOutpoint( //nolint:lll
		ctx, point,
	)
	if err == nil || !errors.Is(err, graphdb.ErrEdgeNotFound) {
		return remoteInfo, remoteP1, remoteP2, err
	}

	// If the remote source does not have the channel, we return the local
	// information if it was found.
	return localInfo, localP1, localP2, localErr
}

// AddrsForNode returns all known addresses for the target node public key. The
// returned boolean must indicate if the given node is unknown to the backing
// source. This merges the results from both the local and remote source.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	// Check both the local and remote sources and merge the results.
	return channeldb.NewMultiAddrSource(
		g.local, g.remote,
	).AddrsForNode(ctx, nodePub)
}

// ForEachChannel iterates through all the channel edges stored within the graph
// and invokes the passed callback for each edge. If the callback returns an
// error, then the transaction is aborted and the iteration stops early. An
// edge's policy structs may be nil if the ChannelUpdate in question has not yet
// been received for the channel. No error is returned if no channels are found.
// First, this will iterate through all channels owned by the local source since
// the local source will have the most up-to-date information on these channels.
// The remote source is used for all other channels.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return err
	}

	ourChans := make(map[uint64]bool)
	err = g.local.ForEachNodeChannel(ctx, srcPub, func(
		info *models.ChannelEdgeInfo, policy *models.ChannelEdgePolicy,
		policy2 *models.ChannelEdgePolicy) error {

		ourChans[info.ChannelID] = true

		return cb(info, policy, policy2)
	})
	if err != nil {
		return err
	}

	return g.remote.ForEachChannel(ctx, func(info *models.ChannelEdgeInfo,
		policy *models.ChannelEdgePolicy,
		policy2 *models.ChannelEdgePolicy) error {

		if ourChans[info.ChannelID] {
			return nil
		}

		return cb(info, policy, policy2)
	})
}

// HasLightningNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean and a nil error.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) HasLightningNode(ctx context.Context,
	nodePub [33]byte) (time.Time, bool, error) {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return time.Time{}, false, err
	}

	// If this is the local node, then it will have the most up-to-date
	// info and so there is no reason to query the remote node.
	if bytes.Equal(srcPub[:], nodePub[:]) {
		return g.local.HasLightningNode(ctx, nodePub)
	}

	// Else, we query the remote node first since it is the most likely to
	// have the most up-to-date timestamp.
	timeStamp, remoteHas, err := g.remote.HasLightningNode(ctx, nodePub)
	if err != nil {
		return timeStamp, false, err
	}
	if remoteHas {
		return timeStamp, true, nil
	}

	// Fall back to querying the local node.
	return g.local.HasLightningNode(ctx, nodePub)
}

// LookupAlias attempts to return the alias as advertised by the target node.
// graphdb.ErrNodeAliasNotFound is returned if the alias is not found.
//
// NOTE: this is part of the GraphSource interface.
func (g *Mux) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	srcPub, err := g.selfNodePub()
	if err != nil {
		return "", err
	}

	// If this is the local node, then it will have the most up-to-date
	// alias, so there is no reason to query the remote source.
	if bytes.Equal(srcPub[:], pub.SerializeCompressed()) {
		return g.local.LookupAlias(ctx, pub)
	}

	// Otherwise, we check with the remote and then fallback to the local.
	alias, err := g.remote.LookupAlias(ctx, pub)
	if err == nil || !errors.Is(err, graphdb.ErrNodeAliasNotFound) {
		return alias, err
	}

	return g.local.LookupAlias(ctx, pub)
}

// selfNodePub fetches the local nodes pub key. It first checks the cached value
// and if non exists, it queries the database.
func (g *Mux) selfNodePub() (route.Vertex, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.srcPub != nil {
		return *g.srcPub, nil
	}

	pub, err := g.getLocalSource()
	if err != nil {
		return route.Vertex{}, err
	}

	g.srcPub = &pub

	return *g.srcPub, nil
}

type rTxConstructor interface {
	NewPathFindTx(ctx context.Context) (session.RTx, error)
}

// rTxSet is an implementation of graphdb.RTx which is backed a read transaction
// for the local graph and one for a remote graph.
type rTxSet struct {
	lRTx session.RTx
	rRTx session.RTx
}

// newMultiRTx uses the given rTxConstructors to begin a read transaction for
// each and returns a multiRTx that represents this open set of transactions.
func newRTxSet(ctx context.Context, localConstructor,
	remoteConstructor rTxConstructor) (*rTxSet, error) {

	localRTx, err := localConstructor.NewPathFindTx(ctx)
	if err != nil {
		return nil, err
	}

	remoteRTx, err := remoteConstructor.NewPathFindTx(ctx)
	if err != nil {
		_ = localRTx.Close()

		return nil, err
	}

	return &rTxSet{
		lRTx: localRTx,
		rRTx: remoteRTx,
	}, nil
}

// Close closes all the transactions held by multiRTx.
//
// NOTE: this is part of the graphdb.RTx interface.
func (s *rTxSet) Close() error {
	var returnErr error

	if s.lRTx != nil {
		if err := s.lRTx.Close(); err != nil {
			returnErr = err
		}
	}

	if s.rRTx != nil {
		if err := s.rRTx.Close(); err != nil {
			returnErr = err
		}
	}

	return returnErr
}

// MustImplementRTx is a helper method that ensures that the rTxSet type
// implements the RTx interface.
//
// NOTE: this is part of the graphdb.RTx interface.
func (s *rTxSet) MustImplementRTx() {}

// A compile-time check to ensure that multiRTx implements graphdb.RTx.
var _ session.RTx = (*rTxSet)(nil)

// extractRTxSet is a helper function that casts an RTx into a rTxSet returns
// the local and remote RTxs respectively.
func extractRTxSet(tx session.RTx) (session.RTx, session.RTx, error) {
	if tx == nil {
		return nil, nil, nil
	}

	set, ok := tx.(*rTxSet)
	if !ok {
		return nil, nil, fmt.Errorf("expected a rTxSet, got %T", tx)
	}

	return set.lRTx, set.rRTx, nil
}
