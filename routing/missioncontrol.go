package routing

import (
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// DefaultPenaltyHalfLife is the default half-life duration. The
	// half-life duration defines after how much time a penalized node or
	// channel is back at 50% probability.
	DefaultPenaltyHalfLife = time.Hour

	// minSecondChanceInterval is the minimum time required between
	// second-chance failures.
	minSecondChanceInterval = time.Minute
)

// MissionControl contains state which summarizes the past attempts of HTLC
// routing by external callers when sending payments throughout the network. It
// acts as a shared memory during routing attempts with the goal to optimize the
// payment attempt success rate.
//
// Failed payment attempts are reported to mission control. These reports are
// used to track the time of the last node or channel level failure. The time
// since the last failure is used to estimate a success probability that is fed
// into the path finding process for subsequent payment attempts.
type MissionControl struct {
	history map[route.Vertex]*nodeHistory

	graph *channeldb.ChannelGraph

	secondChanceChannels map[uint64]struct{}

	selfNode *channeldb.LightningNode

	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi

	// now is expected to return the current time. It is supplied as an
	// external function to enable deterministic unit tests.
	now func() time.Time

	cfg *MissionControlConfig

	sync.Mutex

	// TODO(roasbeef): further counters, if vertex continually unavailable,
	// add to another generation

	// TODO(roasbeef): also add favorable metrics for nodes
}

// A compile time assertion to ensure MissionControl meets the
// PaymentSessionSource interface.
var _ PaymentSessionSource = (*MissionControl)(nil)

// MissionControlConfig defines parameters that control mission control
// behaviour.
type MissionControlConfig struct {
	// PenaltyHalfLife defines after how much time a penalized node or
	// channel is back at 50% probability.
	PenaltyHalfLife time.Duration

	// PaymentAttemptPenalty is the virtual cost in path finding weight
	// units of executing a payment attempt that fails. It is used to trade
	// off potentially better routes against their probability of
	// succeeding.
	PaymentAttemptPenalty lnwire.MilliSatoshi

	// MinProbability defines the minimum success probability of the
	// returned route.
	MinRouteProbability float64

	// AprioriHopProbability is the assumed success probability of a hop in
	// a route when no other information is available.
	AprioriHopProbability float64
}

// nodeHistory contains a summary of payment attempt outcomes involving a
// particular node.
type nodeHistory struct {
	// lastFail is the last time a node level failure occurred, if any.
	lastFail *time.Time

	// channelLastFail tracks history per channel, if available for that
	// channel.
	channelLastFail map[uint64]*channelHistory
}

// channelHistory contains a summary of payment attempt outcomes involving a
// particular channel.
type channelHistory struct {
	// lastFail is the last time a channel level failure occurred.
	lastFail time.Time

	// minPenalizeAmt is the minimum amount for which to take this failure
	// into account.
	minPenalizeAmt lnwire.MilliSatoshi
}

// MissionControlSnapshot contains a snapshot of the current state of mission
// control.
type MissionControlSnapshot struct {
	// Nodes contains the per node information of this snapshot.
	Nodes []MissionControlNodeSnapshot
}

// MissionControlNodeSnapshot contains a snapshot of the current node state in
// mission control.
type MissionControlNodeSnapshot struct {
	// Node pubkey.
	Node route.Vertex

	// Lastfail is the time of last failure, if any.
	LastFail *time.Time

	// Channels is a list of channels for which specific information is
	// logged.
	Channels []MissionControlChannelSnapshot

	// OtherChanSuccessProb is the success probability for channels not in
	// the Channels slice.
	OtherChanSuccessProb float64
}

// MissionControlChannelSnapshot contains a snapshot of the current channel
// state in mission control.
type MissionControlChannelSnapshot struct {
	// ChannelID is the short channel id of the snapshot.
	ChannelID uint64

	// LastFail is the time of last failure.
	LastFail time.Time

	// MinPenalizeAmt is the minimum amount for which the channel will be
	// penalized.
	MinPenalizeAmt lnwire.MilliSatoshi

	// SuccessProb is the success probability estimation for this channel.
	SuccessProb float64
}

// paymentReport consists of all relevant data of a payment to feed into mission
// control.
type paymentReport struct {
	timestamp        time.Time
	route            *route.Route
	errorSourceIndex int
	failure          lnwire.FailureMessage
}

// NewMissionControl returns a new instance of missionControl.
//
// TODO(roasbeef): persist memory
func NewMissionControl(g *channeldb.ChannelGraph,
	selfNode *channeldb.LightningNode,
	qb func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi,
	cfg *MissionControlConfig) *MissionControl {

	log.Debugf("Instantiating mission control with config: "+
		"PenaltyHalfLife=%v, PaymentAttemptPenalty=%v, "+
		"MinRouteProbability=%v, AprioriHopProbability=%v",
		cfg.PenaltyHalfLife,
		int64(cfg.PaymentAttemptPenalty.ToSatoshis()),
		cfg.MinRouteProbability, cfg.AprioriHopProbability)

	return &MissionControl{
		history:              make(map[route.Vertex]*nodeHistory),
		secondChanceChannels: make(map[uint64]struct{}),
		selfNode:             selfNode,
		queryBandwidth:       qb,
		graph:                g,
		now:                  time.Now,
		cfg:                  cfg,
	}
}

// NewPaymentSession creates a new payment session backed by the latest prune
// view from Mission Control. An optional set of routing hints can be provided
// in order to populate additional edges to explore when finding a path to the
// payment's destination.
func (m *MissionControl) NewPaymentSession(routeHints [][]zpay32.HopHint,
	target route.Vertex) (PaymentSession, error) {

	edges := make(map[route.Vertex][]*channeldb.ChannelEdgePolicy)

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

			v := route.NewVertex(hopHint.NodeID)
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
		additionalEdges: edges,
		bandwidthHints:  bandwidthHints,
		mc:              m,
		pathFinder:      findPath,
	}, nil
}

// NewPaymentSessionForRoute creates a new paymentSession instance that is just
// used for failure reporting to missioncontrol.
func (m *MissionControl) NewPaymentSessionForRoute(preBuiltRoute *route.Route) PaymentSession {
	return &paymentSession{
		mc:            m,
		preBuiltRoute: preBuiltRoute,
	}
}

// NewPaymentSessionEmpty creates a new paymentSession instance that is empty,
// and will be exhausted immediately. Used for failure reporting to
// missioncontrol for resumed payment we don't want to make more attempts for.
func (m *MissionControl) NewPaymentSessionEmpty() PaymentSession {
	return &paymentSession{
		mc:                 m,
		preBuiltRoute:      &route.Route{},
		preBuiltRouteTried: true,
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

// ResetHistory resets the history of MissionControl returning it to a state as
// if no payment attempts have been made.
func (m *MissionControl) ResetHistory() {
	m.Lock()
	defer m.Unlock()

	m.history = make(map[route.Vertex]*nodeHistory)

	log.Debugf("Mission control history cleared")
}

// getEdgeProbability is expected to return the success probability of a payment
// from fromNode along edge.
func (m *MissionControl) getEdgeProbability(fromNode route.Vertex,
	edge EdgeLocator, amt lnwire.MilliSatoshi) float64 {

	m.Lock()
	defer m.Unlock()

	// Get the history for this node. If there is no history available,
	// assume that it's success probability is a constant a priori
	// probability. After the attempt new information becomes available to
	// adjust this probability.
	nodeHistory, ok := m.history[fromNode]
	if !ok {
		return m.cfg.AprioriHopProbability
	}

	return m.getEdgeProbabilityForNode(nodeHistory, edge.ChannelID, amt)
}

// getEdgeProbabilityForNode estimates the probability of successfully
// traversing a channel based on the node history.
func (m *MissionControl) getEdgeProbabilityForNode(nodeHistory *nodeHistory,
	channelID uint64, amt lnwire.MilliSatoshi) float64 {

	// Calculate the last failure of the given edge. A node failure is
	// considered a failure that would have affected every edge. Therefore
	// we insert a node level failure into the history of every channel.
	lastFailure := nodeHistory.lastFail

	// Take into account a minimum penalize amount. For balance errors, a
	// failure may be reported with such a minimum to prevent too aggresive
	// penalization. We only take into account a previous failure if the
	// amount that we currently get the probability for is greater or equal
	// than the minPenalizeAmt of the previous failure.
	channelHistory, ok := nodeHistory.channelLastFail[channelID]
	if ok && channelHistory.minPenalizeAmt <= amt {

		// If there is both a node level failure recorded and a channel
		// level failure is applicable too, we take the most recent of
		// the two.
		if lastFailure == nil ||
			channelHistory.lastFail.After(*lastFailure) {

			lastFailure = &channelHistory.lastFail
		}
	}

	if lastFailure == nil {
		return m.cfg.AprioriHopProbability
	}

	timeSinceLastFailure := m.now().Sub(*lastFailure)

	// Calculate success probability. It is an exponential curve that brings
	// the probability down to zero when a failure occurs. From there it
	// recovers asymptotically back to the a priori probability. The rate at
	// which this happens is controlled by the penaltyHalfLife parameter.
	exp := -timeSinceLastFailure.Hours() / m.cfg.PenaltyHalfLife.Hours()
	probability := m.cfg.AprioriHopProbability * (1 - math.Pow(2, exp))

	return probability
}

// createHistoryIfNotExists returns the history for the given node. If the node
// is yet unknown, it will create an empty history structure.
func (m *MissionControl) createHistoryIfNotExists(vertex route.Vertex) *nodeHistory {
	if node, ok := m.history[vertex]; ok {
		return node
	}

	node := &nodeHistory{
		channelLastFail: make(map[uint64]*channelHistory),
	}
	m.history[vertex] = node

	return node
}

// reportVertexFailure reports a node level failure.
func (m *MissionControl) reportVertexFailure(timestamp time.Time, v route.Vertex) {
	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	m.Lock()
	defer m.Unlock()

	history := m.createHistoryIfNotExists(v)
	history.lastFail = &timestamp
}

// reportEdgeFailure reports a channel level failure. The validUpdateAttached
// parameter should indicate whether the attached update - if any - was valid.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (m *MissionControl) reportEdgeFailure(timestamp time.Time, failedEdge edge,
	minPenalizeAmt lnwire.MilliSatoshi, allowSecondChance bool) {

	channelID := failedEdge.channel

	log.Debugf("Reporting failure to Mission Control: "+
		"chan=%v, allowSecondChance=%v", channelID,
		allowSecondChance)

	m.Lock()
	defer m.Unlock()

	history := m.createHistoryIfNotExists(failedEdge.from)

	_, secondChanceGiven := m.secondChanceChannels[channelID]

	// If the channel hasn't already be given a second chance and we allow a
	// second chance, it may get one.
	if allowSecondChance && !secondChanceGiven {
		// If channel has no history yet or the previous failure is long
		// enough ago, allow a second chance.
		channelHistory, ok := history.channelLastFail[channelID]
		if !ok || timestamp.Sub(channelHistory.lastFail) >
			minSecondChanceInterval {

			m.secondChanceChannels[channelID] = struct{}{}

			log.Debugf("Second chance granted for chan=%v",
				channelID)

			return
		}

		// Otherwise penalize the channel, because we don't allow
		// channel updates that are that frequent. This is to prevent
		// nodes from keeping us busy by continuously sending new
		// channel updates.
	}

	history.channelLastFail[channelID] = &channelHistory{
		lastFail:       timestamp,
		minPenalizeAmt: minPenalizeAmt,
	}

	delete(m.secondChanceChannels, channelID)
}

// GetHistorySnapshot takes a snapshot from the current mission control state
// and actual probability estimates.
func (m *MissionControl) GetHistorySnapshot() *MissionControlSnapshot {
	m.Lock()
	defer m.Unlock()

	log.Debugf("Requesting history snapshot from mission control: "+
		"node_count=%v", len(m.history))

	nodes := make([]MissionControlNodeSnapshot, 0, len(m.history))

	for v, h := range m.history {
		channelSnapshot := make([]MissionControlChannelSnapshot, 0,
			len(h.channelLastFail),
		)

		for id, lastFail := range h.channelLastFail {
			// Show probability assuming amount meets min
			// penalization amount.
			prob := m.getEdgeProbabilityForNode(
				h, id, lastFail.minPenalizeAmt,
			)

			channelSnapshot = append(channelSnapshot,
				MissionControlChannelSnapshot{
					ChannelID:      id,
					LastFail:       lastFail.lastFail,
					MinPenalizeAmt: lastFail.minPenalizeAmt,
					SuccessProb:    prob,
				},
			)
		}

		otherProb := m.getEdgeProbabilityForNode(h, 0, 0)

		nodes = append(nodes,
			MissionControlNodeSnapshot{
				Node:                 v,
				LastFail:             h.lastFail,
				OtherChanSuccessProb: otherProb,
				Channels:             channelSnapshot,
			},
		)
	}

	snapshot := MissionControlSnapshot{
		Nodes: nodes,
	}

	return &snapshot
}

// reportPaymentOutcome reports a failed payment to mission control as input for
// future probability estimates. It returns a bool indicating whether this error
// is a final error and no further payment attempts need to be made.
func (m *MissionControl) reportPaymentOutcome(rt *route.Route,
	errorSourceIndex int, failure lnwire.FailureMessage) (bool, error) {

	timestamp := m.now()

	report := &paymentReport{
		timestamp:        timestamp,
		route:            rt,
		errorSourceIndex: errorSourceIndex,
		failure:          failure,
	}

	return m.processPaymentOutcome(report), nil
}

// processPaymentOutcome processes a failed payment as input for future
// probability estimates. It returns a bool indicating whether this error is a
// final error and no further payment attempts need to be made.
func (m *MissionControl) processPaymentOutcome(rp *paymentReport) bool {

	// TODO: Improve error attribution. Depending on the failure type,
	// either the incoming or the outgoing channel should be penalized. Also
	// the position of the node in the route is relevant (first after
	// source, last before destination).
	errorSource := rp.route.GetNodePubkeyByIndex(rp.errorSourceIndex)
	failedEdge, failedAmt := getFailedEdge(rp.route, rp.errorSourceIndex)

	switch rp.failure.(type) {

	// If the end destination didn't know the payment
	// hash or we sent the wrong payment amount to the
	// destination, then we'll terminate immediately.
	case *lnwire.FailUnknownPaymentHash:
		return true

	// If we sent the wrong amount to the destination, then
	// we'll exit early.
	case *lnwire.FailIncorrectPaymentAmount:
		return true

	// If the time-lock that was extended to the final node
	// was incorrect, then we can't proceed.
	case *lnwire.FailFinalIncorrectCltvExpiry:
		return true

	// If we crafted an invalid onion payload for the final
	// node, then we'll exit early.
	case *lnwire.FailFinalIncorrectHtlcAmount:
		return true

	// Similarly, if the HTLC expiry that we extended to
	// the final hop expires too soon, then will fail the
	// payment.
	//
	// TODO(roasbeef): can happen to to race condition, try
	// again with recent block height
	case *lnwire.FailFinalExpiryTooSoon:
		return true

	// If we erroneously attempted to cross a chain border,
	// then we'll cancel the payment.
	case *lnwire.FailInvalidRealm:
		return true

	// If we get a notice that the expiry was too soon for
	// an intermediate node, then we'll prune out the node
	// that sent us this error, as it doesn't now what the
	// correct block height is.
	case *lnwire.FailExpiryTooSoon:
		m.reportVertexFailure(rp.timestamp, errorSource)
		return false

	// If we hit an instance of onion payload corruption or
	// an invalid version, then we'll exit early as this
	// shouldn't happen in the typical case.
	case *lnwire.FailInvalidOnionVersion:
		return true
	case *lnwire.FailInvalidOnionHmac:
		return true
	case *lnwire.FailInvalidOnionKey:
		return true

	// If we get a failure due to violating the minimum amount, we'll apply
	// the new minimum amount and retry routing.
	//
	// A node sending a policy failures with an embedded channel update may
	// get a second chance.
	//
	// TODO: Check update for nilness.
	case *lnwire.FailAmountBelowMinimum:
		m.reportEdgeFailure(rp.timestamp, failedEdge, 0, true)
		return false

	// If we get a failure due to a fee, we'll apply the
	// new fee update, and retry our attempt using the
	// newly updated fees.
	case *lnwire.FailFeeInsufficient:
		m.reportEdgeFailure(rp.timestamp, failedEdge, 0, true)
		return false

	// If we get the failure for an intermediate node that
	// disagrees with our time lock values, then we'll
	// apply the new delta value and try it once more.
	case *lnwire.FailIncorrectCltvExpiry:
		m.reportEdgeFailure(rp.timestamp, failedEdge, 0, true)
		return false

	// The outgoing channel that this node was meant to
	// forward one is currently disabled, so we'll apply
	// the update and continue.
	case *lnwire.FailChannelDisabled:
		m.reportEdgeFailure(rp.timestamp, failedEdge, 0, false)
		return false

	// It's likely that the outgoing channel didn't have
	// sufficient capacity, so we'll prune this edge for
	// now, and continue onwards with our path finding.
	case *lnwire.FailTemporaryChannelFailure:
		m.reportEdgeFailure(rp.timestamp, failedEdge, failedAmt, false)
		return false

	// If the send fail due to a node not having the
	// required features, then we'll note this error and
	// continue.
	case *lnwire.FailRequiredNodeFeatureMissing:
		m.reportVertexFailure(rp.timestamp, errorSource)
		return false

	// If the send fail due to a node not having the
	// required features, then we'll note this error and
	// continue.
	case *lnwire.FailRequiredChannelFeatureMissing:
		m.reportVertexFailure(rp.timestamp, errorSource)
		return false

	// If the next hop in the route wasn't known or
	// offline, we'll only the channel which we attempted
	// to route over. This is conservative, and it can
	// handle faulty channels between nodes properly.
	// Additionally, this guards against routing nodes
	// returning errors in order to attempt to black list
	// another node.
	case *lnwire.FailUnknownNextPeer:
		m.reportEdgeFailure(rp.timestamp, failedEdge, 0, false)
		return false

	// If the node wasn't able to forward for which ever
	// reason, then we'll note this and continue with the
	// routes.
	case *lnwire.FailTemporaryNodeFailure:
		m.reportVertexFailure(rp.timestamp, errorSource)
		return false

	case *lnwire.FailPermanentNodeFailure:
		m.reportVertexFailure(rp.timestamp, errorSource)
		return false

	// If we crafted a route that contains a too long time
	// lock for an intermediate node, we'll prune the node.
	// As there currently is no way of knowing that node's
	// maximum acceptable cltv, we cannot take this
	// constraint into account during routing.
	//
	// TODO(joostjager): Record the rejected cltv and use
	// that as a hint during future path finding through
	// that node.
	case *lnwire.FailExpiryTooFar:
		m.reportVertexFailure(rp.timestamp, errorSource)
		return false

	// If we get a permanent channel or node failure, then
	// we'll prune the channel in both directions and
	// continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		m.reportEdgeFailure(rp.timestamp, failedEdge, 0, false)
		m.reportEdgeFailure(
			rp.timestamp,
			edge{
				from:    failedEdge.to,
				to:      failedEdge.from,
				channel: failedEdge.channel,
			}, 0, false,
		)
		return false

	default:
		return true
	}
}

// getEdgeByIndex returns a structure describing the edge specified by index.
// Index zero is the first edge of the route.
func getEdgeByIndex(rt *route.Route, index int) (edge, lnwire.MilliSatoshi) {
	var (
		fromNode route.Vertex
		amt      lnwire.MilliSatoshi
	)

	if index == 0 {
		fromNode = rt.SourcePubKey
		amt = rt.TotalAmount
	} else {
		fromNode = rt.Hops[index-1].PubKeyBytes
		amt = rt.Hops[index-1].AmtToForward
	}

	return edge{
		from:    fromNode,
		to:      rt.Hops[index].PubKeyBytes,
		channel: rt.Hops[index].ChannelID,
	}, amt
}

// getFailedEdge tries to locate the failing channel given a route and the index
// of the node that sent the error.
func getFailedEdge(route *route.Route, errIdx int) (edge, lnwire.MilliSatoshi) {
	// Determine if we have a failure from the final hop. In that case we
	// return the incoming channel.
	//
	// TODO(joostjager): In this case, certain types of errors are not
	// expected. For example FailUnknownNextPeer. This could be a reason to
	// prune the node?
	if errIdx == len(route.Hops) {
		return getEdgeByIndex(route, errIdx-1)
	}

	// As this error indicates that the target channel was unable to
	// carry this HTLC (for w/e reason), we'll return the _outgoing_
	// channel that the source of the error was meant to pass the
	// HTLC along to.
	return getEdgeByIndex(route, errIdx)
}
