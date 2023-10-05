package routing

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// CapacityFraction and capacitySmearingFraction define how
	// capacity-related probability reweighting works. CapacityFraction
	// defines the fraction of the channel capacity at which the effect
	// roughly sets in and capacitySmearingFraction defines over which range
	// the factor changes from 1 to minCapacityFactor.

	// DefaultCapacityFraction is the default value for CapacityFraction.
	// It is chosen such that the capacity factor is active but with a small
	// effect. This value together with capacitySmearingFraction leads to a
	// noticeable reduction in probability if the amount starts to come
	// close to 90% of a channel's capacity.
	DefaultCapacityFraction = 0.9999

	// capacitySmearingFraction defines how quickly the capacity factor
	// drops from 1 to minCapacityFactor. This value results in about a
	// variation over 20% of the capacity.
	capacitySmearingFraction = 0.025

	// minCapacityFactor is the minimal value the capacityFactor can take.
	// Having a too low value can lead to discarding of paths due to the
	// enforced minimal probability or to too high pathfinding weights.
	minCapacityFactor = 0.5

	// minCapacityFraction is the minimum allowed value for
	// CapacityFraction. The success probability in the random balance model
	// (which may not be an accurate description of the liquidity
	// distribution in the network) can be approximated with P(a) = 1 - a/c,
	// for amount a and capacity c. If we require a probability P(a) = 0.25,
	// this translates into a value of 0.75 for a/c. We limit this value in
	// order to not discard too many channels.
	minCapacityFraction = 0.75

	// AprioriEstimatorName is used to identify the apriori probability
	// estimator.
	AprioriEstimatorName = "apriori"
)

var (
	// ErrInvalidHalflife is returned when we get an invalid half life.
	ErrInvalidHalflife = errors.New("penalty half life must be >= 0")

	// ErrInvalidHopProbability is returned when we get an invalid hop
	// probability.
	ErrInvalidHopProbability = errors.New("hop probability must be in " +
		"[0, 1]")

	// ErrInvalidAprioriWeight is returned when we get an apriori weight
	// that is out of range.
	ErrInvalidAprioriWeight = errors.New("apriori weight must be in [0, 1]")

	// ErrInvalidCapacityFraction is returned when we get a capacity
	// fraction that is out of range.
	ErrInvalidCapacityFraction = fmt.Errorf("capacity fraction must be in "+
		"[%v, 1]", minCapacityFraction)
)

// AprioriConfig contains configuration for our probability estimator.
type AprioriConfig struct {
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

	// CapacityFraction is the fraction of a channel's capacity that we
	// consider to have liquidity. For amounts that come close to or exceed
	// the fraction, an additional penalty is applied. A value of 1.0
	// disables the capacityFactor.
	CapacityFraction float64
}

// validate checks the configuration of the estimator for allowed values.
func (p AprioriConfig) validate() error {
	if p.PenaltyHalfLife < 0 {
		return ErrInvalidHalflife
	}

	if p.AprioriHopProbability < 0 || p.AprioriHopProbability > 1 {
		return ErrInvalidHopProbability
	}

	if p.AprioriWeight < 0 || p.AprioriWeight > 1 {
		return ErrInvalidAprioriWeight
	}

	if p.CapacityFraction < minCapacityFraction || p.CapacityFraction > 1 {
		return ErrInvalidCapacityFraction
	}

	return nil
}

// DefaultAprioriConfig returns the default configuration for the estimator.
func DefaultAprioriConfig() AprioriConfig {
	return AprioriConfig{
		PenaltyHalfLife:       DefaultPenaltyHalfLife,
		AprioriHopProbability: DefaultAprioriHopProbability,
		AprioriWeight:         DefaultAprioriWeight,
		CapacityFraction:      DefaultCapacityFraction,
	}
}

// AprioriEstimator returns node and pair probabilities based on historical
// payment results. It uses a preconfigured success probability value for
// untried hops (AprioriHopProbability) and returns a high success probability
// for hops that could previously conduct a payment (prevSuccessProbability).
// Successful edges are retried until proven otherwise. Recently failed hops are
// penalized by an exponential time decay (PenaltyHalfLife), after which they
// are reconsidered for routing. If information was learned about a forwarding
// node, the information is taken into account to estimate a per node
// probability that mixes with the a priori probability (AprioriWeight).
type AprioriEstimator struct {
	// AprioriConfig contains configuration options for our estimator.
	AprioriConfig

	// prevSuccessProbability is the assumed probability for node pairs that
	// successfully relayed the previous attempt.
	prevSuccessProbability float64
}

// NewAprioriEstimator creates a new AprioriEstimator.
func NewAprioriEstimator(cfg AprioriConfig) (*AprioriEstimator, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &AprioriEstimator{
		AprioriConfig:          cfg,
		prevSuccessProbability: prevSuccessProbability,
	}, nil
}

// Compile-time checks that interfaces are implemented.
var _ Estimator = (*AprioriEstimator)(nil)
var _ estimatorConfig = (*AprioriConfig)(nil)

// Config returns the estimator's configuration.
func (p *AprioriEstimator) Config() estimatorConfig {
	return p.AprioriConfig
}

// String returns the estimator's configuration as a string representation.
func (p *AprioriEstimator) String() string {
	return fmt.Sprintf("estimator type: %v, penalty halflife time: %v, "+
		"apriori hop probability: %v, apriori weight: %v, previous "+
		"success probability: %v, capacity fraction: %v",
		AprioriEstimatorName, p.PenaltyHalfLife,
		p.AprioriHopProbability, p.AprioriWeight,
		p.prevSuccessProbability, p.CapacityFraction)
}

// getNodeProbability calculates the probability for connections from a node
// that have not been tried before. The results parameter is a list of last
// payment results for that node.
func (p *AprioriEstimator) getNodeProbability(now time.Time,
	results NodeResults, amt lnwire.MilliSatoshi,
	capacity btcutil.Amount) float64 {

	// We reduce the apriori hop probability if the amount comes close to
	// the capacity.
	apriori := p.AprioriHopProbability * capacityFactor(
		amt, capacity, p.CapacityFraction,
	)

	// If the channel history is not to be taken into account, we can return
	// early here with the configured a priori probability.
	if p.AprioriWeight == 1 {
		return apriori
	}

	// If there is no channel history, our best estimate is still the a
	// priori probability.
	if len(results) == 0 {
		return apriori
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
	probabilitiesTotal := apriori * aprioriFactor
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
func (p *AprioriEstimator) getWeight(age time.Duration) float64 {
	exp := -age.Hours() / p.PenaltyHalfLife.Hours()
	return math.Pow(2, exp)
}

// capacityFactor is a multiplier that can be used to reduce the probability
// depending on how much of the capacity is sent. In other words, the factor
// sorts out channels that don't provide enough liquidity. Effectively, this
// leads to usage of larger channels in total to increase success probability,
// but it may also increase fees. The limits are 1 for amt == 0 and
// minCapacityFactor for amt >> capacityCutoffFraction. The function drops
// significantly when amt reaches cutoffMsat. smearingMsat determines over which
// scale the reduction takes place.
func capacityFactor(amt lnwire.MilliSatoshi, capacity btcutil.Amount,
	capacityCutoffFraction float64) float64 {

	// The special value of 1.0 for capacityFactor disables any effect from
	// this factor.
	if capacityCutoffFraction == 1 {
		return 1.0
	}

	// If we don't have information about the capacity, which can be the
	// case for hop hints or local channels, we return unity to not alter
	// anything.
	if capacity == 0 {
		return 1.0
	}

	capMsat := float64(lnwire.NewMSatFromSatoshis(capacity))
	amtMsat := float64(amt)

	if amtMsat > capMsat {
		return 0
	}

	cutoffMsat := capacityCutoffFraction * capMsat
	smearingMsat := capacitySmearingFraction * capMsat

	// We compute a logistic function mirrored around the y axis, centered
	// at cutoffMsat, decaying over the smearingMsat scale.
	denominator := 1 + math.Exp(-(amtMsat-cutoffMsat)/smearingMsat)

	// The numerator decides what the minimal value of this function will
	// be. The minimal value is set by minCapacityFactor.
	numerator := 1 - minCapacityFactor

	return 1 - numerator/denominator
}

// PairProbability estimates the probability of successfully traversing to
// toNode based on historical payment outcomes for the from node. Those outcomes
// are passed in via the results parameter.
func (p *AprioriEstimator) PairProbability(now time.Time,
	results NodeResults, toNode route.Vertex, amt lnwire.MilliSatoshi,
	capacity btcutil.Amount) float64 {

	nodeProbability := p.getNodeProbability(now, results, amt, capacity)

	return p.calculateProbability(
		now, results, nodeProbability, toNode, amt,
	)
}

// LocalPairProbability estimates the probability of successfully traversing
// our own local channels to toNode.
func (p *AprioriEstimator) LocalPairProbability(
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
func (p *AprioriEstimator) calculateProbability(
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
