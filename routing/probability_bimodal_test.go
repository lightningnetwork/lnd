package routing

import (
	"math"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const (
	smallAmount = lnwire.MilliSatoshi(400_000_000)
	largeAmount = lnwire.MilliSatoshi(5_000_000_000)
	capacity    = lnwire.MilliSatoshi(10_000_000_000)
	scale       = lnwire.MilliSatoshi(400_000_000)

	// defaultTolerance is the default absolute tolerance for comparing
	// probability calculations to expected values.
	defaultTolerance = 0.001
)

// TestSuccessProbability tests that we get correct probability estimates for
// the direct channel probability.
func TestSuccessProbability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		expectedProbability float64
		successAmount       lnwire.MilliSatoshi
		failAmount          lnwire.MilliSatoshi
		amount              lnwire.MilliSatoshi
		capacity            lnwire.MilliSatoshi
	}{
		// We can't send more than the capacity.
		{
			name:                "no info, larger than capacity",
			capacity:            capacity,
			successAmount:       0,
			failAmount:          capacity,
			amount:              capacity + 1,
			expectedProbability: 0.0,
		},
		// With the current model we don't prefer any channels if the
		// send amount is large compared to the scale but small compared
		// to the capacity.
		{
			name:                "no info, large amount",
			capacity:            capacity,
			successAmount:       0,
			failAmount:          capacity,
			amount:              largeAmount,
			expectedProbability: 0.5,
		},
		// We always expect to be able to "send" an amount of 0.
		{
			name:                "no info, zero amount",
			capacity:            capacity,
			successAmount:       0,
			failAmount:          capacity,
			amount:              0,
			expectedProbability: 1.0,
		},
		// We can't send the whole capacity.
		{
			name:                "no info, full capacity",
			capacity:            capacity,
			successAmount:       0,
			failAmount:          capacity,
			amount:              capacity,
			expectedProbability: 0.0,
		},
		// Sending a small amount will have a higher probability to go
		// through than a large amount.
		{
			name:                "no info, small amount",
			capacity:            capacity,
			successAmount:       0,
			failAmount:          capacity,
			amount:              smallAmount,
			expectedProbability: 0.684,
		},
		// If we had an unsettled success, we are sure we can send a
		// lower amount.
		{
			name:                "previous success, lower amount",
			capacity:            capacity,
			successAmount:       largeAmount,
			failAmount:          capacity,
			amount:              smallAmount,
			expectedProbability: 1.0,
		},
		// If we had an unsettled success, we are sure we can send the
		// same amount.
		{
			name:                "previous success, success amount",
			capacity:            capacity,
			successAmount:       largeAmount,
			failAmount:          capacity,
			amount:              largeAmount,
			expectedProbability: 1.0,
		},
		// If we had an unsettled success with a small amount, we know
		// with increased probability that we can send a comparable
		// higher amount.
		{
			name:                "previous success, larger amount",
			capacity:            capacity,
			successAmount:       smallAmount / 2,
			failAmount:          capacity,
			amount:              smallAmount,
			expectedProbability: 0.851,
		},
		// If we had a large unsettled success before, we know we can
		// send even larger payments with high probability.
		{
			name: "previous large success, larger " +
				"amount",
			capacity:            capacity,
			successAmount:       largeAmount / 2,
			failAmount:          capacity,
			amount:              largeAmount,
			expectedProbability: 0.998,
		},
		// If we had a failure before, we can't send with the fail
		// amount.
		{
			name:                "previous failure, fail amount",
			capacity:            capacity,
			failAmount:          largeAmount,
			amount:              largeAmount,
			expectedProbability: 0.0,
		},
		// We can't send a higher amount than the fail amount either.
		{
			name: "previous failure, larger fail " +
				"amount",
			capacity:            capacity,
			failAmount:          largeAmount,
			amount:              largeAmount + smallAmount,
			expectedProbability: 0.0,
		},
		// We expect a diminished non-zero probability if we try to send
		// an amount that's lower than the last fail amount.
		{
			name: "previous failure, lower than fail " +
				"amount",
			capacity:            capacity,
			failAmount:          largeAmount,
			amount:              smallAmount,
			expectedProbability: 0.368,
		},
		// From here on we deal with mixed previous successes and
		// failures.
		// We expect to be always able to send a tiny amount.
		{
			name:                "previous f/s, very small amount",
			capacity:            capacity,
			failAmount:          largeAmount,
			successAmount:       smallAmount,
			amount:              0,
			expectedProbability: 1.0,
		},
		// We expect to be able to send up to the previous success
		// amount will full certainty.
		{
			name:                "previous f/s, success amount",
			capacity:            capacity,
			failAmount:          largeAmount,
			successAmount:       smallAmount,
			amount:              smallAmount,
			expectedProbability: 1.0,
		},
		// This tests a random value between small amount and large
		// amount.
		{
			name:                "previous f/s, between f/s",
			capacity:            capacity,
			failAmount:          largeAmount,
			successAmount:       smallAmount,
			amount:              smallAmount + largeAmount/10,
			expectedProbability: 0.287,
		},
		// We still can't send the fail amount.
		{
			name:                "previous f/s, fail amount",
			capacity:            capacity,
			failAmount:          largeAmount,
			successAmount:       smallAmount,
			amount:              largeAmount,
			expectedProbability: 0.0,
		},
		// Same success and failure amounts (illogical), which gets
		// reset to no knowledge.
		{
			name:                "previous f/s, same",
			capacity:            capacity,
			failAmount:          largeAmount,
			successAmount:       largeAmount,
			amount:              largeAmount,
			expectedProbability: 0.5,
		},
		// Higher success than failure amount (illogical), which gets
		// reset to no knowledge.
		{
			name:                "previous f/s, illogical",
			capacity:            capacity,
			failAmount:          smallAmount,
			successAmount:       largeAmount,
			amount:              largeAmount,
			expectedProbability: 0.5,
		},
		// Larger success and larger failure than the old capacity are
		// rescaled to still give a very high success rate.
		{
			name:                "smaller cap, large success/fail",
			capacity:            capacity,
			failAmount:          2*capacity + 1,
			successAmount:       2 * capacity,
			amount:              largeAmount,
			expectedProbability: 1.0,
		},
		// A lower success amount is not rescaled.
		{
			name:          "smaller cap, large fail",
			capacity:      capacity,
			successAmount: smallAmount / 2,
			failAmount:    2 * capacity,
			amount:        smallAmount,
			// See "previous success, larger amount".
			expectedProbability: 0.851,
		},
	}

	estimator := BimodalEstimator{
		BimodalConfig: BimodalConfig{BimodalScaleMsat: scale},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			p, err := estimator.probabilityFormula(
				test.capacity, test.successAmount,
				test.failAmount, test.amount,
			)
			require.InDelta(t, test.expectedProbability, p,
				defaultTolerance)
			require.NoError(t, err)
		})
	}

	// We expect an error when the capacity is zero.
	t.Run("zero capacity", func(t *testing.T) {
		t.Parallel()

		_, err := estimator.probabilityFormula(
			0, 0, 0, 0,
		)
		require.ErrorIs(t, err, ErrZeroCapacity)
	})
}

// TestSmallScale tests that the probability formula works with small scale
// values.
func TestSmallScale(t *testing.T) {
	var (
		// We use the smallest possible scale value together with a
		// large capacity. This is an extreme form of a bimodal
		// distribution.
		scale    lnwire.MilliSatoshi = 1
		capacity lnwire.MilliSatoshi = 7e+09

		// Success and failure amounts are chosen such that the expected
		// balance must be somewhere in the middle of the channel, a
		// value not expected when dealing with a bimodal distribution.
		// In this case, the bimodal model fails to give good forecasts
		// due to the numerics of the exponential functions, which get
		// evaluated to exact zero floats.
		successAmount lnwire.MilliSatoshi = 1.0e+09
		failAmount    lnwire.MilliSatoshi = 4.0e+09
	)

	estimator := BimodalEstimator{
		BimodalConfig: BimodalConfig{BimodalScaleMsat: scale},
	}

	// An amount that's close to the success amount should have a very high
	// probability.
	amtCloseSuccess := successAmount + 1
	p, err := estimator.probabilityFormula(
		capacity, successAmount, failAmount, amtCloseSuccess,
	)
	require.NoError(t, err)
	require.InDelta(t, 1.0, p, defaultTolerance)

	// An amount that's close to the fail amount should have a very low
	// probability.
	amtCloseFail := failAmount - 1
	p, err = estimator.probabilityFormula(
		capacity, successAmount, failAmount, amtCloseFail,
	)
	require.NoError(t, err)
	require.InDelta(t, 0.0, p, defaultTolerance)

	// In the region where the bimodal model doesn't give good forecasts, we
	// fall back to a uniform model, which interpolates probabilities
	// linearly.
	amtLinear := successAmount + (failAmount-successAmount)*1/4
	p, err = estimator.probabilityFormula(
		capacity, successAmount, failAmount, amtLinear,
	)
	require.NoError(t, err)
	require.InDelta(t, 0.75, p, defaultTolerance)
}

// TestIntegral tests certain limits of the probability distribution integral.
func TestIntegral(t *testing.T) {
	t.Parallel()

	defaultScale := lnwire.NewMSatFromSatoshis(300_000)

	tests := []struct {
		name     string
		capacity float64
		lower    float64
		upper    float64
		scale    lnwire.MilliSatoshi
		expected float64
	}{
		{
			name:     "all zero",
			expected: math.NaN(),
			scale:    defaultScale,
		},
		{
			name:     "all same",
			capacity: 1,
			lower:    1,
			upper:    1,
			scale:    defaultScale,
		},
		{
			name:     "large numbers, low lower",
			capacity: 21e17,
			lower:    0,
			upper:    21e17,
			expected: 1,
			scale:    defaultScale,
		},
		{
			name:     "large numbers, high lower",
			capacity: 21e17,
			lower:    21e17,
			upper:    21e17,
			scale:    defaultScale,
		},
		{
			name:     "same scale and capacity",
			capacity: 21e17,
			lower:    21e17,
			upper:    21e17,
			scale:    21e17,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			estimator := BimodalEstimator{
				BimodalConfig: BimodalConfig{
					BimodalScaleMsat: test.scale,
				},
			}

			p := estimator.integral(
				test.capacity, test.lower, test.upper,
			)
			require.InDelta(t, test.expected, p, 0.001)
		})
	}
}

// TestCanSend tests that the success amount drops to zero over time.
func TestCanSend(t *testing.T) {
	t.Parallel()

	successAmount := lnwire.MilliSatoshi(1_000_000)
	successTime := time.Unix(1_000, 0)
	now := time.Unix(2_000, 0)
	decayTime := time.Duration(1_000) * time.Second
	infinity := time.Unix(10_000_000_000, 0)

	// Test an immediate retry.
	require.Equal(t, successAmount, canSend(
		successAmount, successTime, successTime, decayTime,
	))

	// Test that after the decay time, the successAmount is 1/e of its
	// value.
	decayAmount := lnwire.MilliSatoshi(float64(successAmount) / math.E)
	require.Equal(t, decayAmount, canSend(
		successAmount, now, successTime, decayTime,
	))

	// After a long time, we want the amount to approach 0.
	require.Equal(t, lnwire.MilliSatoshi(0), canSend(
		successAmount, infinity, successTime, decayTime,
	))
}

// TestCannotSend tests that the fail amount approaches the capacity over time.
func TestCannotSend(t *testing.T) {
	t.Parallel()

	failAmount := lnwire.MilliSatoshi(1_000_000)
	failTime := time.Unix(1_000, 0)
	now := time.Unix(2_000, 0)
	decayTime := time.Duration(1_000) * time.Second
	infinity := time.Unix(10_000_000_000, 0)
	capacity := lnwire.MilliSatoshi(3_000_000)

	// Test immediate retry.
	require.EqualValues(t, failAmount, cannotSend(
		failAmount, capacity, failTime, failTime, decayTime,
	))

	// After the decay time we want to be between the fail amount and
	// the capacity.
	summand := lnwire.MilliSatoshi(float64(capacity-failAmount) / math.E)
	expected := capacity - summand
	require.Equal(t, expected, cannotSend(
		failAmount, capacity, now, failTime, decayTime,
	))

	// After a long time, we want the amount to approach the capacity.
	require.Equal(t, capacity, cannotSend(
		failAmount, capacity, infinity, failTime, decayTime,
	))
}

// TestComputeProbability tests the inclusion of previous forwarding results of
// other channels of the node into the total probability.
func TestComputeProbability(t *testing.T) {
	t.Parallel()

	nodeWeight := 1 / 5.
	toNode := route.Vertex{10}
	tolerance := 0.01
	now := time.Unix(0, 0)
	decayTime := time.Duration(1) * time.Hour * 24

	// makeNodeResults prepares forwarding data for the other channels of
	// the node.
	makeNodeResults := func(successes []bool, resultsTime time.Time,
		directResultTime time.Time) NodeResults {

		results := make(NodeResults, len(successes))

		for i, s := range successes {
			vertex := route.Vertex{byte(i)}

			results[vertex] = TimedPairResult{
				FailTime: resultsTime, FailAmt: 1,
			}
			if s {
				results[vertex] = TimedPairResult{
					SuccessTime: resultsTime, SuccessAmt: 1,
				}
			}
		}

		// Include a direct result.
		results[toNode] = TimedPairResult{
			SuccessTime: directResultTime, SuccessAmt: 1,
		}

		return results
	}

	tests := []struct {
		name                string
		directProbability   float64
		recentDirectResult  bool
		otherResults        []bool
		expectedProbability float64
		resultsTimeAgo      time.Duration
	}{
		// If no other information is available, use the direct
		// probability.
		{
			name:                "unknown, only direct",
			directProbability:   0.5,
			expectedProbability: 0.5,
		},
		// If there was a single success, expect increased success
		// probability.
		{
			name:                "unknown, single success",
			directProbability:   0.5,
			otherResults:        []bool{true},
			expectedProbability: 0.583,
		},
		// If there were many successes, expect even higher success
		// probability.
		{
			name:              "unknown, many successes",
			directProbability: 0.5,
			otherResults: []bool{
				true, true, true, true, true,
			},
			expectedProbability: 0.75,
		},
		// If there was a single failure, we expect a slightly decreased
		// probability.
		{
			name:                "unknown, single failure",
			directProbability:   0.5,
			otherResults:        []bool{false},
			expectedProbability: 0.416,
		},
		// If there were many failures, we expect a strongly decreased
		// probability.
		{
			name:              "unknown, many failures",
			directProbability: 0.5,
			otherResults: []bool{
				false, false, false, false, false,
			},
			expectedProbability: 0.25,
		},
		// A success and a failure neutralize themselves.
		{
			name:                "unknown, mixed even",
			directProbability:   0.5,
			otherResults:        []bool{true, false},
			expectedProbability: 0.5,
		},
		// A mixed result history leads to increase/decrease of the most
		// experienced successes/failures.
		{
			name:              "unknown, mixed uneven",
			directProbability: 0.5,
			otherResults: []bool{
				true, true, false, false, false,
			},
			expectedProbability: 0.45,
		},
		// Many successes don't elevate the probability above 1.
		{
			name:              "success, successes",
			directProbability: 1.0,
			otherResults: []bool{
				true, true, true, true, true,
			},
			expectedProbability: 1.0,
		},
		// Five failures on a very certain channel will lower its
		// success probability to the unknown probability.
		{
			name:              "success, failures",
			directProbability: 1.0,
			otherResults: []bool{
				false, false, false, false, false,
			},
			expectedProbability: 0.5,
		},
		// If we are sure that the channel can send, a single failure
		// will not decrease the outcome significantly.
		{
			name:                "success, single failure",
			directProbability:   1.0,
			otherResults:        []bool{false},
			expectedProbability: 0.8333,
		},
		{
			name:              "success, many failures",
			directProbability: 1.0,
			otherResults: []bool{
				false, false, false, false, false, false, false,
			},
			expectedProbability: 0.416,
		},
		// Failures won't decrease the probability below zero.
		{
			name:                "fail, failures",
			directProbability:   0.0,
			otherResults:        []bool{false, false, false},
			expectedProbability: 0.0,
		},
		{
			name:              "fail, successes",
			directProbability: 0.0,
			otherResults: []bool{
				true, true, true, true, true,
			},
			expectedProbability: 0.5,
		},
		// We test forgetting information with the time decay.
		// A past success won't alter the certain success probability.
		{
			name: "success, single success, decay " +
				"time",
			directProbability:   1.0,
			otherResults:        []bool{true},
			resultsTimeAgo:      decayTime,
			expectedProbability: 1.00,
		},
		// A failure that was experienced some time ago won't influence
		// as much as a recent one.
		{
			name:                "success, single fail, decay time",
			directProbability:   1.0,
			otherResults:        []bool{false},
			resultsTimeAgo:      decayTime,
			expectedProbability: 0.9314,
		},
		// Information from a long time ago doesn't have any effect.
		{
			name:                "success, single fail, long ago",
			directProbability:   1.0,
			otherResults:        []bool{false},
			resultsTimeAgo:      10 * decayTime,
			expectedProbability: 1.0,
		},
		{
			name:              "fail, successes decay time",
			directProbability: 0.0,
			otherResults: []bool{
				true, true, true, true, true,
			},
			resultsTimeAgo:      decayTime,
			expectedProbability: 0.269,
		},
		// Very recent info approaches the case with no time decay.
		{
			name:              "unknown, successes close",
			directProbability: 0.5,
			otherResults: []bool{
				true, true, true, true, true,
			},
			resultsTimeAgo:      decayTime / 10,
			expectedProbability: 0.741,
		},
		// If we have recent info on the direct probability, we don't
		// include node-wide info. Here we check that a recent failure
		// is not pinned to a high probability by many successes on the
		// node.
		{
			name:               "recent direct result",
			directProbability:  0.1,
			recentDirectResult: true,
			otherResults: []bool{
				true, true, true, true, true,
			},
			expectedProbability: 0.1,
		},
	}

	estimator := BimodalEstimator{
		BimodalConfig: BimodalConfig{
			BimodalScaleMsat: scale, BimodalNodeWeight: nodeWeight,
			BimodalDecayTime: decayTime,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var directResultTime time.Time
			if test.recentDirectResult {
				directResultTime = now.Add(-decayTime / 2)
			} else {
				directResultTime = now.Add(-2 * decayTime)
			}

			resultsTime := now.Add(-test.resultsTimeAgo)

			results := makeNodeResults(
				test.otherResults, resultsTime,
				directResultTime,
			)

			p := estimator.calculateProbability(
				test.directProbability, now, results, toNode,
			)

			require.InDelta(t, test.expectedProbability, p,
				tolerance)
		})
	}
}

// TestLocalPairProbability tests that we reduce probability for failed direct
// neighbors.
func TestLocalPairProbability(t *testing.T) {
	t.Parallel()

	decayTime := time.Hour
	now := time.Unix(1000000000, 0)
	toNode := route.Vertex{1}

	createFailedResult := func(timeAgo time.Duration) NodeResults {
		return NodeResults{
			toNode: TimedPairResult{
				FailTime: now.Add(-timeAgo),
			},
		}
	}

	tests := []struct {
		name                string
		expectedProbability float64
		results             NodeResults
	}{
		{
			name:                "no results",
			expectedProbability: 1.0,
		},
		{
			name:                "recent failure",
			results:             createFailedResult(0),
			expectedProbability: 0.0,
		},
		{
			name:                "after decay time",
			results:             createFailedResult(decayTime),
			expectedProbability: 1 - 1/math.E,
		},
		{
			name:                "long ago",
			results:             createFailedResult(10 * decayTime),
			expectedProbability: 1.0,
		},
	}

	estimator := BimodalEstimator{
		BimodalConfig: BimodalConfig{BimodalDecayTime: decayTime},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := estimator.LocalPairProbability(
				now, test.results, toNode,
			)
			require.InDelta(t, test.expectedProbability, p, 0.001)
		})
	}
}

// FuzzProbability checks that we don't encounter errors related to NaNs.
func FuzzProbability(f *testing.F) {
	estimator := BimodalEstimator{
		BimodalConfig: BimodalConfig{BimodalScaleMsat: 400_000},
	}

	// Predefined seed reported in
	// https://github.com/lightningnetwork/lnd/issues/9085. This test found
	// a case where we could not compute a normalization factor because we
	// learned that the balance lies somewhere in the middle of the channel,
	// a surprising result for the bimodal model, which predicts two
	// distinct modes at the edges and therefore has numerical issues in the
	// middle. Additionally, the scale is small with respect to the values
	// used here.
	f.Add(
		uint64(1_000_000_000),
		uint64(300_000_000),
		uint64(400_000_000),
		uint64(300_000_000),
	)

	f.Fuzz(func(t *testing.T, capacity, successAmt, failAmt, amt uint64) {
		if capacity == 0 {
			return
		}

		_, err := estimator.probabilityFormula(
			lnwire.MilliSatoshi(capacity),
			lnwire.MilliSatoshi(successAmt),
			lnwire.MilliSatoshi(failAmt), lnwire.MilliSatoshi(amt),
		)

		require.NoError(t, err, "c: %v s: %v f: %v a: %v", capacity,
			successAmt, failAmt, amt)
	})
}
