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

	// now is expected to return the current time. It is supplied as an
	// external function to enable deterministic unit tests.
	now func() time.Time

	cfg *MissionControlConfig

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

// NewMissionControl returns a new instance of missionControl.
func NewMissionControl(cfg *MissionControlConfig) *MissionControl {
	log.Debugf("Instantiating mission control with config: "+
		"PenaltyHalfLife=%v, AprioriHopProbability=%v",
		cfg.PenaltyHalfLife, cfg.AprioriHopProbability)

	return &MissionControl{
		history: make(map[route.Vertex]*nodeHistory),
		now:     time.Now,
		cfg:     cfg,
	}
}


// ResetHistory resets the history of MissionControl returning it to a state as
// if no payment attempts have been made.
func (m *MissionControl) ResetHistory() {
	m.Lock()
	defer m.Unlock()

	m.history = make(map[route.Vertex]*nodeHistory)

	log.Debugf("Mission control history cleared")
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

// ReportVertexFailure reports a node level failure.
func (m *MissionControl) ReportVertexFailure(v route.Vertex) {
	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	now := m.now()

	m.Lock()
	defer m.Unlock()

	history := m.createHistoryIfNotExists(v)
	history.lastFail = &now
}

// ReportEdgeFailure reports a channel level failure.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (m *MissionControl) ReportEdgeFailure(failedEdge edge,
	minPenalizeAmt lnwire.MilliSatoshi) {

	log.Debugf("Reporting channel %v failure to Mission Control",
		failedEdge.channel)

	now := m.now()

	m.Lock()
	defer m.Unlock()

	history := m.createHistoryIfNotExists(failedEdge.from)
	history.channelLastFail[failedEdge.channel] = &channelHistory{
		lastFail:       now,
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
