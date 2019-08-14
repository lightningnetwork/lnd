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
	// lastPairFailure tracks the last payment failure per node pair.
	lastPairFailure map[DirectedNodePair]pairFailure

	// lastNodeFailure tracks the last node level failure per node.
	lastNodeFailure map[route.Vertex]time.Time

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

// pairFailure describes a payment failure for a node pair.
type pairFailure struct {
	// timestamp is the time when this failure result was obtained.
	timestamp time.Time

	// minPenalizeAmt is the minimum amount for which to take this failure
	// into account.
	minPenalizeAmt lnwire.MilliSatoshi
}

// MissionControlSnapshot contains a snapshot of the current state of mission
// control.
type MissionControlSnapshot struct {
	// Nodes contains the per node information of this snapshot.
	Nodes []MissionControlNodeSnapshot

	// Pairs is a list of channels for which specific information is
	// logged.
	Pairs []MissionControlPairSnapshot
}

// MissionControlNodeSnapshot contains a snapshot of the current node state in
// mission control.
type MissionControlNodeSnapshot struct {
	// Node pubkey.
	Node route.Vertex

	// LastFail is the time of last failure.
	LastFail time.Time

	// OtherSuccessProb is the success probability for pairs not in
	// the Pairs slice.
	OtherSuccessProb float64
}

// MissionControlPairSnapshot contains a snapshot of the current node pair
// state in mission control.
type MissionControlPairSnapshot struct {
	// Pair is the node pair of which the state is described.
	Pair DirectedNodePair

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
		lastPairFailure:  make(map[DirectedNodePair]pairFailure),
		lastNodeFailure:  make(map[route.Vertex]time.Time),
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

	m.lastPairFailure = make(map[DirectedNodePair]pairFailure)
	m.lastNodeFailure = make(map[route.Vertex]time.Time)
	m.lastSecondChance = make(map[DirectedNodePair]time.Time)

	log.Debugf("Mission control history cleared")

	return nil
}

// GetProbability is expected to return the success probability of a payment
// from fromNode along edge.
func (m *MissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	m.Lock()
	defer m.Unlock()

	return m.getPairProbability(fromNode, toNode, amt)
}

// getProbAfterFail returns a probability estimate based on a last failure time.
func (m *MissionControl) getProbAfterFail(lastFailure time.Time) float64 {
	if lastFailure.IsZero() {
		return m.cfg.AprioriHopProbability
	}

	timeSinceLastFailure := m.now().Sub(lastFailure)

	// Calculate success probability. It is an exponential curve that brings
	// the probability down to zero when a failure occurs. From there it
	// recovers asymptotically back to the a priori probability. The rate at
	// which this happens is controlled by the penaltyHalfLife parameter.
	exp := -timeSinceLastFailure.Hours() / m.cfg.PenaltyHalfLife.Hours()
	probability := m.cfg.AprioriHopProbability * (1 - math.Pow(2, exp))

	return probability
}

// getPairProbability estimates the probability of successfully
// traversing from fromNode to toNode based on historical payment outcomes.
func (m *MissionControl) getPairProbability(fromNode,
	toNode route.Vertex, amt lnwire.MilliSatoshi) float64 {

	// Start by getting the last node level failure. A node failure is
	// considered a failure that would have affected every edge. Therefore
	// we insert a node level failure into the history of every channel. If
	// there is none, lastFail will be zero.
	lastFail := m.lastNodeFailure[fromNode]

	// Retrieve the last pair outcome.
	pair := NewDirectedNodePair(fromNode, toNode)
	lastPairResult, ok := m.lastPairFailure[pair]

	// Only look at the last pair outcome if it happened after the last node
	// level failure. Otherwise the node level failure is the most recent
	// and used as the basis for calculation of the probability.
	if ok && lastPairResult.timestamp.After(lastFail) {
		// Take into account a minimum penalize amount. For balance
		// errors, a failure may be reported with such a minimum to
		// prevent too aggresive penalization. We only take into account
		// a previous failure if the amount that we currently get the
		// probability for is greater or equal than the minPenalizeAmt
		// of the previous failure.
		if amt >= lastPairResult.minPenalizeAmt {
			lastFail = lastPairResult.timestamp
		}
	}

	return m.getProbAfterFail(lastFail)
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

// reportVertexFailure reports a node level failure.
func (m *MissionControl) reportVertexFailure(timestamp time.Time,
	v route.Vertex) {

	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	m.Lock()
	defer m.Unlock()

	m.lastNodeFailure[v] = timestamp
}

// reportPairPolicyFailure reports a policy related failure.
func (m *MissionControl) reportPairPolicyFailure(timestamp time.Time,
	failedPair DirectedNodePair) {

	m.Lock()
	defer m.Unlock()

	// We may have an out of date graph. Therefore we don't always penalize
	// immediately. If some time has passed since the last policy failure,
	// we grant the node a second chance at forwarding the payment.
	if m.requestSecondChance(
		timestamp, failedPair.From, failedPair.To,
	) {
		return
	}

	m.lastNodeFailure[failedPair.From] = timestamp
}

// reportPairFailure reports a pair level failure.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (m *MissionControl) reportPairFailure(timestamp time.Time,
	failedPair DirectedNodePair, minPenalizeAmt lnwire.MilliSatoshi) {

	log.Debugf("Reporting pair %v failure to Mission Control", failedPair)

	m.Lock()
	defer m.Unlock()

	pair := NewDirectedNodePair(failedPair.From, failedPair.To)
	m.lastPairFailure[pair] = pairFailure{
		minPenalizeAmt: minPenalizeAmt,
		timestamp:      timestamp,
	}
}

// GetHistorySnapshot takes a snapshot from the current mission control state
// and actual probability estimates.
func (m *MissionControl) GetHistorySnapshot() *MissionControlSnapshot {
	m.Lock()
	defer m.Unlock()

	log.Debugf("Requesting history snapshot from mission control: "+
		"node_failure_count=%v, pair_result_count=%v",
		len(m.lastNodeFailure), len(m.lastPairFailure))

	nodes := make([]MissionControlNodeSnapshot, 0, len(m.lastNodeFailure))
	for v, h := range m.lastNodeFailure {
		otherProb := m.getPairProbability(v, route.Vertex{}, 0)

		nodes = append(nodes, MissionControlNodeSnapshot{
			Node:             v,
			LastFail:         h,
			OtherSuccessProb: otherProb,
		})
	}

	pairs := make([]MissionControlPairSnapshot, 0, len(m.lastPairFailure))

	for v, h := range m.lastPairFailure {
		// Show probability assuming amount meets min
		// penalization amount.
		prob := m.getPairProbability(v.From, v.To, h.minPenalizeAmt)

		pair := MissionControlPairSnapshot{
			Pair:           v,
			MinPenalizeAmt: h.minPenalizeAmt,
			LastFail:       h.timestamp,
			SuccessProb:    prob,
		}

		pairs = append(pairs, pair)
	}

	snapshot := MissionControlSnapshot{
		Nodes: nodes,
		Pairs: pairs,
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
	failedPair, failedAmt := getFailedPair(
		result.route, failureSourceIdxInt,
	)

	switch failure.(type) {

	// If the end destination didn't know the payment
	// hash or we sent the wrong payment amount to the
	// destination, then we'll terminate immediately.
	case *lnwire.FailIncorrectDetails:
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
		m.reportPairPolicyFailure(result.timeReply, failedPair)
		return false, 0

	// If we get a failure due to a fee, we'll apply the
	// new fee update, and retry our attempt using the
	// newly updated fees.
	case *lnwire.FailFeeInsufficient:
		m.reportPairPolicyFailure(result.timeReply, failedPair)
		return false, 0

	// If we get the failure for an intermediate node that
	// disagrees with our time lock values, then we'll
	// apply the new delta value and try it once more.
	case *lnwire.FailIncorrectCltvExpiry:
		m.reportPairPolicyFailure(result.timeReply, failedPair)
		return false, 0

	// The outgoing channel that this node was meant to
	// forward one is currently disabled, so we'll apply
	// the update and continue.
	case *lnwire.FailChannelDisabled:
		m.reportPairFailure(result.timeReply, failedPair, 0)
		return false, 0

	// It's likely that the outgoing channel didn't have
	// sufficient capacity, so we'll prune this pair for
	// now, and continue onwards with our path finding.
	case *lnwire.FailTemporaryChannelFailure:
		m.reportPairFailure(result.timeReply, failedPair, failedAmt)
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
		m.reportPairFailure(result.timeReply, failedPair, 0)
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
		m.reportPairFailure(result.timeReply, failedPair, 0)
		m.reportPairFailure(
			result.timeReply, failedPair.Reverse(), 0,
		)
		return false, 0

	// Any other failure or an empty failure will get the node pruned.
	default:
		m.reportVertexFailure(result.timeReply, failureVertex)
		return false, 0
	}
}

// getFailedPair tries to locate the failing pair given a route and the pubkey
// of the node that sent the failure. It will assume that the failure is
// associated with the outgoing channel set of the failing node. As a second
// result, it returns the amount sent between the pair.
func getFailedPair(route *route.Route, failureSource int) (DirectedNodePair,
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
		return NewDirectedNodePair(
			route.SourcePubKey,
			route.Hops[0].PubKeyBytes,
		), route.TotalAmount
	}

	return NewDirectedNodePair(
		route.Hops[failureSource-1].PubKeyBytes,
		route.Hops[failureSource].PubKeyBytes,
	), route.Hops[failureSource-1].AmtToForward
}
