package routing

import (
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// vertexDecay is the decay period of colored vertexes added to
	// missionControl. Once vertexDecay passes after an entry has been
	// added to the prune view, it is garbage collected. This value is
	// larger than edgeDecay as an edge failure typical indicates an
	// unbalanced channel, while a vertex failure indicates a node is not
	// online and active.
	vertexDecay = time.Duration(time.Minute * 5)

	// edgeDecay is the decay period of colored edges added to
	// missionControl. Once edgeDecay passed after an entry has been added,
	// it is garbage collected. This value is smaller than vertexDecay as
	// an edge related failure during payment sending typically indicates
	// that a channel was unbalanced, a condition which may quickly change.
	//
	// TODO(roasbeef): instead use random delay on each?
	edgeDecay = time.Duration(time.Second * 5)
)

// missionControl contains state which summarizes the past attempts of HTLC
// routing by external callers when sending payments throughout the network.
// missionControl remembers the outcome of these past routing attempts (success
// and failure), and is able to provide hints/guidance to future HTLC routing
// attempts.  missionControl maintains a decaying network view of the
// edges/vertexes that should be marked as "pruned" during path finding. This
// graph view acts as a shared memory during HTLC payment routing attempts.
// With each execution, if an error is encountered, based on the type of error
// and the location of the error within the route, an edge or vertex is added
// to the view. Later sending attempts will then query the view for all the
// vertexes/edges that should be ignored. Items in the view decay after a set
// period of time, allowing the view to be dynamic w.r.t network changes.
type missionControl struct {
	// failedEdges maps a short channel ID to be pruned, to the time that
	// it was added to the prune view. Edges are added to this map if a
	// caller reports to missionControl a failure localized to that edge
	// when sending a payment.
	failedEdges map[edgeLocator]time.Time

	// failedVertexes maps a node's public key that should be pruned, to
	// the time that it was added to the prune view. Vertexes are added to
	// this map if a caller reports to missionControl a failure localized
	// to that particular vertex.
	failedVertexes map[Vertex]time.Time

	graph *channeldb.ChannelGraph

	selfNode *channeldb.LightningNode

	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi

	sync.Mutex

	// TODO(roasbeef): further counters, if vertex continually unavailable,
	// add to another generation

	// TODO(roasbeef): also add favorable metrics for nodes
}

// newMissionControl returns a new instance of missionControl.
//
// TODO(roasbeef): persist memory
func newMissionControl(g *channeldb.ChannelGraph, selfNode *channeldb.LightningNode,
	qb func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi) *missionControl {

	return &missionControl{
		failedEdges:    make(map[edgeLocator]time.Time),
		failedVertexes: make(map[Vertex]time.Time),
		selfNode:       selfNode,
		queryBandwidth: qb,
		graph:          g,
	}
}

// graphPruneView is a filter of sorts that path finding routines should
// consult during the execution. Any edges or vertexes within the view should
// be ignored during path finding. The contents of the view reflect the current
// state of the wider network from the PoV of mission control compiled via HTLC
// routing attempts in the past.
type graphPruneView struct {
	edges map[edgeLocator]struct{}

	vertexes map[Vertex]struct{}
}

// GraphPruneView returns a new graphPruneView instance which is to be
// consulted during path finding. If a vertex/edge is found within the returned
// prune view, it is to be ignored as a goroutine has had issues routing
// through it successfully. Within this method the main view of the
// missionControl is garbage collected as entries are detected to be "stale".
func (m *missionControl) GraphPruneView() graphPruneView {
	// First, we'll grab the current time, this value will be used to
	// determine if an entry is stale or not.
	now := time.Now()

	m.Lock()

	// For each of the vertexes that have been added to the prune view, if
	// it is now "stale", then we'll ignore it and avoid adding it to the
	// view we'll return.
	vertexes := make(map[Vertex]struct{})
	for vertex, pruneTime := range m.failedVertexes {
		if now.Sub(pruneTime) >= vertexDecay {
			log.Tracef("Pruning decayed failure report for vertex %v "+
				"from Mission Control", vertex)

			delete(m.failedVertexes, vertex)
			continue
		}

		vertexes[vertex] = struct{}{}
	}

	// We'll also do the same for edges, but use the edgeDecay this time
	// rather than the decay for vertexes.
	edges := make(map[edgeLocator]struct{})
	for edge, pruneTime := range m.failedEdges {
		if now.Sub(pruneTime) >= edgeDecay {
			log.Tracef("Pruning decayed failure report for edge %v "+
				"from Mission Control", edge)

			delete(m.failedEdges, edge)
			continue
		}

		edges[edge] = struct{}{}
	}

	m.Unlock()

	log.Debugf("Mission Control returning prune view of %v edges, %v "+
		"vertexes", len(edges), len(vertexes))

	return graphPruneView{
		edges:    edges,
		vertexes: vertexes,
	}
}

// NewPaymentSession creates a new payment session backed by the latest prune
// view from Mission Control. An optional set of routing hints can be provided
// in order to populate additional edges to explore when finding a path to the
// payment's destination.
func (m *missionControl) NewPaymentSession(routeHints [][]zpay32.HopHint,
	target *btcec.PublicKey) (*paymentSession, error) {

	viewSnapshot := m.GraphPruneView()

	edges := make(map[Vertex][]*channeldb.ChannelEdgePolicy)

	// Traverse through all of the available hop hints and include them in
	// our edges map, indexed by the public key of the channel's starting
	// node.
	for _, routeHint := range routeHints {
		// If multiple hop hints are provided within a single route
		// hint, we'll assume they must be chained together and sorted
		// in forward order in order to reach the target successfully.
		for i, hopHint := range routeHint {
			// In order to determine the end node of this hint,
			// we'll need to look at the next hint's start node. If
			// we've reached the end of the hints list, we can
			// assume we've reached the destination.
			endNode := &channeldb.LightningNode{}
			if i != len(routeHint)-1 {
				endNode.AddPubKey(routeHint[i+1].NodeID)
			} else {
				endNode.AddPubKey(target)
			}

			// Finally, create the channel edge from the hop hint
			// and add it to list of edges corresponding to the node
			// at the start of the channel.
			edge := &channeldb.ChannelEdgePolicy{
				Node:      endNode,
				ChannelID: hopHint.ChannelID,
				FeeBaseMSat: lnwire.MilliSatoshi(
					hopHint.FeeBaseMSat,
				),
				FeeProportionalMillionths: lnwire.MilliSatoshi(
					hopHint.FeeProportionalMillionths,
				),
				TimeLockDelta: hopHint.CLTVExpiryDelta,
			}

			v := NewVertex(hopHint.NodeID)
			edges[v] = append(edges[v], edge)
		}
	}

	// We'll also obtain a set of bandwidthHints from the lower layer for
	// each of our outbound channels. This will allow the path finding to
	// skip any links that aren't active or just don't have enough
	// bandwidth to carry the payment.
	sourceNode, err := m.graph.SourceNode()
	if err != nil {
		return nil, err
	}
	bandwidthHints, err := generateBandwidthHints(
		sourceNode, m.queryBandwidth,
	)
	if err != nil {
		return nil, err
	}

	return &paymentSession{
		pruneViewSnapshot:    viewSnapshot,
		additionalEdges:      edges,
		bandwidthHints:       bandwidthHints,
		errFailedPolicyChans: make(map[edgeLocator]struct{}),
		mc:                   m,
	}, nil
}

// NewPaymentSessionFromRoutes creates a new paymentSession instance that will
// skip all path finding, and will instead utilize a set of pre-built routes.
// This constructor allows callers to specify their own routes which can be
// used for things like channel rebalancing, and swaps.
func (m *missionControl) NewPaymentSessionFromRoutes(routes []*Route) *paymentSession {
	return &paymentSession{
		pruneViewSnapshot:    m.GraphPruneView(),
		haveRoutes:           true,
		preBuiltRoutes:       routes,
		errFailedPolicyChans: make(map[edgeLocator]struct{}),
		mc:                   m,
	}
}

// generateBandwidthHints is a helper function that's utilized the main
// findPath function in order to obtain hints from the lower layer w.r.t to the
// available bandwidth of edges on the network. Currently, we'll only obtain
// bandwidth hints for the edges we directly have open ourselves. Obtaining
// these hints allows us to reduce the number of extraneous attempts as we can
// skip channels that are inactive, or just don't have enough bandwidth to
// carry the payment.
func generateBandwidthHints(sourceNode *channeldb.LightningNode,
	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi) (map[uint64]lnwire.MilliSatoshi, error) {

	// First, we'll collect the set of outbound edges from the target
	// source node.
	var localChans []*channeldb.ChannelEdgeInfo
	err := sourceNode.ForEachChannel(nil, func(tx *bbolt.Tx,
		edgeInfo *channeldb.ChannelEdgeInfo,
		_, _ *channeldb.ChannelEdgePolicy) error {

		localChans = append(localChans, edgeInfo)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Now that we have all of our outbound edges, we'll populate the set
	// of bandwidth hints, querying the lower switch layer for the most up
	// to date values.
	bandwidthHints := make(map[uint64]lnwire.MilliSatoshi)
	for _, localChan := range localChans {
		bandwidthHints[localChan.ChannelID] = queryBandwidth(localChan)
	}

	return bandwidthHints, nil
}

// ResetHistory resets the history of missionControl returning it to a state as
// if no payment attempts have been made.
func (m *missionControl) ResetHistory() {
	m.Lock()
	m.failedEdges = make(map[edgeLocator]time.Time)
	m.failedVertexes = make(map[Vertex]time.Time)
	m.Unlock()
}
