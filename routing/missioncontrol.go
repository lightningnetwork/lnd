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

	// prevSuccessProbability is the assumed probability for node pairs that
	// successfully relayed the previous attempt.
	prevSuccessProbability = 0.95
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
	// lastPairResult tracks the last payment result per node pair.
	lastPairResult map[DirectedNodePair]timedPairResult

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

// timedPairResult describes a timestamped pair result.
type timedPairResult struct {
	// timestamp is the time when this result was obtained.
	timestamp time.Time

	pairResult
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

	// Timestamp is the time of last result.
	Timestamp time.Time

	// MinPenalizeAmt is the minimum amount for which the channel will be
	// penalized.
	MinPenalizeAmt lnwire.MilliSatoshi

	// SuccessProb is the success probability estimation for this channel.
	SuccessProb float64

	// LastAttemptSuccessful indicates whether the last payment attempt
	// through this pair was successful.
	LastAttemptSuccessful bool
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
		lastPairResult:   make(map[DirectedNodePair]timedPairResult),
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

	m.lastPairResult = make(map[DirectedNodePair]timedPairResult)
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
	lastPairResult, ok := m.lastPairResult[pair]

	// Only look at the last pair outcome if it happened after the last node
	// level failure. Otherwise the node level failure is the most recent
	// and used as the basis for calculation of the probability.
	if ok && lastPairResult.timestamp.After(lastFail) {
		if lastPairResult.success {
			return prevSuccessProbability
		}

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

// GetHistorySnapshot takes a snapshot from the current mission control state
// and actual probability estimates.
func (m *MissionControl) GetHistorySnapshot() *MissionControlSnapshot {
	m.Lock()
	defer m.Unlock()

	log.Debugf("Requesting history snapshot from mission control: "+
		"node_failure_count=%v, pair_result_count=%v",
		len(m.lastNodeFailure), len(m.lastPairResult))

	nodes := make([]MissionControlNodeSnapshot, 0, len(m.lastNodeFailure))
	for v, h := range m.lastNodeFailure {
		otherProb := m.getPairProbability(v, route.Vertex{}, 0)

		nodes = append(nodes, MissionControlNodeSnapshot{
			Node:             v,
			LastFail:         h,
			OtherSuccessProb: otherProb,
		})
	}

	pairs := make([]MissionControlPairSnapshot, 0, len(m.lastPairResult))

	for v, h := range m.lastPairResult {
		// Show probability assuming amount meets min
		// penalization amount.
		prob := m.getPairProbability(v.From, v.To, h.minPenalizeAmt)

		pair := MissionControlPairSnapshot{
			Pair:                  v,
			MinPenalizeAmt:        h.minPenalizeAmt,
			Timestamp:             h.timestamp,
			SuccessProb:           prob,
			LastAttemptSuccessful: h.success,
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
// returns a reason if this failure is a final failure. In that case no further
// payment attempts need to be made.
func (m *MissionControl) ReportPaymentFail(paymentID uint64, rt *route.Route,
	failureSourceIdx *int, failure lnwire.FailureMessage) (
	*channeldb.FailureReason, error) {

	timestamp := m.now()

	result := &paymentResult{
		success:          false,
		timeFwd:          timestamp,
		timeReply:        timestamp,
		id:               paymentID,
		failureSourceIdx: failureSourceIdx,
		failure:          failure,
		route:            rt,
	}

	return m.processPaymentResult(result)
}

// ReportPaymentSuccess reports a successful payment to mission control as input
// for future probability estimates.
func (m *MissionControl) ReportPaymentSuccess(paymentID uint64,
	rt *route.Route) error {

	timestamp := m.now()

	result := &paymentResult{
		timeFwd:   timestamp,
		timeReply: timestamp,
		id:        paymentID,
		success:   true,
		route:     rt,
	}

	_, err := m.processPaymentResult(result)
	return err
}

// processPaymentResult stores a payment result in the mission control store and
// updates mission control's in-memory state.
func (m *MissionControl) processPaymentResult(result *paymentResult) (
	*channeldb.FailureReason, error) {

	// Store complete result in database.
	if err := m.store.AddResult(result); err != nil {
		return nil, err
	}

	// Apply result to update mission control state.
	reason := m.applyPaymentResult(result)

	return reason, nil
}

// applyPaymentResult applies a payment result as input for future probability
// estimates. It returns a bool indicating whether this error is a final error
// and no further payment attempts need to be made.
func (m *MissionControl) applyPaymentResult(
	result *paymentResult) *channeldb.FailureReason {

	// Interpret result.
	i := interpretResult(
		result.route, result.success, result.failureSourceIdx,
		result.failure,
	)

	// Update mission control state using the interpretation.
	m.Lock()
	defer m.Unlock()

	if i.policyFailure != nil {
		if m.requestSecondChance(
			result.timeReply,
			i.policyFailure.From, i.policyFailure.To,
		) {
			return nil
		}
	}

	if i.nodeFailure != nil {
		log.Debugf("Reporting node failure to Mission Control: "+
			"node=%v", *i.nodeFailure)

		m.lastNodeFailure[*i.nodeFailure] = result.timeReply
	}

	for pair, pairResult := range i.pairResults {
		if pairResult.success {
			log.Debugf("Reporting pair success to Mission "+
				"Control: pair=%v", pair)
		} else {
			log.Debugf("Reporting pair failure to Mission "+
				"Control: pair=%v, minPenalizeAmt=%v",
				pair, pairResult.minPenalizeAmt)
		}

		m.lastPairResult[pair] = timedPairResult{
			timestamp:  result.timeReply,
			pairResult: pairResult,
		}
	}

	return i.finalFailureReason
}
