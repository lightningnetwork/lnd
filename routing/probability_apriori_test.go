package routing

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const (
	// Define node identifiers.
	node1 = 1
	node2 = 2
	node3 = 3

	// untriedNode is a node id for which we don't record any results in
	// this test. This can be used to assert the probability for untried
	// nodes.
	untriedNode = 255

	// Define test estimator parameters.
	aprioriHopProb     = 0.6
	aprioriWeight      = 0.75
	aprioriPrevSucProb = 0.95

	// testCapacity is used to define a capacity for some channels.
	testCapacity         = btcutil.Amount(100_000)
	testAmount           = lnwire.MilliSatoshi(90_000_000)
	testCapacityFraction = 0.9999

	// capFactor is the capacityFactor for testAmount, testCapacity and
	// testCapacityFraction.
	capFactor = 0.9909715
)

type estimatorTestContext struct {
	t         *testing.T
	estimator *AprioriEstimator

	// results contains a list of last results. Every element in the list
	// corresponds to the last result towards a node. The list index equals
	// the node id. So the first element in the list is the result towards
	// node 0.
	results map[int]TimedPairResult
}

func newEstimatorTestContext(t *testing.T) *estimatorTestContext {
	return &estimatorTestContext{
		t: t,
		estimator: &AprioriEstimator{
			AprioriConfig: AprioriConfig{
				AprioriHopProbability: aprioriHopProb,
				AprioriWeight:         aprioriWeight,
				PenaltyHalfLife:       time.Hour,
				CapacityFraction:      testCapacityFraction,
			},
			prevSuccessProbability: aprioriPrevSucProb,
		},
	}
}

// assertPairProbability asserts that the calculated success probability is
// correct.
func (c *estimatorTestContext) assertPairProbability(now time.Time,
	toNode byte, amt lnwire.MilliSatoshi, capacity btcutil.Amount,
	expectedProb float64) {

	c.t.Helper()

	results := make(NodeResults)
	for i, r := range c.results {
		results[route.Vertex{byte(i)}] = r
	}

	const tolerance = 0.01

	p := c.estimator.PairProbability(
		now, results, route.Vertex{toNode}, amt, capacity,
	)
	diff := p - expectedProb
	if diff > tolerance || diff < -tolerance {
		c.t.Fatalf("expected probability %v for node %v, but got %v",
			expectedProb, toNode, p)
	}
}

// TestProbabilityEstimatorNoResults tests the probability estimation when no
// results are available.
func TestProbabilityEstimatorNoResults(t *testing.T) {
	t.Parallel()

	ctx := newEstimatorTestContext(t)

	// A zero amount does not trigger capacity rescaling.
	ctx.assertPairProbability(
		testTime, 0, 0, testCapacity, aprioriHopProb,
	)

	// We expect a reduced probability when a higher amount is used.
	expected := aprioriHopProb * capFactor
	ctx.assertPairProbability(
		testTime, 0, testAmount, testCapacity, expected,
	)
}

// TestProbabilityEstimatorOneSuccess tests the probability estimation for nodes
// that have a single success result.
func TestProbabilityEstimatorOneSuccess(t *testing.T) {
	t.Parallel()

	ctx := newEstimatorTestContext(t)

	ctx.results = map[int]TimedPairResult{
		node1: {
			SuccessAmt: testAmount,
		},
	}

	// Because of the previous success, this channel keep reporting a high
	// probability.
	ctx.assertPairProbability(
		testTime, node1, 100, testCapacity, aprioriPrevSucProb,
	)

	// The apriori success probability indicates that in the past we were
	// able to send the full amount. We don't want to reduce this
	// probability with the capacity factor, which is tested here.
	ctx.assertPairProbability(
		testTime, node1, testAmount, testCapacity, aprioriPrevSucProb,
	)

	// Untried channels are also influenced by the success. With a
	// aprioriWeight of 0.75, the a priori probability is assigned weight 3.
	expectedP := (3*aprioriHopProb + 1*aprioriPrevSucProb) / 4
	ctx.assertPairProbability(
		testTime, untriedNode, 100, testCapacity, expectedP,
	)

	// Check that the correct probability is computed for larger amounts.
	apriori := aprioriHopProb * capFactor

	expectedP = (3*apriori + 1*aprioriPrevSucProb) / 4
	ctx.assertPairProbability(
		testTime, untriedNode, testAmount, testCapacity, expectedP,
	)
}

// TestProbabilityEstimatorOneFailure tests the probability estimation for nodes
// that have a single failure.
func TestProbabilityEstimatorOneFailure(t *testing.T) {
	t.Parallel()

	ctx := newEstimatorTestContext(t)

	ctx.results = map[int]TimedPairResult{
		node1: {
			FailTime: testTime.Add(-time.Hour),
			FailAmt:  lnwire.MilliSatoshi(50),
		},
	}

	// For an untried node, we expected the node probability. The weight for
	// the failure after one hour is 0.5. This makes the node probability
	// 0.51:
	expectedNodeProb := (3*aprioriHopProb + 0.5*0) / 3.5
	ctx.assertPairProbability(
		testTime, untriedNode, 100, testCapacity, expectedNodeProb,
	)

	// The pair probability decays back to the node probability. With the
	// weight at 0.5, we expected a pair probability of 0.5 * 0.51 = 0.25.
	ctx.assertPairProbability(
		testTime, node1, 100, testCapacity, expectedNodeProb/2,
	)
}

// TestProbabilityEstimatorMix tests the probability estimation for nodes for
// which a mix of successes and failures is recorded.
func TestProbabilityEstimatorMix(t *testing.T) {
	t.Parallel()

	ctx := newEstimatorTestContext(t)

	ctx.results = map[int]TimedPairResult{
		node1: {
			SuccessAmt: lnwire.MilliSatoshi(1000),
		},
		node2: {
			FailTime: testTime.Add(-2 * time.Hour),
			FailAmt:  lnwire.MilliSatoshi(50),
		},
		node3: {
			FailTime: testTime.Add(-3 * time.Hour),
			FailAmt:  lnwire.MilliSatoshi(50),
		},
	}

	// We expect the probability for a previously successful channel to
	// remain high.
	ctx.assertPairProbability(
		testTime, node1, 100, testCapacity, prevSuccessProbability,
	)

	// For an untried node, we expected the node probability to be returned.
	// This is a weighted average of the results above and the a priori
	// probability: 0.62.
	expectedNodeProb := (3*aprioriHopProb + 1*prevSuccessProbability) /
		(3 + 1 + 0.25 + 0.125)

	ctx.assertPairProbability(
		testTime, untriedNode, 100, testCapacity, expectedNodeProb,
	)

	// For the previously failed connection with node 1, we expect 0.75 *
	// the node probability = 0.47.
	ctx.assertPairProbability(
		testTime, node2, 100, testCapacity, expectedNodeProb*0.75,
	)
}

// TestCapacityCutoff tests the mathematical expression and limits for the
// capacity factor.
func TestCapacityCutoff(t *testing.T) {
	t.Parallel()

	capacitySat := 1_000_000
	capacityMSat := capacitySat * 1000

	tests := []struct {
		name             string
		capacityFraction float64
		amountMsat       int
		expectedFactor   float64
	}{
		// Minimal CapacityFraction of 0.75.
		{
			name:             "zero amount",
			capacityFraction: 0.75,
			expectedFactor:   1,
		},
		{
			name:             "low amount",
			capacityFraction: 0.75,
			amountMsat:       capacityMSat / 10,
			expectedFactor:   1,
		},
		{
			name:             "half amount",
			capacityFraction: 0.75,
			amountMsat:       capacityMSat / 2,
			expectedFactor:   1,
		},
		{
			name:             "cutoff amount",
			capacityFraction: 0.75,
			amountMsat: int(
				0.75 * float64(capacityMSat),
			),
			expectedFactor: 0.75,
		},
		{
			name:             "high amount",
			capacityFraction: 0.75,
			amountMsat:       capacityMSat * 80 / 100,
			expectedFactor:   0.560,
		},
		{
			// Even when we spend the full capacity, we still want
			// to have some residual probability to not throw away
			// routes due to a min probability requirement of the
			// whole path.
			name:             "full amount",
			capacityFraction: 0.75,
			amountMsat:       capacityMSat,
			expectedFactor:   0.5,
		},
		{
			name:             "more than capacity",
			capacityFraction: 0.75,
			amountMsat:       capacityMSat + 1,
			expectedFactor:   0.0,
		},
		// Default CapacityFactor of 0.9999.
		{
			name:             "zero amount",
			capacityFraction: 0.9999,
			amountMsat:       0,
			expectedFactor:   1.00,
		},
		{
			name:             "90% of the channel capacity",
			capacityFraction: 0.9999,
			amountMsat:       capacityMSat * 90 / 100,
			expectedFactor:   0.990,
		},
		{
			// We won't saturate at 0.5 as in the other case but at
			// a higher value of 0.75 due to the smearing, this
			// translates to a penalty increase of a factor of 1.33.
			name:             "full amount",
			capacityFraction: 0.9999,
			amountMsat:       capacityMSat,
			expectedFactor:   0.75,
		},
		// Inactive capacity factor.
		{
			name:             "inactive capacity factor",
			capacityFraction: 1.0,
			amountMsat:       capacityMSat,
			expectedFactor:   1.00,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := capacityFactor(
				lnwire.MilliSatoshi(test.amountMsat),
				btcutil.Amount(capacitySat),
				test.capacityFraction,
			)
			require.InDelta(t, test.expectedFactor, got, 0.001)
		})
	}
}
