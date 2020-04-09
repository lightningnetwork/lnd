package routing

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
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

	// DefaultAprioriWeight is the default a priori weight. See
	// MissionControlConfig for further explanation.
	DefaultAprioriWeight = 0.5

	// DefaultMinFailureRelaxInterval is the default minimum time that must
	// have passed since the previously recorded failure before the failure
	// amount may be raised.
	DefaultMinFailureRelaxInterval = time.Minute
)

// NodeResults contains previous results from a node to its peers.
type NodeResults map[route.Vertex]TimedPairResult

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
	// state is the internal mission control state that is input for
	// probability estimation.
	state *missionControlState

	// now is expected to return the current time. It is supplied as an
	// external function to enable deterministic unit tests.
	now func() time.Time

	cfg *MissionControlConfig

	store *missionControlStore

	// estimator is the probability estimator that is used with the payment
	// results that mission control collects.
	estimator *probabilityEstimator

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

	// AprioriWeight is a value in the range [0, 1] that defines to what
	// extent historical results should be extrapolated to untried
	// connections. Setting it to one will completely ignore historical
	// results and always assume the configured a priori probability for
	// untried connections. A value of zero will ignore the a priori
	// probability completely and only base the probability on historical
	// results, unless there are none available.
	AprioriWeight float64

	// MinFailureRelaxInterval is the minimum time that must have passed
	// since the previously recorded failure before the failure amount may
	// be raised.
	MinFailureRelaxInterval time.Duration

	// SelfNode is our own pubkey.
	SelfNode route.Vertex
}

// TimedPairResult describes a timestamped pair result.
type TimedPairResult struct {
	// FailTime is the time of the last failure.
	FailTime time.Time

	// FailAmt is the amount of the last failure. This amount may be pushed
	// up if a later success is higher than the last failed amount.
	FailAmt lnwire.MilliSatoshi

	// SuccessTime is the time of the last success.
	SuccessTime time.Time

	// SuccessAmt is the highest amount that successfully forwarded. This
	// isn't necessarily the last success amount. The value of this field
	// may also be pushed down if a later failure is lower than the highest
	// success amount. Because of this, SuccessAmt may not match
	// SuccessTime.
	SuccessAmt lnwire.MilliSatoshi
}

// MissionControlSnapshot contains a snapshot of the current state of mission
// control.
type MissionControlSnapshot struct {
	// Pairs is a list of channels for which specific information is
	// logged.
	Pairs []MissionControlPairSnapshot
}

// MissionControlPairSnapshot contains a snapshot of the current node pair
// state in mission control.
type MissionControlPairSnapshot struct {
	// Pair is the node pair of which the state is described.
	Pair DirectedNodePair

	// TimedPairResult contains the data for this pair.
	TimedPairResult
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
func NewMissionControl(db kvdb.Backend, cfg *MissionControlConfig) (
	*MissionControl, error) {

	log.Debugf("Instantiating mission control with config: "+
		"PenaltyHalfLife=%v, AprioriHopProbability=%v, "+
		"AprioriWeight=%v", cfg.PenaltyHalfLife,
		cfg.AprioriHopProbability, cfg.AprioriWeight)

	store, err := newMissionControlStore(db, cfg.MaxMcHistory)
	if err != nil {
		return nil, err
	}

	estimator := &probabilityEstimator{
		aprioriHopProbability:  cfg.AprioriHopProbability,
		aprioriWeight:          cfg.AprioriWeight,
		penaltyHalfLife:        cfg.PenaltyHalfLife,
		prevSuccessProbability: prevSuccessProbability,
	}

	mc := &MissionControl{
		state:     newMissionControlState(cfg.MinFailureRelaxInterval),
		now:       time.Now,
		cfg:       cfg,
		store:     store,
		estimator: estimator,
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
		"n=%v, time=%v", len(results), time.Since(start))

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

	m.state.resetHistory()

	log.Debugf("Mission control history cleared")

	return nil
}

// GetProbability is expected to return the success probability of a payment
// from fromNode along edge.
func (m *MissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	m.Lock()
	defer m.Unlock()

	now := m.now()
	results, _ := m.state.getLastPairResult(fromNode)

	// Use a distinct probability estimation function for local channels.
	if fromNode == m.cfg.SelfNode {
		return m.estimator.getLocalPairProbability(now, results, toNode)
	}

	return m.estimator.getPairProbability(now, results, toNode, amt)
}

// GetHistorySnapshot takes a snapshot from the current mission control state
// and actual probability estimates.
func (m *MissionControl) GetHistorySnapshot() *MissionControlSnapshot {
	m.Lock()
	defer m.Unlock()

	log.Debugf("Requesting history snapshot from mission control")

	return m.state.getSnapshot()
}

// GetPairHistorySnapshot returns the stored history for a given node pair.
func (m *MissionControl) GetPairHistorySnapshot(
	fromNode, toNode route.Vertex) TimedPairResult {

	m.Lock()
	defer m.Unlock()

	results, ok := m.state.getLastPairResult(fromNode)
	if !ok {
		return TimedPairResult{}
	}

	result, ok := results[toNode]
	if !ok {
		return TimedPairResult{}
	}

	return result
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
		if m.state.requestSecondChance(
			result.timeReply,
			i.policyFailure.From, i.policyFailure.To,
		) {
			return nil
		}
	}

	// If there is a node-level failure, record a failure for every tried
	// connection of that node. A node-level failure can be considered as a
	// failure that would have occurred with any of the node's channels.
	//
	// Ideally we'd also record the failure for the untried connections of
	// the node. Unfortunately this would require access to the graph and
	// adding this dependency and db calls does not outweigh the benefits.
	//
	// Untried connections will fall back to the node probability. After the
	// call to setAllPairResult below, the node probability will be equal to
	// the probability of the tried channels except that the a priori
	// probability is mixed in too. This effect is controlled by the
	// aprioriWeight parameter. If that parameter isn't set to an extreme
	// and there are a few known connections, there shouldn't be much of a
	// difference. The largest difference occurs when aprioriWeight is 1. In
	// that case, a node-level failure would not be applied to untried
	// channels.
	if i.nodeFailure != nil {
		log.Debugf("Reporting node failure to Mission Control: "+
			"node=%v", *i.nodeFailure)

		m.state.setAllFail(*i.nodeFailure, result.timeReply)
	}

	for pair, pairResult := range i.pairResults {
		pairResult := pairResult

		if pairResult.success {
			log.Debugf("Reporting pair success to Mission "+
				"Control: pair=%v, amt=%v",
				pair, pairResult.amt)
		} else {
			log.Debugf("Reporting pair failure to Mission "+
				"Control: pair=%v, amt=%v",
				pair, pairResult.amt)
		}

		m.state.setLastPairResult(
			pair.From, pair.To, result.timeReply, &pairResult,
		)
	}

	return i.finalFailureReason
}
