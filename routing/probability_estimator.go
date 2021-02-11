package routing

import (
	"errors"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrInvalidHalflife is returned when we get an invalid half life.
	ErrInvalidHalflife = errors.New("penalty half life must be >= 0")

	// ErrInvalidHopProbability is returned when we get an invalid hop
	// probability.
	ErrInvalidHopProbability = errors.New("hop probability must be in [0;1]")

	// ErrInvalidAprioriWeight is returned when we get an apriori weight
	// that is out of range.
	ErrInvalidAprioriWeight = errors.New("apriori weight must be in [0;1]")
)

// ProbabilityEstimatorCfg contains configuration for our probability estimator.
type ProbabilityEstimatorCfg struct {
	// PenaltyHalfLife defines after how much time a penalized node or
	// channel is back at 50% probability.
	PenaltyHalfLife time.Duration

	// AprioriHopProbability is the assumed success probability of a hop in
	// a route when no other information is available.
	AprioriHopProbability float64

	// AprioriWeight is a value in the range [0, 1] that defines to what
	// extent historical results should be extrapolated to untried
	// connections. Setting it to one will completely ignore historical
	// results and always assume the configured a priori probability for
	// untried connections. A value of zero will ignore the a priori
	// probability completely and only base the probability on historical
	// results, unless there are none available.
	AprioriWeight float64
}

func (p ProbabilityEstimatorCfg) validate() error {
	if p.PenaltyHalfLife < 0 {
		return ErrInvalidHalflife
	}

	if p.AprioriHopProbability < 0 || p.AprioriHopProbability > 1 {
		return ErrInvalidHopProbability
	}

	if p.AprioriWeight < 0 || p.AprioriWeight > 1 {
		return ErrInvalidAprioriWeight
	}

	return nil
}

// probabilityEstimator returns node and pair probabilities based on historical
// payment results.
type probabilityEstimator struct {
	// ProbabilityEstimatorCfg contains configuration options for our
	// estimator.
	ProbabilityEstimatorCfg

	// prevSuccessProbability is the assumed probability for node pairs that
	// successfully relayed the previous attempt.
	prevSuccessProbability float64
}

// getNodeProbability calculates the probability for connections from a node
// that have not been tried before. The results parameter is a list of last
// payment results for that node.
func (p *probabilityEstimator) getNodeProbability(now time.Time,
	results NodeResults, amt lnwire.MilliSatoshi) float64 {

	// If the channel history is not to be taken into account, we can return
	// early here with the configured a priori probability.
	if p.AprioriWeight == 1 {
		return p.AprioriHopProbability
	}

	// If there is no channel history, our best estimate is still the a
	// priori probability.
	if len(results) == 0 {
		return p.AprioriHopProbability
	}

	// The value of the apriori weight is in the range [0, 1]. Convert it to
	// a factor that properly expresses the intention of the weight in the
	// following weight average calculation. When the apriori weight is 0,
	// the apriori factor is also 0. This means it won't have any effect on
	// the weighted average calculation below. When the apriori weight
	// approaches 1, the apriori factor goes to infinity. It will heavily
	// outweigh any observations that have been collected.
	aprioriFactor := 1/(1-p.AprioriWeight) - 1

	// Calculate a weighted average consisting of the apriori probability
	// and historical observations. This is the part that incentivizes nodes
	// to make sure that all (not just some) of their channels are in good
	// shape. Senders will steer around nodes that have shown a few
	// failures, even though there may be many channels still untried.
	//
	// If there is just a single observation and the apriori weight is 0,
	// this single observation will totally determine the node probability.
	// The node probability is returned for all other channels of the node.
	// This means that one failure will lead to the success probability
	// estimates for all other channels being 0 too. The probability for the
	// channel that was tried will not even recover, because it is
	// recovering to the node probability (which is zero). So one failure
	// effectively prunes all channels of the node forever. This is the most
	// aggressive way in which we can penalize nodes and unlikely to yield
	// good results in a real network.
	probabilitiesTotal := p.AprioriHopProbability * aprioriFactor
	totalWeight := aprioriFactor

	for _, result := range results {
		switch {

		// Weigh success with a constant high weight of 1. There is no
		// decay. Amt is never zero, so this clause is never executed
		// when result.SuccessAmt is zero.
		case amt <= result.SuccessAmt:
			totalWeight++
			probabilitiesTotal += p.prevSuccessProbability

		// Weigh failures in accordance with their age. The base
		// probability of a failure is considered zero, so nothing needs
		// to be added to probabilitiesTotal.
		case !result.FailTime.IsZero() && amt >= result.FailAmt:
			age := now.Sub(result.FailTime)
			totalWeight += p.getWeight(age)
		}
	}

	return probabilitiesTotal / totalWeight
}

// getWeight calculates a weight in the range [0, 1] that should be assigned to
// a payment result. Weight follows an exponential curve that starts at 1 when
// the result is fresh and asymptotically approaches zero over time. The rate at
// which this happens is controlled by the penaltyHalfLife parameter.
func (p *probabilityEstimator) getWeight(age time.Duration) float64 {
	exp := -age.Hours() / p.PenaltyHalfLife.Hours()
	return math.Pow(2, exp)
}

// getPairProbability estimates the probability of successfully traversing to
// toNode based on historical payment outcomes for the from node. Those outcomes
// are passed in via the results parameter.
func (p *probabilityEstimator) getPairProbability(
	now time.Time, results NodeResults,
	toNode route.Vertex, amt lnwire.MilliSatoshi) float64 {

	nodeProbability := p.getNodeProbability(now, results, amt)

	return p.calculateProbability(
		now, results, nodeProbability, toNode, amt,
	)
}

// getLocalPairProbability estimates the probability of successfully traversing
// our own local channels to toNode.
func (p *probabilityEstimator) getLocalPairProbability(
	now time.Time, results NodeResults, toNode route.Vertex) float64 {

	// For local channels that have never been tried before, we assume them
	// to be successful. We have accurate balance and online status
	// information on our own channels, so when we select them in a route it
	// is close to certain that those channels will work.
	nodeProbability := p.prevSuccessProbability

	return p.calculateProbability(
		now, results, nodeProbability, toNode, lnwire.MaxMilliSatoshi,
	)
}

// calculateProbability estimates the probability of successfully traversing to
// toNode based on historical payment outcomes and a fall-back node probability.
func (p *probabilityEstimator) calculateProbability(
	now time.Time, results NodeResults,
	nodeProbability float64, toNode route.Vertex,
	amt lnwire.MilliSatoshi) float64 {

	// Retrieve the last pair outcome.
	lastPairResult, ok := results[toNode]

	// If there is no history for this pair, return the node probability
	// that is a probability estimate for untried channel.
	if !ok {
		return nodeProbability
	}

	// For successes, we have a fixed (high) probability. Those pairs will
	// be assumed good until proven otherwise. Amt is never zero, so this
	// clause is never executed when lastPairResult.SuccessAmt is zero.
	if amt <= lastPairResult.SuccessAmt {
		return p.prevSuccessProbability
	}

	// Take into account a minimum penalize amount. For balance errors, a
	// failure may be reported with such a minimum to prevent too aggressive
	// penalization. If the current amount is smaller than the amount that
	// previously triggered a failure, we act as if this is an untried
	// channel.
	if lastPairResult.FailTime.IsZero() || amt < lastPairResult.FailAmt {
		return nodeProbability
	}

	timeSinceLastFailure := now.Sub(lastPairResult.FailTime)

	// Calculate success probability based on the weight of the last
	// failure. When the failure is fresh, its weight is 1 and we'll return
	// probability 0. Over time the probability recovers to the node
	// probability. It would be as if this channel was never tried before.
	weight := p.getWeight(timeSinceLastFailure)
	probability := nodeProbability * (1 - weight)

	return probability
}
