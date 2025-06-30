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
	// DefaultBimodalScaleMsat is the default value for BimodalScaleMsat in
	// BimodalConfig. It describes the distribution of funds in the LN based
	// on empirical findings. We assume an unbalanced network by default.
	DefaultBimodalScaleMsat = lnwire.MilliSatoshi(300_000_000)

	// DefaultBimodalNodeWeight is the default value for the
	// BimodalNodeWeight in BimodalConfig. It is chosen such that past
	// forwardings on other channels of a router are only slightly taken
	// into account.
	DefaultBimodalNodeWeight = 0.2

	// DefaultBimodalDecayTime is the default value for BimodalDecayTime.
	// We will forget about previous learnings about channel liquidity on
	// the timescale of about a week.
	DefaultBimodalDecayTime = 7 * 24 * time.Hour

	// BimodalScaleMsatMax is the maximum value for BimodalScaleMsat. We
	// limit it here to the fakeHopHintCapacity to avoid issues with hop
	// hint probability calculations.
	BimodalScaleMsatMax = lnwire.MilliSatoshi(
		1000 * fakeHopHintCapacity / 4,
	)

	// BimodalEstimatorName is used to identify the bimodal estimator.
	BimodalEstimatorName = "bimodal"
)

var (
	// ErrInvalidScale is returned when we get a scale below or equal zero.
	ErrInvalidScale = errors.New("scale must be >= 0 and sane")

	// ErrInvalidNodeWeight is returned when we get a node weight that is
	// out of range.
	ErrInvalidNodeWeight = errors.New("node weight must be in [0, 1]")

	// ErrInvalidDecayTime is returned when we get a decay time below zero.
	ErrInvalidDecayTime = errors.New("decay time must be larger than zero")

	// ErrZeroCapacity is returned when we encounter a channel with zero
	// capacity in probability estimation.
	ErrZeroCapacity = errors.New("capacity must be larger than zero")
)

// BimodalConfig contains configuration for our probability estimator.
type BimodalConfig struct {
	// BimodalNodeWeight defines how strongly other previous forwardings on
	// channels of a router should be taken into account when computing a
	// channel's probability to route. The allowed values are in the range
	// [0, 1], where a value of 0 means that only direct information about a
	// channel is taken into account.
	BimodalNodeWeight float64

	// BimodalScaleMsat describes the scale over which channels
	// statistically have some liquidity left. The value determines how
	// quickly the bimodal distribution drops off from the edges of a
	// channel. A larger value (compared to typical channel capacities)
	// means that the drop off is slow and that channel balances are
	// distributed more uniformly. A small value leads to the assumption of
	// very unbalanced channels.
	BimodalScaleMsat lnwire.MilliSatoshi

	// BimodalDecayTime is the scale for the exponential information decay
	// over time for previous successes or failures.
	BimodalDecayTime time.Duration
}

// validate checks the configuration of the estimator for allowed values.
func (p BimodalConfig) validate() error {
	if p.BimodalDecayTime <= 0 {
		return fmt.Errorf("%v: %w", BimodalEstimatorName,
			ErrInvalidDecayTime)
	}

	if p.BimodalNodeWeight < 0 || p.BimodalNodeWeight > 1 {
		return fmt.Errorf("%v: %w", BimodalEstimatorName,
			ErrInvalidNodeWeight)
	}

	if p.BimodalScaleMsat == 0 || p.BimodalScaleMsat > BimodalScaleMsatMax {
		return fmt.Errorf("%v: %w", BimodalEstimatorName,
			ErrInvalidScale)
	}

	return nil
}

// DefaultBimodalConfig returns the default configuration for the estimator.
func DefaultBimodalConfig() BimodalConfig {
	return BimodalConfig{
		BimodalNodeWeight: DefaultBimodalNodeWeight,
		BimodalScaleMsat:  DefaultBimodalScaleMsat,
		BimodalDecayTime:  DefaultBimodalDecayTime,
	}
}

// BimodalEstimator returns node and pair probabilities based on historical
// payment results based on a liquidity distribution model of the LN. The main
// function is to estimate the direct channel probability based on a depleted
// liquidity distribution model, with additional information decay over time. A
// per-node probability can be mixed with the direct probability, taking into
// account successes/failures on other channels of the forwarder.
type BimodalEstimator struct {
	// BimodalConfig contains configuration options for our estimator.
	BimodalConfig
}

// NewBimodalEstimator creates a new BimodalEstimator.
func NewBimodalEstimator(cfg BimodalConfig) (*BimodalEstimator, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &BimodalEstimator{
		BimodalConfig: cfg,
	}, nil
}

// Compile-time checks that interfaces are implemented.
var _ Estimator = (*BimodalEstimator)(nil)
var _ estimatorConfig = (*BimodalConfig)(nil)

// config returns the current configuration of the estimator.
func (p *BimodalEstimator) Config() estimatorConfig {
	return p.BimodalConfig
}

// String returns the estimator's configuration as a string representation.
func (p *BimodalEstimator) String() string {
	return fmt.Sprintf("estimator type: %v, decay time: %v, liquidity "+
		"scale: %v, node weight: %v", BimodalEstimatorName,
		p.BimodalDecayTime, p.BimodalScaleMsat, p.BimodalNodeWeight)
}

// PairProbability estimates the probability of successfully traversing to
// toNode based on historical payment outcomes for the from node. Those outcomes
// are passed in via the results parameter.
func (p *BimodalEstimator) PairProbability(now time.Time,
	results NodeResults, toNode route.Vertex, amt lnwire.MilliSatoshi,
	capacity btcutil.Amount) float64 {

	// We first compute the probability for the desired hop taking into
	// account previous knowledge.
	directProbability := p.directProbability(
		now, results, toNode, amt, lnwire.NewMSatFromSatoshis(capacity),
	)

	// The final probability is computed by taking into account other
	// channels of the from node.
	return p.calculateProbability(directProbability, now, results, toNode)
}

// LocalPairProbability computes the probability to reach toNode given a set of
// previous learnings.
func (p *BimodalEstimator) LocalPairProbability(now time.Time,
	results NodeResults, toNode route.Vertex) float64 {

	// For direct local probabilities we assume to know exactly how much we
	// can send over a channel, which assumes that channels are active and
	// have enough liquidity.
	directProbability := 1.0

	// If we had an unexpected failure for this node, we reduce the
	// probability for some time to avoid infinite retries.
	result, ok := results[toNode]
	if ok && !result.FailTime.IsZero() {
		timeAgo := now.Sub(result.FailTime)

		// We only expect results in the past to get a probability
		// between 0 and 1.
		if timeAgo < 0 {
			timeAgo = 0
		}
		exponent := -float64(timeAgo) / float64(p.BimodalDecayTime)
		directProbability -= math.Exp(exponent)
	}

	return directProbability
}

// directProbability computes the probability to reach a node based on the
// liquidity distribution in the LN.
func (p *BimodalEstimator) directProbability(now time.Time,
	results NodeResults, toNode route.Vertex, amt lnwire.MilliSatoshi,
	capacity lnwire.MilliSatoshi) float64 {

	// We first determine the time-adjusted success and failure amounts to
	// then compute a probability. We know that we can send a zero amount.
	successAmount := lnwire.MilliSatoshi(0)

	// We know that we cannot send the full capacity.
	failAmount := capacity

	// If we have information about past successes or failures, we modify
	// them with a time decay.
	result, ok := results[toNode]
	if ok {
		// Apply a time decay for the amount we cannot send.
		if !result.FailTime.IsZero() {
			failAmount = cannotSend(
				result.FailAmt, capacity, now, result.FailTime,
				p.BimodalDecayTime,
			)
		}

		// Apply a time decay for the amount we can send.
		if !result.SuccessTime.IsZero() {
			successAmount = canSend(
				result.SuccessAmt, now, result.SuccessTime,
				p.BimodalDecayTime,
			)
		}
	}

	// Compute the direct channel probability.
	probability, err := p.probabilityFormula(
		capacity, successAmount, failAmount, amt,
	)
	if err != nil {
		log.Errorf("error computing probability to node: %v "+
			"(node: %v, results: %v, amt: %v, capacity: %v)",
			err, toNode, results, amt, capacity)

		return 0.0
	}

	return probability
}

// calculateProbability computes the total hop probability combining the channel
// probability and historic forwarding data of other channels of the node we try
// to send from.
//
// Goals:
// * We want to incentivize good routing nodes: the more routable channels a
// node has, the more we want to incentivize (vice versa for failures).
// -> We reduce/increase the direct probability depending on past
// failures/successes for other channels of the node.
//
// * We want to be forgiving/give other nodes a chance as well: we want to
// forget about (non-)routable channels over time.
// -> We weight the successes/failures with a time decay such that they will not
// influence the total probability if a long time went by.
//
// * If we don't have other info, we want to solely rely on the direct
// probability.
//
// * We want to be able to specify how important the other channels are compared
// to the direct channel.
// -> Introduce a node weight factor that weights the direct probability against
// the node-wide average. The larger the node weight, the more important other
// channels of the node are.
//
// How do failures on low fee nodes redirect routing to higher fee nodes?
// Assumptions:
// * attemptCostPPM of 1000 PPM
// * constant direct channel probability of P0 (usually 0.5 for large amounts)
// * node weight w of 0.2
//
// The question we want to answer is:
// How often would a zero-fee node be tried (even if there were failures for its
// other channels) over trying a high-fee node with 2000 PPM and no direct
// knowledge about the channel to send over?
//
// The probability of a route of length l is P(l) = l * P0.
//
// The total probability after n failures (with the implemented method here) is:
// P(l, n) = P(l-1) * P(n)
// = P(l-1) * (P0 + n*0) / (1 + n*w)
// = P(l) / (1 + n*w)
//
// Condition for a high-fee channel to overcome a low fee channel in the
// Dijkstra weight function (only looking at fee and probability PPM terms):
// highFeePPM + attemptCostPPM * 1/P(l) = 0PPM + attemptCostPPM * 1/P(l, n)
// highFeePPM/attemptCostPPM = 1/P(l, n) - 1/P(l) =
// = (1 + n*w)/P(l) - 1/P(l) =
// = n*w/P(l)
//
// Therefore:
// n = (highFeePPM/attemptCostPPM) * (P(l)/w) =
// = (2000/1000) * 0.5 * l / w = l/w
//
// For a one-hop route we get:
// n = 1/0.2 = 5 tolerated failures
//
// For a three-hop route we get:
// n = 3/0.2 = 15 tolerated failures
//
// For more details on the behavior see tests.
func (p *BimodalEstimator) calculateProbability(directProbability float64,
	now time.Time, results NodeResults, toNode route.Vertex) float64 {

	// If we don't take other channels into account, we can return early.
	if p.BimodalNodeWeight == 0.0 {
		return directProbability
	}

	// If we have up-to-date information about the channel we want to use,
	// i.e. the info stems from results not longer ago than the decay time,
	// we will only use the direct probability. This is needed in order to
	// avoid that other previous results (on all other channels of the same
	// routing node) will distort and pin the calculated probability even if
	// we have accurate direct information. This helps to dip the
	// probability below the min probability in case of failures, to start
	// the splitting process.
	directResult, ok := results[toNode]
	if ok {
		latest := directResult.SuccessTime
		if directResult.FailTime.After(latest) {
			latest = directResult.FailTime
		}

		// We use BimonodalDecayTime to judge the currentness of the
		// data. It is the time scale on which we assume to have lost
		// information.
		if now.Sub(latest) < p.BimodalDecayTime {
			log.Tracef("Using direct probability for node %v: %v",
				toNode, directResult)

			return directProbability
		}
	}

	// w is a parameter which determines how strongly the other channels of
	// a node should be incorporated, the higher the stronger.
	w := p.BimodalNodeWeight

	// dt determines the timeliness of the previous successes/failures
	// to be taken into account.
	dt := float64(p.BimodalDecayTime)

	// The direct channel probability is weighted fully, all other results
	// are weighted according to how recent the information is.
	totalProbabilities := directProbability
	totalWeights := 1.0

	for peer, result := range results {
		// We don't include the direct hop probability here because it
		// is already included in totalProbabilities.
		if peer == toNode {
			continue
		}

		// We add probabilities weighted by how recent the info is.
		var weight float64
		if result.SuccessAmt > 0 {
			exponent := -float64(now.Sub(result.SuccessTime)) / dt
			weight = math.Exp(exponent)
			totalProbabilities += w * weight
			totalWeights += w * weight
		}
		if result.FailAmt > 0 {
			exponent := -float64(now.Sub(result.FailTime)) / dt
			weight = math.Exp(exponent)

			// Failures don't add to total success probability.
			totalWeights += w * weight
		}
	}

	return totalProbabilities / totalWeights
}

// canSend returns the sendable amount over the channel, respecting time decay.
// canSend approaches zero, if we wait for a much longer time than the decay
// time.
func canSend(successAmount lnwire.MilliSatoshi, now, successTime time.Time,
	decayConstant time.Duration) lnwire.MilliSatoshi {

	// The factor approaches 0 for successTime a long time in the past,
	// is 1 when the successTime is now.
	factor := math.Exp(
		-float64(now.Sub(successTime)) / float64(decayConstant),
	)

	canSend := factor * float64(successAmount)

	return lnwire.MilliSatoshi(canSend)
}

// cannotSend returns the not sendable amount over the channel, respecting time
// decay. cannotSend approaches the capacity, if we wait for a much longer time
// than the decay time.
func cannotSend(failAmount, capacity lnwire.MilliSatoshi, now,
	failTime time.Time, decayConstant time.Duration) lnwire.MilliSatoshi {

	if failAmount > capacity {
		failAmount = capacity
	}

	// The factor approaches 0 for failTime a long time in the past and it
	// is 1 when the failTime is now.
	factor := math.Exp(
		-float64(now.Sub(failTime)) / float64(decayConstant),
	)

	cannotSend := capacity - lnwire.MilliSatoshi(
		factor*float64(capacity-failAmount),
	)

	return cannotSend
}

// primitive computes the indefinite integral of our assumed (normalized)
// liquidity probability distribution. The distribution of liquidity x here is
// the function P(x) ~ exp(-x/s) + exp((x-c)/s) + 1/c, i.e., two exponentials
// residing at the ends of channels. This means that we expect liquidity to be
// at either side of the channel with capacity c. The s parameter (scale)
// defines how far the liquidity leaks into the channel. A very low scale
// assumes completely unbalanced channels, a very high scale assumes a random
// distribution. More details can be found in
// https://github.com/lightningnetwork/lnd/issues/5988#issuecomment-1131234858.
// Additionally, we add a constant term 1/c to the distribution to avoid
// normalization issues and to fall back to a uniform distribution should the
// previous success and fail amounts contradict a bimodal distribution.
func (p *BimodalEstimator) primitive(c, x float64) float64 {
	s := float64(p.BimodalScaleMsat)

	// The indefinite integral of P(x) is given by
	// Int P(x) dx = H(x) = s * (-e(-x/s) + e((x-c)/s) + x/(c*s)),
	// and its norm from 0 to c can be computed from it,
	// norm = [H(x)]_0^c = s * (-e(-c/s) + 1 + 1/s -(-1 + e(-c/s))) =
	// = s * (-2*e(-c/s) + 2 + 1/s).
	// The prefactors s are left out, as they cancel out in the end.
	// norm can only become zero, if c is zero, which we sorted out before
	// calling this method.
	ecs := math.Exp(-c / s)
	norm := -2*ecs + 2 + 1/s

	// It would be possible to split the next term and reuse the factors
	// from before, but this can lead to numerical issues with large
	// numbers.
	excs := math.Exp((x - c) / s)
	exs := math.Exp(-x / s)

	// We end up with the primitive function of the normalized P(x).
	return (-exs + excs + x/(c*s)) / norm
}

// integral computes the integral of our liquidity distribution from the lower
// to the upper value.
func (p *BimodalEstimator) integral(capacity, lower, upper float64) float64 {
	if lower < 0 || lower > upper {
		log.Errorf("probability integral limits nonsensical: capacity:"+
			"%v lower: %v upper: %v", capacity, lower, upper)

		return 0.0
	}

	return p.primitive(capacity, upper) - p.primitive(capacity, lower)
}

// probabilityFormula computes the expected probability for a payment of
// amountMsat given prior learnings for a channel of certain capacity.
// successAmountMsat and failAmountMsat stand for the unsettled success and
// failure amounts, respectively. The formula is derived using the formalism
// presented in Pickhardt et al., https://arxiv.org/abs/2103.08576.
func (p *BimodalEstimator) probabilityFormula(capacityMsat, successAmountMsat,
	failAmountMsat, amountMsat lnwire.MilliSatoshi) (float64, error) {

	// Convert to positive-valued floats.
	capacity := float64(capacityMsat)
	successAmount := float64(successAmountMsat)
	failAmount := float64(failAmountMsat)
	amount := float64(amountMsat)

	// In order for this formula to give reasonable results, we need to have
	// an estimate of the capacity of a channel (or edge between nodes).
	if capacity == 0.0 {
		return 0, ErrZeroCapacity
	}

	// We cannot send more than the capacity.
	if amount > capacity {
		return 0.0, nil
	}

	// The next statement is a safety check against an illogical condition.
	// We discard the knowledge for the channel in that case since we have
	// inconsistent data.
	if failAmount <= successAmount {
		log.Warnf("Fail amount (%s) is smaller than or equal to the "+
			"success amount (%s) for capacity (%s)",
			failAmountMsat, successAmountMsat, capacityMsat)

		successAmount = 0
		failAmount = capacity
	}

	// Mission control may have some outdated values with regard to the
	// current channel capacity between a node pair. This can happen in case
	// a large parallel channel was closed or if a channel was downscaled
	// and can lead to success and/or failure amounts to be out of the range
	// [0, capacity]. We assume that the liquidity situation of the channel
	// is similar as before due to flow bias.

	// In case we have a large success we need to correct it to be in the
	// valid range. We set the success amount close to the capacity, because
	// we assume to still be able to send. Any possible failure (that must
	// in this case be larger than the capacity) is corrected as well.
	if successAmount >= capacity {
		log.Debugf("Correcting success amount %s and failure amount "+
			"%s to capacity %s", successAmountMsat,
			failAmount, capacityMsat)

		// We choose the success amount to be one less than the
		// capacity, to both fit success and failure amounts into the
		// capacity range in a consistent manner.
		successAmount = capacity - 1
		failAmount = capacity
	}

	// Having no or only a small success, but a large failure only needs
	// adjustment of the failure amount.
	if failAmount > capacity {
		log.Debugf("Correcting failure amount %s to capacity %s",
			failAmountMsat, capacityMsat)

		failAmount = capacity
	}

	// We cannot send more than the fail amount.
	if amount >= failAmount {
		return 0.0, nil
	}

	// We can send the amount if it is smaller than the success amount.
	if amount <= successAmount {
		return 1.0, nil
	}

	// The success probability for payment amount a is the integral over the
	// prior distribution P(x), the probability to find liquidity between
	// the amount a and channel capacity c (or failAmount a_f):
	// P(X >= a | X < a_f) = Integral_{a}^{a_f} P(x) dx
	prob := p.integral(capacity, amount, failAmount)
	if math.IsNaN(prob) {
		return 0.0, fmt.Errorf("non-normalized probability is NaN, "+
			"capacity: %v, amount: %v, fail amount: %v",
			capacity, amount, failAmount)
	}

	// If we have payment information, we need to adjust the prior
	// distribution P(x) and get the posterior distribution by renormalizing
	// the prior distribution in such a way that the probability mass lies
	// between a_s and a_f.
	reNorm := p.integral(capacity, successAmount, failAmount)
	if math.IsNaN(reNorm) {
		return 0.0, fmt.Errorf("normalization factor is NaN, "+
			"capacity: %v, success amount: %v, fail amount: %v",
			capacity, successAmount, failAmount)
	}

	// The normalization factor can only be zero if the success amount is
	// equal or larger than the fail amount. This should not happen as we
	// have checked this scenario above.
	if reNorm == 0.0 {
		return 0.0, fmt.Errorf("normalization factor is zero, "+
			"capacity: %v, success amount: %v, fail amount: %v",
			capacity, successAmount, failAmount)
	}

	prob /= reNorm

	// Note that for payment amounts smaller than successAmount, we can get
	// a value larger than unity, which we cap here to get a proper
	// probability.
	if prob > 1.0 {
		if amount > successAmount {
			return 0.0, fmt.Errorf("unexpected large probability "+
				"(%v) capacity: %v, amount: %v, success "+
				"amount: %v, fail amount: %v", prob, capacity,
				amount, successAmount, failAmount)
		}

		return 1.0, nil
	} else if prob < 0.0 {
		return 0.0, fmt.Errorf("negative probability "+
			"(%v) capacity: %v, amount: %v, success "+
			"amount: %v, fail amount: %v", prob, capacity,
			amount, successAmount, failAmount)
	}

	return prob, nil
}
