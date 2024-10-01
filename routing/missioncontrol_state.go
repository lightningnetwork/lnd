package routing

import (
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
)

// missionControlState is an object that manages the internal mission control
// state. Note that it isn't thread safe and synchronization needs to be
// enforced externally.
type missionControlState struct {
	// lastPairResult tracks the last payment result (on a pair basis) for
	// each transited node. This is a multi-layer map that allows us to look
	// up the failure history of all connected channels (node pairs) for a
	// particular node.
	lastPairResult map[route.Vertex]NodeResults

	// lastSecondChance tracks the last time a second chance was granted for
	// a directed node pair.
	lastSecondChance map[DirectedNodePair]time.Time

	// minFailureRelaxInterval is the minimum time that must have passed
	// since the previously recorded failure before the failure amount may
	// be raised.
	minFailureRelaxInterval time.Duration
}

// newMissionControlState instantiates a new mission control state object.
func newMissionControlState(
	minFailureRelaxInterval time.Duration) *missionControlState {

	return &missionControlState{
		lastPairResult:          make(map[route.Vertex]NodeResults),
		lastSecondChance:        make(map[DirectedNodePair]time.Time),
		minFailureRelaxInterval: minFailureRelaxInterval,
	}
}

// getLastPairResult returns the current state for connections to the given
// node.
func (m *missionControlState) getLastPairResult(node route.Vertex) (NodeResults,
	bool) {

	result, ok := m.lastPairResult[node]
	return result, ok
}

// ResetHistory resets the history of missionControlState returning it to a
// state as if no payment attempts have been made.
func (m *missionControlState) resetHistory() {
	m.lastPairResult = make(map[route.Vertex]NodeResults)
	m.lastSecondChance = make(map[DirectedNodePair]time.Time)
}

// setLastPairResult stores a result for a node pair.
func (m *missionControlState) setLastPairResult(fromNode, toNode route.Vertex,
	timestamp time.Time, result *pairResult, force bool) {

	nodePairs, ok := m.lastPairResult[fromNode]
	if !ok {
		nodePairs = make(NodeResults)
		m.lastPairResult[fromNode] = nodePairs
	}

	current := nodePairs[toNode]

	// Apply the new result to the existing data for this pair. If there is
	// no existing data, apply it to the default values for TimedPairResult.
	if result.success {
		successAmt := result.amt
		current.SuccessTime = timestamp

		// Only update the success amount if this amount is higher. This
		// prevents the success range from shrinking when there is no
		// reason to do so. For example: small amount probes shouldn't
		// affect a previous success for a much larger amount.
		if force || successAmt > current.SuccessAmt {
			current.SuccessAmt = successAmt
		}

		// If the success amount goes into the failure range, move the
		// failure range up. Future attempts up to the success amount
		// are likely to succeed. We don't want to clear the failure
		// completely, because we haven't learnt much for amounts above
		// the current success amount.
		if force || (!current.FailTime.IsZero() &&
			successAmt >= current.FailAmt) {

			current.FailAmt = successAmt + 1
		}
	} else {
		// For failures we always want to update both the amount and the
		// time. Those need to relate to the same result, because the
		// time is used to gradually diminish the penalty for that
		// specific result. Updating the timestamp but not the amount
		// could cause a failure for a lower amount (a more severe
		// condition) to be revived as if it just happened.
		failAmt := result.amt

		// Drop result if it would increase the failure amount too soon
		// after a previous failure. This can happen if htlc results
		// come in out of order. This check makes it easier for payment
		// processes to converge to a final state.
		failInterval := timestamp.Sub(current.FailTime)
		if !force && failAmt > current.FailAmt &&
			failInterval < m.minFailureRelaxInterval {

			log.Debugf("Ignoring higher amount failure within min "+
				"failure relaxation interval: prev_fail_amt=%v, "+
				"fail_amt=%v, interval=%v",
				current.FailAmt, failAmt, failInterval)

			return
		}

		current.FailTime = timestamp
		current.FailAmt = failAmt

		switch {
		// The failure amount is set to zero when the failure is
		// amount-independent, meaning that the attempt would have
		// failed regardless of the amount. This should also reset the
		// success amount to zero.
		case failAmt == 0:
			current.SuccessAmt = 0

		// If the failure range goes into the success range, move the
		// success range down.
		case failAmt <= current.SuccessAmt:
			current.SuccessAmt = failAmt - 1
		}
	}

	log.Debugf("Setting %v->%v range to [%v-%v]",
		fromNode, toNode, current.SuccessAmt, current.FailAmt)

	nodePairs[toNode] = current
}

// setAllFail stores a fail result for all known connections to and from the
// given node.
func (m *missionControlState) setAllFail(node route.Vertex,
	timestamp time.Time) {

	for fromNode, nodePairs := range m.lastPairResult {
		for toNode := range nodePairs {
			if fromNode == node || toNode == node {
				nodePairs[toNode] = TimedPairResult{
					FailTime: timestamp,
				}
			}
		}
	}
}

// requestSecondChance checks whether the node fromNode can have a second chance
// at providing a channel update for its channel with toNode.
func (m *missionControlState) requestSecondChance(timestamp time.Time,
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
func (m *missionControlState) getSnapshot() *MissionControlSnapshot {
	log.Debugf("Requesting history snapshot from mission control: "+
		"pair_result_count=%v", len(m.lastPairResult))

	pairs := make([]MissionControlPairSnapshot, 0, len(m.lastPairResult))

	for fromNode, fromPairs := range m.lastPairResult {
		for toNode, result := range fromPairs {
			pair := NewDirectedNodePair(fromNode, toNode)

			pairSnapshot := MissionControlPairSnapshot{
				Pair:            pair,
				TimedPairResult: result,
			}

			pairs = append(pairs, pairSnapshot)
		}
	}

	snapshot := MissionControlSnapshot{
		Pairs: pairs,
	}

	return &snapshot
}

// importSnapshot takes an existing snapshot and merges it with our current
// state if the result provided are fresher than our current results. It returns
// the number of pairs that were used.
func (m *missionControlState) importSnapshot(snapshot *MissionControlSnapshot,
	force bool) int {

	var imported int

	for _, pair := range snapshot.Pairs {
		fromNode := pair.Pair.From
		toNode := pair.Pair.To

		results, found := m.getLastPairResult(fromNode)
		if !found {
			results = make(map[route.Vertex]TimedPairResult)
		}

		lastResult := results[toNode]

		failResult := failPairResult(pair.FailAmt)
		imported += m.importResult(
			lastResult.FailTime, pair.FailTime, failResult,
			fromNode, toNode, force,
		)

		successResult := successPairResult(pair.SuccessAmt)
		imported += m.importResult(
			lastResult.SuccessTime, pair.SuccessTime, successResult,
			fromNode, toNode, force,
		)
	}

	return imported
}

func (m *missionControlState) importResult(currentTs, importedTs time.Time,
	importedResult pairResult, fromNode, toNode route.Vertex,
	force bool) int {

	if !force && currentTs.After(importedTs) {
		log.Debugf("Not setting pair result for %v->%v (%v) "+
			"success=%v, timestamp %v older than last result %v",
			fromNode, toNode, importedResult.amt,
			importedResult.success, importedTs, currentTs)

		return 0
	}

	m.setLastPairResult(
		fromNode, toNode, importedTs, &importedResult, force,
	)

	return 1
}
