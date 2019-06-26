package routing

import (
	"math"
	"sync"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// DefaultPenaltyHalfLife is the default half-life duration. The
	// half-life duration defines after how much time a penalized node or
	// channel is back at 50% probability.
	DefaultPenaltyHalfLife = time.Hour

	// minSecondChanceInterval is the minimum time required between
	// second-chance failures.
	//
	// If nodes return a channel policy related failure, they may get a
	// second chance to forward the payment. It could be that the channel
	// policy that we are aware of is not up to date. This is especially
	// important in case of mobile apps that are mostly offline.
	//
	// However, we don't want to give nodes the option to endlessly return
	// new channel updates so that we are kept busy trying to route through
	// that node until the payment loop times out.
	//
	// Therefore we only grant a second chance to a node if the previous
	// second chance is sufficiently long ago. This is what
	// minSecondChanceInterval defines. If a second policy failure comes in
	// within that interval, we will apply a penalty.
	//
	// Second chances granted are tracked on the level of node pairs. This
	// means that if a node has multiple channels to the same peer, they
	// will only get a single second chance to route to that peer again.
	// Nodes forward non-strict, so it isn't necessary to apply a less
	// restrictive channel level tracking scheme here.
	minSecondChanceInterval = time.Minute

	// DefaultMaxMcHistory is the default maximum history size.
	DefaultMaxMcHistory = 1000
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

	// lastSecondChance tracks the last time a second chance was granted for
	// a directed node pair.
	lastSecondChance map[DirectedNodePair]time.Time

	// now is expected to return the current time. It is supplied as an
	// external function to enable deterministic unit tests.
	now func() time.Time

	cfg *MissionControlConfig

	store *missionControlStore

	sync.Mutex

	// TODO(roasbeef): further counters, if vertex continually unavailable,
	// add to another generation

	// TODO(roasbeef): also add favorable metrics for nodes
}

// MissionControlConfig defines parameters that control mission control
// behaviour.
type MissionControlConfig struct {
	// PenaltyHalfLife defines after how much time a penalized node or
	// channel is back at 50% probability.
	PenaltyHalfLife time.Duration

	// AprioriHopProbability is the assumed success probability of a hop in
	// a route when no other information is available.
	AprioriHopProbability float64

	// MaxMcHistory defines the maximum number of payment results that are
	// held on disk.
	MaxMcHistory int
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

// paymentResult is the information that becomes available when a payment
// attempt completes.
type paymentResult struct {
	id                 uint64
	timeFwd, timeReply time.Time
	route              *route.Route
	success            bool
	failureSourceIdx   *int
	failure            lnwire.FailureMessage
}

// NewMissionControl returns a new instance of missionControl.
func NewMissionControl(db *bbolt.DB, cfg *MissionControlConfig) (
	*MissionControl, error) {

	log.Debugf("Instantiating mission control with config: "+
		"PenaltyHalfLife=%v, AprioriHopProbability=%v",
		cfg.PenaltyHalfLife, cfg.AprioriHopProbability)

	store, err := newMissionControlStore(db, cfg.MaxMcHistory)
	if err != nil {
		return nil, err
	}

	mc := &MissionControl{
		history:          make(map[route.Vertex]*nodeHistory),
		lastSecondChance: make(map[DirectedNodePair]time.Time),
		now:              time.Now,
		cfg:              cfg,
		store:            store,
	}

	if err := mc.init(); err != nil {
		return nil, err
	}

	return mc, nil
}

// init initializes mission control with historical data.
func (m *MissionControl) init() error {
	log.Debugf("Mission control state reconstruction started")

	start := time.Now()

	results, err := m.store.fetchAll()
	if err != nil {
		return err
	}

	for _, result := range results {
		m.applyPaymentResult(result)
	}

	log.Debugf("Mission control state reconstruction finished: "+
		"n=%v, time=%v", len(results), time.Now().Sub(start))

	return nil
}

// ResetHistory resets the history of MissionControl returning it to a state as
// if no payment attempts have been made.
func (m *MissionControl) ResetHistory() error {
	m.Lock()
	defer m.Unlock()

	if err := m.store.clear(); err != nil {
		return err
	}

	m.history = make(map[route.Vertex]*nodeHistory)
	m.lastSecondChance = make(map[DirectedNodePair]time.Time)

	log.Debugf("Mission control history cleared")

	return nil
}

// GetEdgeProbability is expected to return the success probability of a payment
// from fromNode along edge.
func (m *MissionControl) GetEdgeProbability(fromNode route.Vertex,
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

// requestSecondChance checks whether the node fromNode can have a second chance
// at providing a channel update for its channel with toNode.
func (m *MissionControl) requestSecondChance(timestamp time.Time,
	fromNode, toNode route.Vertex) bool {

	// Look up previous second chance time.
	pair := DirectedNodePair{
		From: fromNode,
		To:   toNode,
	}
	lastSecondChance, ok := m.lastSecondChance[pair]

	// If the channel hasn't already be given a second chance or its last
	// second chance was long ago, we give it another chance.
	if !ok || timestamp.Sub(lastSecondChance) > minSecondChanceInterval {
		m.lastSecondChance[pair] = timestamp

		log.Debugf("Second chance granted for %v->%v", fromNode, toNode)

		return true
	}

	// Otherwise penalize the channel, because we don't allow channel
	// updates that are that frequent. This is to prevent nodes from keeping
	// us busy by continuously sending new channel updates.
	log.Debugf("Second chance denied for %v->%v, remaining interval: %v",
		fromNode, toNode, timestamp.Sub(lastSecondChance))

	return false
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
func (m *MissionControl) reportVertexFailure(timestamp time.Time,
	v route.Vertex) {

	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	m.Lock()
	defer m.Unlock()

	history := m.createHistoryIfNotExists(v)
	history.lastFail = &timestamp
}

// reportEdgePolicyFailure reports a policy related failure.
func (m *MissionControl) reportEdgePolicyFailure(timestamp time.Time,
	failedEdge edge) {

	m.Lock()
	defer m.Unlock()

	// We may have an out of date graph. Therefore we don't always penalize
	// immediately. If some time has passed since the last policy failure,
	// we grant the node a second chance at forwarding the payment.
	if m.requestSecondChance(
		timestamp, failedEdge.from, failedEdge.to,
	) {
		return
	}

	history := m.createHistoryIfNotExists(failedEdge.from)
	history.lastFail = &timestamp
}

// reportEdgeFailure reports a channel level failure.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (m *MissionControl) reportEdgeFailure(timestamp time.Time, failedEdge edge,
	minPenalizeAmt lnwire.MilliSatoshi) {

	log.Debugf("Reporting channel %v failure to Mission Control",
		failedEdge.channel)

	m.Lock()
	defer m.Unlock()

	history := m.createHistoryIfNotExists(failedEdge.from)
	history.channelLastFail[failedEdge.channel] = &channelHistory{
		lastFail:       timestamp,
		minPenalizeAmt: minPenalizeAmt,
	}
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

// ReportPaymentFail reports a failed payment to mission control as input for
// future probability estimates. The failureSourceIdx argument indicates the
// failure source. If it is nil, the failure source is unknown. This function
// returns a bool indicating whether this error is a final error. If it is
// final, a failure reason is returned and no further payment attempts need to
// be made.
func (m *MissionControl) ReportPaymentFail(paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (bool,
	channeldb.FailureReason, error) {

	timestamp := m.now()

	// TODO(joostjager): Use actual payment initiation time for timeFwd.
	result := &paymentResult{
		success:          false,
		timeFwd:          timestamp,
		timeReply:        timestamp,
		id:               paymentID,
		failureSourceIdx: failureSourceIdx,
		failure:          failure,
		route:            rt,
	}

	// Store complete result in database.
	if err := m.store.AddResult(result); err != nil {
		return false, 0, err
	}

	// Apply result to update mission control state.
	final, reason := m.applyPaymentResult(result)

	return final, reason, nil
}

// applyPaymentResult applies a payment result as input for future probability
// estimates. It returns a bool indicating whether this error is a final error
// and no further payment attempts need to be made.
func (m *MissionControl) applyPaymentResult(result *paymentResult) (
	bool, channeldb.FailureReason) {

	var (
		failureSourceIdxInt int
		failure             lnwire.FailureMessage
	)

	if result.failureSourceIdx == nil {
		// If the failure message could not be decrypted, attribute the
		// failure to our own outgoing channel.
		//
		// TODO(joostager): Penalize all channels in the route.
		failureSourceIdxInt = 0
		failure = lnwire.NewTemporaryChannelFailure(nil)
	} else {
		failureSourceIdxInt = *result.failureSourceIdx
		failure = result.failure
	}

	var failureVertex route.Vertex

	if failureSourceIdxInt > 0 {
		failureVertex = result.route.Hops[failureSourceIdxInt-1].PubKeyBytes
	} else {
		failureVertex = result.route.SourcePubKey
	}
	log.Tracef("Node %x (index %v) reported failure when sending htlc",
		failureVertex, result.failureSourceIdx)

	// Always determine chan id ourselves, because a channel update with id
	// may not be available.
	failedEdge, failedAmt := getFailedEdge(
		result.route, failureSourceIdxInt,
	)

	switch failure.(type) {

	// If the end destination didn't know the payment
	// hash or we sent the wrong payment amount to the
	// destination, then we'll terminate immediately.
	case *lnwire.FailUnknownPaymentHash:
		// TODO(joostjager): Check onionErr.Amount() whether it matches
		// what we expect. (Will it ever not match, because if not
		// final_incorrect_htlc_amount would be returned?)

		return true, channeldb.FailureReasonIncorrectPaymentDetails

	// If we sent the wrong amount to the destination, then
	// we'll exit early.
	case *lnwire.FailIncorrectPaymentAmount:
		return true, channeldb.FailureReasonIncorrectPaymentDetails

	// If the time-lock that was extended to the final node
	// was incorrect, then we can't proceed.
	case *lnwire.FailFinalIncorrectCltvExpiry:
		// TODO(joostjager): Take into account that second last hop may
		// have deliberately handed out an htlc that expires too soon.
		// In that case we should continue routing.
		return true, channeldb.FailureReasonError

	// If we crafted an invalid onion payload for the final
	// node, then we'll exit early.
	case *lnwire.FailFinalIncorrectHtlcAmount:
		// TODO(joostjager): Take into account that second last hop may
		// have deliberately handed out an htlc with a too low value. In
		// that case we should continue routing.

		return true, channeldb.FailureReasonError

	// Similarly, if the HTLC expiry that we extended to
	// the final hop expires too soon, then will fail the
	// payment.
	//
	// TODO(roasbeef): can happen to to race condition, try
	// again with recent block height
	case *lnwire.FailFinalExpiryTooSoon:
		// TODO(joostjager): Take into account that any hop may have
		// delayed. Ideally we should continue routing. Knowing the
		// delaying node at this point would help.
		return true, channeldb.FailureReasonIncorrectPaymentDetails

	// If we erroneously attempted to cross a chain border,
	// then we'll cancel the payment.
	case *lnwire.FailInvalidRealm:
		return true, channeldb.FailureReasonError

	// If we get a notice that the expiry was too soon for
	// an intermediate node, then we'll prune out the node
	// that sent us this error, as it doesn't now what the
	// correct block height is.
	case *lnwire.FailExpiryTooSoon:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0

	// If we hit an instance of onion payload corruption or an invalid
	// version, then we'll exit early as this shouldn't happen in the
	// typical case.
	//
	// TODO(joostjager): Take into account that the previous hop may have
	// tampered with the onion. Routing should continue using other paths.
	case *lnwire.FailInvalidOnionVersion:
		return true, channeldb.FailureReasonError
	case *lnwire.FailInvalidOnionHmac:
		return true, channeldb.FailureReasonError
	case *lnwire.FailInvalidOnionKey:
		return true, channeldb.FailureReasonError

	// If we get a failure due to violating the minimum
	// amount, we'll apply the new minimum amount and retry
	// routing.
	case *lnwire.FailAmountBelowMinimum:
		m.reportEdgePolicyFailure(result.timeReply, failedEdge)
		return false, 0

	// If we get a failure due to a fee, we'll apply the
	// new fee update, and retry our attempt using the
	// newly updated fees.
	case *lnwire.FailFeeInsufficient:
		m.reportEdgePolicyFailure(result.timeReply, failedEdge)
		return false, 0

	// If we get the failure for an intermediate node that
	// disagrees with our time lock values, then we'll
	// apply the new delta value and try it once more.
	case *lnwire.FailIncorrectCltvExpiry:
		m.reportEdgePolicyFailure(result.timeReply, failedEdge)
		return false, 0

	// The outgoing channel that this node was meant to
	// forward one is currently disabled, so we'll apply
	// the update and continue.
	case *lnwire.FailChannelDisabled:
		m.reportEdgeFailure(result.timeReply, failedEdge, 0)
		return false, 0

	// It's likely that the outgoing channel didn't have
	// sufficient capacity, so we'll prune this edge for
	// now, and continue onwards with our path finding.
	case *lnwire.FailTemporaryChannelFailure:
		m.reportEdgeFailure(result.timeReply, failedEdge, failedAmt)
		return false, 0

	// If the send fail due to a node not having the
	// required features, then we'll note this error and
	// continue.
	case *lnwire.FailRequiredNodeFeatureMissing:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0

	// If the send fail due to a node not having the
	// required features, then we'll note this error and
	// continue.
	case *lnwire.FailRequiredChannelFeatureMissing:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0

	// If the next hop in the route wasn't known or
	// offline, we'll only the channel which we attempted
	// to route over. This is conservative, and it can
	// handle faulty channels between nodes properly.
	// Additionally, this guards against routing nodes
	// returning errors in order to attempt to black list
	// another node.
	case *lnwire.FailUnknownNextPeer:
		m.reportEdgeFailure(result.timeReply, failedEdge, 0)
		return false, 0

	// If the node wasn't able to forward for which ever
	// reason, then we'll note this and continue with the
	// routes.
	case *lnwire.FailTemporaryNodeFailure:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0

	case *lnwire.FailPermanentNodeFailure:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0

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
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0

	// If we get a permanent channel or node failure, then
	// we'll prune the channel in both directions and
	// continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		m.reportEdgeFailure(result.timeReply, failedEdge, 0)
		m.reportEdgeFailure(result.timeReply, edge{
			from:    failedEdge.to,
			to:      failedEdge.from,
			channel: failedEdge.channel,
		}, 0)
		return false, 0

	// Any other failure or an empty failure will get the node pruned.
	default:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0
	}
}

// getFailedEdge tries to locate the failing channel given a route and the
// pubkey of the node that sent the failure. It will assume that the failure is
// associated with the outgoing channel of the failing node. As a second result,
// it returns the amount sent over the edge.
func getFailedEdge(route *route.Route, failureSource int) (edge,
	lnwire.MilliSatoshi) {

	// Determine if we have a failure from the final hop. If it is, we
	// assume that the failing channel is the incoming channel.
	//
	// TODO(joostjager): In this case, certain types of failures are not
	// expected. For example FailUnknownNextPeer. This could be a reason to
	// prune the node?
	if failureSource == len(route.Hops) {
		failureSource--
	}

	// As this failure indicates that the target channel was unable to carry
	// this HTLC (for w/e reason), we'll return the _outgoing_ channel that
	// the source of the failure was meant to pass the HTLC along to.
	if failureSource == 0 {
		return edge{
			from:    route.SourcePubKey,
			to:      route.Hops[0].PubKeyBytes,
			channel: route.Hops[0].ChannelID,
		}, route.TotalAmount
	}

	return edge{
		from:    route.Hops[failureSource-1].PubKeyBytes,
		to:      route.Hops[failureSource].PubKeyBytes,
		channel: route.Hops[failureSource].ChannelID,
	}, route.Hops[failureSource-1].AmtToForward
}
