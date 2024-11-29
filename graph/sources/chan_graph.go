package sources

import (
	"context"
	"fmt"
	"math"
	"net"
	"runtime"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
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

// NetworkStats returns statistics concerning the current state of the known
// channel graph within the network.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) NetworkStats(_ context.Context) (*models.NetworkStats,
	error) {

	var (
		numNodes             uint32
		numChannels          uint32
		maxChanOut           uint32
		totalNetworkCapacity btcutil.Amount
		minChannelSize       btcutil.Amount = math.MaxInt64
		maxChannelSize       btcutil.Amount
		medianChanSize       btcutil.Amount
	)

	// We'll use this map to de-duplicate channels during our traversal.
	// This is needed since channels are directional, so there will be two
	// edges for each channel within the graph.
	seenChans := make(map[uint64]struct{})

	// We also keep a list of all encountered capacities, in order to
	// calculate the median channel size.
	var allChans []btcutil.Amount

	// We'll run through all the known nodes in the within our view of the
	// network, tallying up the total number of nodes, and also gathering
	// each node so we can measure the graph diameter and degree stats
	// below.
	err := s.db.ForEachNodeCached(func(node route.Vertex,
		edges map[uint64]*graphdb.DirectedChannel) error {

		// Increment the total number of nodes with each iteration.
		numNodes++

		// For each channel we'll compute the out degree of each node,
		// and also update our running tallies of the min/max channel
		// capacity, as well as the total channel capacity. We pass
		// through the DB transaction from the outer view so we can
		// re-use it within this inner view.
		var outDegree uint32
		for _, edge := range edges {
			// Bump up the out degree for this node for each
			// channel encountered.
			outDegree++

			// If we've already seen this channel, then we'll
			// return early to ensure that we don't double-count
			// stats.
			if _, ok := seenChans[edge.ChannelID]; ok {
				return nil
			}

			// Compare the capacity of this channel against the
			// running min/max to see if we should update the
			// extrema.
			chanCapacity := edge.Capacity
			if chanCapacity < minChannelSize {
				minChannelSize = chanCapacity
			}
			if chanCapacity > maxChannelSize {
				maxChannelSize = chanCapacity
			}

			// Accumulate the total capacity of this channel to the
			// network wide-capacity.
			totalNetworkCapacity += chanCapacity

			numChannels++

			seenChans[edge.ChannelID] = struct{}{}
			allChans = append(allChans, edge.Capacity)
		}

		// Finally, if the out degree of this node is greater than what
		// we've seen so far, update the maxChanOut variable.
		if outDegree > maxChanOut {
			maxChanOut = outDegree
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Find the median.
	medianChanSize = autopilot.Median(allChans)

	// If we don't have any channels, then reset the minChannelSize to zero
	// to avoid outputting NaN in encoded JSON.
	if numChannels == 0 {
		minChannelSize = 0
	}

	// Graph diameter.
	channelGraph := autopilot.ChannelGraphFromCachedDatabase(s.db)
	simpleGraph, err := autopilot.NewSimpleGraph(channelGraph)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	diameter := simpleGraph.DiameterRadialCutoff()

	log.Infof("Elapsed time for diameter (%d) calculation: %v", diameter,
		time.Since(start))

	// Query the graph for the current number of zombie channels.
	numZombies, err := s.db.NumZombies()
	if err != nil {
		return nil, err
	}

	return &models.NetworkStats{
		Diameter:             diameter,
		MaxChanOut:           maxChanOut,
		NumNodes:             numNodes,
		NumChannels:          numChannels,
		TotalNetworkCapacity: totalNetworkCapacity,
		MinChanSize:          minChannelSize,
		MaxChanSize:          maxChannelSize,
		MedianChanSize:       medianChanSize,
		NumZombies:           numZombies,
	}, nil
}

// BetweennessCentrality computes the normalised and non-normalised betweenness
// centrality for each node in the graph.
//
// NOTE: this is part of the GraphSource interface.
func (s *DBSource) BetweennessCentrality(_ context.Context) (
	map[autopilot.NodeID]*models.BetweennessCentrality, error) {

	channelGraph := autopilot.ChannelGraphFromDatabase(s.db)
	centralityMetric, err := autopilot.NewBetweennessCentralityMetric(
		runtime.NumCPU(),
	)
	if err != nil {
		return nil, err
	}

	if err := centralityMetric.Refresh(channelGraph); err != nil {
		return nil, err
	}

	centrality := make(map[autopilot.NodeID]*models.BetweennessCentrality)

	for nodeID, val := range centralityMetric.GetMetric(true) {
		centrality[nodeID] = &models.BetweennessCentrality{
			Normalized: val,
		}
	}

	for nodeID, val := range centralityMetric.GetMetric(false) {
		if _, ok := centrality[nodeID]; !ok {
			centrality[nodeID] = &models.BetweennessCentrality{
				Normalized: val,
			}

			continue
		}
		centrality[nodeID].NonNormalized = val
	}

	return centrality, nil
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
