package routing

import (
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
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
	defaultVertexDecay = time.Hour

	// edgeDecay is the decay period of colored edges added to
	// missionControl. Once edgeDecay passed after an entry has been added,
	// it is garbage collected. This value is smaller than vertexDecay as
	// an edge related failure during payment sending typically indicates
	// that a channel was unbalanced, a condition which may quickly change.
	//
	// TODO(roasbeef): instead use random delay on each?
	defaultEdgeDecay = time.Hour

	// hardPruneDuration defines the time window during which pruned nodes
	// and edges will receive zero probability.
	defaultHardPruneDuration = time.Minute
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
	failedEdges map[EdgeLocator]time.Time

	// failedVertexes maps a node's public key that should be pruned, to
	// the time that it was added to the prune view. Vertexes are added to
	// this map if a caller reports to missionControl a failure localized
	// to that particular vertex.
	failedVertexes map[Vertex]time.Time

	graph *channeldb.ChannelGraph

	selfNode *channeldb.LightningNode

	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi

	now func() time.Time

	vertexDecay time.Duration

	// edgeDecay is the decay period of colored edges added to
	// missionControl. Once edgeDecay passed after an entry has been added,
	// it is garbage collected. This value is smaller than vertexDecay as
	// an edge related failure during payment sending typically indicates
	// that a channel was unbalanced, a condition which may quickly change.
	edgeDecay time.Duration

	// hardPruneDuration defines the time window during which pruned nodes
	// and edges will receive zero probability.
	hardPruneDuration time.Duration

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
		failedEdges:       make(map[EdgeLocator]time.Time),
		failedVertexes:    make(map[Vertex]time.Time),
		selfNode:          selfNode,
		queryBandwidth:    qb,
		graph:             g,
		now:               time.Now,
		edgeDecay:         defaultEdgeDecay,
		vertexDecay:       defaultVertexDecay,
		hardPruneDuration: defaultHardPruneDuration,
	}
}

// NewPaymentSession creates a new payment session backed by the latest prune
// view from Mission Control. An optional set of routing hints can be provided
// in order to populate additional edges to explore when finding a path to the
// payment's destination.
func (m *missionControl) NewPaymentSession(routeHints [][]zpay32.HopHint,
	target Vertex) (*paymentSession, error) {

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
				targetPubKey, err := btcec.ParsePubKey(
					target[:], btcec.S256(),
				)
				if err != nil {
					return nil, err
				}
				endNode.AddPubKey(targetPubKey)
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
		additionalEdges:      edges,
		bandwidthHints:       bandwidthHints,
		errFailedPolicyChans: make(map[EdgeLocator]struct{}),
		mc:                   m,
		pathFinder:           findPath,
	}, nil
}

// NewPaymentSessionFromRoutes creates a new paymentSession instance that will
// skip all path finding, and will instead utilize a set of pre-built routes.
// This constructor allows callers to specify their own routes which can be
// used for things like channel rebalancing, and swaps.
func (m *missionControl) NewPaymentSessionFromRoutes(routes []*Route) *paymentSession {
	return &paymentSession{
		haveRoutes:           true,
		preBuiltRoutes:       routes,
		errFailedPolicyChans: make(map[EdgeLocator]struct{}),
		mc:                   m,
		pathFinder:           findPath,
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
	m.failedEdges = make(map[EdgeLocator]time.Time)
	m.failedVertexes = make(map[Vertex]time.Time)
	m.Unlock()
}

// getEdgeProbability is expected to return the success probability of a payment
// of amount amt from fromNode along edge.
func (m *missionControl) getEdgeProbability(fromNode Vertex,
	amt lnwire.MilliSatoshi, edge EdgeLocator,
	capacity btcutil.Amount) float64 {

	now := m.now()

	m.Lock()
	defer m.Unlock()

	// A priori node probability is assumed to be 1.
	nodeProbability := 1.0

	// Calculate an a priori edge probability based on the amount to send.
	// This assumes the channel balance is at a random point.
	satAmt := amt.ToSatoshis()
	edgeProbability := 1 - float64(satAmt)/float64(capacity)

	// There can be failed nodes and failed edges. The assumption in this
	// probability calculation is that these are independent factors. Both
	// the node and the edge must be successful. Therefore the probabilities
	// are calculated separately and multiplied to get to the total
	// probability of this edge.
	pruneTime, ok := m.failedVertexes[fromNode]
	if ok {
		timeSincePrune := now.Sub(pruneTime)
		switch {

		case timeSincePrune < m.hardPruneDuration:
			return 0

		case timeSincePrune < m.hardPruneDuration+m.vertexDecay:
			nodeProbability = float64(timeSincePrune-
				m.hardPruneDuration) / float64(m.vertexDecay)

		default:
			delete(m.failedVertexes, fromNode)
		}
	}

	pruneTime, ok = m.failedEdges[edge]
	if ok {
		timeSincePrune := now.Sub(pruneTime)
		switch {

		case timeSincePrune < m.hardPruneDuration:
			return 0

		case timeSincePrune < m.hardPruneDuration+m.edgeDecay:
			edgeProbability *= float64(timeSincePrune-
				m.hardPruneDuration) / float64(m.edgeDecay)

		default:
			delete(m.failedEdges, edge)
		}
	}

	return nodeProbability * edgeProbability
}

// reportVertexFailure adds a vertex to the graph prune view after a client
// reports a routing failure localized to the vertex. The time the vertex was
// added is noted, as it'll be pruned from the shared view after a period of
// vertexDecay. However, the vertex will remain pruned for the *local* session.
// This ensures we don't retry this vertex during the payment attempt.
func (m *missionControl) reportVertexFailure(v Vertex) {
	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	// With the vertex added, we'll now report back to the global prune
	// view, with this new piece of information so it can be utilized for
	// new payment sessions.
	m.Lock()
	m.failedVertexes[v] = m.now()
	m.Unlock()
}

// reportChannelFailure adds a channel to the graph prune view. The time the
// channel was added is noted, as it'll be pruned from the global view after a
// period of edgeDecay. However, the edge will remain pruned for the duration
// of the *local* session. This ensures that we don't flap by continually
// retrying an edge after its pruning has expired.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (m *missionControl) reportEdgeFailure(e *EdgeLocator) {
	log.Debugf("Reporting edge %v failure to Mission Control", e)

	// With the edge added, we'll now report back to the global prune view,
	// with this new piece of information so it can be utilized for new
	// payment sessions.
	m.Lock()
	m.failedEdges[*e] = m.now()
	m.Unlock()
}
