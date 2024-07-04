package chainfee

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestFeeRateTypes checks that converting fee rates between the
// different types that represent fee rates and calculating fees
// work as expected.
func TestFeeRateTypes(t *testing.T) {
	t.Parallel()

	// We'll be calculating the transaction fees for the given measurements
	// using different fee rates and expecting them to match.
	const vsize = 300
	const weight = vsize * 4

	// Test the conversion from sat/kw to sat/kb.
	for feePerKw := SatPerKWeight(250); feePerKw < 10000; feePerKw += 50 {
		feePerKB := feePerKw.FeePerKVByte()
		if feePerKB != SatPerKVByte(feePerKw*4) {
			t.Fatalf("expected %d sat/kb, got %d sat/kb when "+
				"converting from %d sat/kw", feePerKw*4,
				feePerKB, feePerKw)
		}

		// The resulting transaction fee should be the same when using
		// both rates.
		expectedFee := btcutil.Amount(feePerKw * weight / 1000)
		fee1 := feePerKw.FeeForWeight(weight)
		if fee1 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee1)
		}
		fee2 := feePerKB.FeeForVSize(vsize)
		if fee2 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee2)
		}
	}

	// Test the conversion from sat/kb to sat/kw.
	for feePerKB := SatPerKVByte(1000); feePerKB < 40000; feePerKB += 1000 {
		feePerKw := feePerKB.FeePerKWeight()
		if feePerKw != SatPerKWeight(feePerKB/4) {
			t.Fatalf("expected %d sat/kw, got %d sat/kw when "+
				"converting from %d sat/kb", feePerKB/4,
				feePerKw, feePerKB)
		}

		// The resulting transaction fee should be the same when using
		// both rates.
		expectedFee := btcutil.Amount(feePerKB * vsize / 1000)
		fee1 := feePerKB.FeeForVSize(vsize)
		if fee1 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee1)
		}
		fee2 := feePerKw.FeeForWeight(weight)
		if fee2 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee2)
		}
	}
}

// TestStaticFeeEstimator checks that the StaticFeeEstimator returns the
// expected fee rate.
func TestStaticFeeEstimator(t *testing.T) {
	t.Parallel()

	const feePerKw = FeePerKwFloor

	feeEstimator := NewStaticEstimator(feePerKw, 0)
	if err := feeEstimator.Start(); err != nil {
		t.Fatalf("unable to start fee estimator: %v", err)
	}
	defer feeEstimator.Stop()

	feeRate, err := feeEstimator.EstimateFeePerKW(6)
	require.NoError(t, err, "unable to get fee rate")

	if feeRate != feePerKw {
		t.Fatalf("expected fee rate %v, got %v", feePerKw, feeRate)
	}
}

// TestSparseConfFeeSource checks that SparseConfFeeSource generates URLs and
// parses API responses as expected.
func TestSparseConfFeeSource(t *testing.T) {
	t.Parallel()

	// Test that GenQueryURL returns the URL as is.
	url := "test"
	feeSource := SparseConfFeeSource{URL: url}

	// Test parsing a properly formatted JSON API response.
	// First, create the response as a bytes.Reader.
	testFees := map[uint32]uint32{
		1: 12345,
		2: 42,
		3: 54321,
	}
	testMinRelayFee := SatPerKVByte(1000)
	testResp := WebAPIResponse{
		MinRelayFeerate:  testMinRelayFee,
		FeeByBlockTarget: testFees,
	}

	jsonResp, err := json.Marshal(testResp)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader := bytes.NewReader(jsonResp)

	// Finally, ensure the expected map is returned without error.
	resp, err := feeSource.parseResponse(reader)
	require.NoError(t, err, "unable to parse API response")
	require.Equal(t, testResp, resp, "unexpected resp returned")

	// Test parsing an improperly formatted JSON API response.
	badFees := map[string]uint32{"hi": 12345, "hello": 42, "satoshi": 54321}
	badJSON := map[string]map[string]uint32{"fee_by_block_target": badFees}
	jsonResp, err = json.Marshal(badJSON)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader = bytes.NewReader(jsonResp)

	// Finally, ensure the improperly formatted fees error.
	_, err = feeSource.parseResponse(reader)
	require.Error(t, err, "expected error when parsing bad JSON")
}

// TestFeeSourceCompatibility checks that when a fee source doesn't return a
// `min_relay_feerate` field in its response, the floor feerate is used.
//
// NOTE: Field `min_relay_feerate` was added in v0.18.3.
func TestFeeSourceCompatibility(t *testing.T) {
	t.Parallel()

	// Test that GenQueryURL returns the URL as is.
	url := "test"
	feeSource := SparseConfFeeSource{URL: url}

	// Test parsing a properly formatted JSON API response.
	//
	// Create the resp without the `min_relay_feerate` field.
	testFees := map[uint32]uint32{
		1: 12345,
	}
	testResp := struct {
		// FeeByBlockTarget is a map of confirmation targets to sat/kvb
		// fees.
		FeeByBlockTarget map[uint32]uint32 `json:"fee_by_block_target"`
	}{
		FeeByBlockTarget: testFees,
	}

	jsonResp, err := json.Marshal(testResp)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader := bytes.NewReader(jsonResp)

	// Ensure the expected map is returned without error.
	resp, err := feeSource.parseResponse(reader)
	require.NoError(t, err, "unable to parse API response")
	require.Equal(t, testResp.FeeByBlockTarget, resp.FeeByBlockTarget,
		"unexpected resp returned")

	// Expect the floor feerate to be used.
	require.Equal(t, FeePerKwFloor.FeePerKVByte(), resp.MinRelayFeerate)
}

// TestWebAPIFeeEstimator checks that the WebAPIFeeEstimator returns fee rates
// as expected.
func TestWebAPIFeeEstimator(t *testing.T) {
	t.Parallel()

	var (
		minTarget uint32 = 2
		maxTarget uint32 = 6

		// Fee rates are in sat/kb.
		minFeeRate uint32 = 2000 // 500 sat/kw
		maxFeeRate uint32 = 4000 // 1000 sat/kw

		minFeeUpdateTimeout = 5 * time.Minute
		maxFeeUpdateTimeout = 20 * time.Minute
	)

	testCases := []struct {
		name            string
		target          uint32
		expectedFeeRate uint32
		expectedErr     string
	}{
		{
			// When requested target is below minBlockTarget, an
			// error is returned.
			name:            "target_below_min",
			target:          0,
			expectedFeeRate: 0,
			expectedErr:     "too low, minimum",
		},
		{
			// When requested target is larger than the max cached
			// target, the fee rate of the max cached target is
			// returned.
			name:            "target_w_too-low_fee",
			target:          maxTarget + 100,
			expectedFeeRate: minFeeRate,
			expectedErr:     "",
		},
		{
			// When requested target is smaller than the min cached
			// target, the fee rate of the min cached target is
			// returned.
			name:            "API-omitted_target",
			target:          minTarget - 1,
			expectedFeeRate: maxFeeRate,
			expectedErr:     "",
		},
		{
			// When the target is found, return it.
			name:            "valid_target",
			target:          maxTarget,
			expectedFeeRate: minFeeRate,
			expectedErr:     "",
		},
	}

	// Construct mock fee source for the Estimator to pull fees from.
	//
	// This will create a `feeByBlockTarget` map with the following values,
	// - 2: 4000 sat/kb
	// - 6: 2000 sat/kb.
	feeRates := map[uint32]uint32{
		minTarget: maxFeeRate,
		maxTarget: minFeeRate,
	}
	resp := WebAPIResponse{
		FeeByBlockTarget: feeRates,
	}

	// Create a mock fee source and mock its returned map.
	feeSource := &mockFeeSource{}
	feeSource.On("GetFeeInfo").Return(resp, nil)

	estimator, _ := NewWebAPIEstimator(
		feeSource, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)

	// Test that when the estimator is not started, an error is returned.
	feeRate, err := estimator.EstimateFeePerKW(5)
	require.Error(t, err, "expected an error")
	require.Zero(t, feeRate, "expected zero fee rate")

	// Start the estimator.
	require.NoError(t, estimator.Start(), "unable to start fee estimator")

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			est, err := estimator.EstimateFeePerKW(tc.target)

			// Test an error case.
			if tc.expectedErr != "" {
				require.Error(t, err, "expected error")
				require.ErrorContains(t, err, tc.expectedErr)

				return
			}

			// Test an non-error case.
			require.NoErrorf(t, err, "error from target %v",
				tc.target)

			exp := SatPerKVByte(tc.expectedFeeRate).FeePerKWeight()
			require.Equalf(t, exp, est, "target %v failed, fee "+
				"map is %v", tc.target, feeRate)
		})
	}

	// Stop the estimator when test ends.
	require.NoError(t, estimator.Stop(), "unable to stop fee estimator")

	// Assert the mocked fee source is called as expected.
	feeSource.AssertExpectations(t)
}

// TestGetCachedFee checks that the fee caching logic works as expected.
func TestGetCachedFee(t *testing.T) {
	var (
		minTarget uint32 = 2
		maxTarget uint32 = 6

		minFeeRate uint32 = 100
		maxFeeRate uint32 = 1000

		minFeeUpdateTimeout = 5 * time.Minute
		maxFeeUpdateTimeout = 20 * time.Minute
	)

	// Create a dummy estimator without WebAPIFeeSource.
	estimator, _ := NewWebAPIEstimator(
		nil, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)

	// When the cache is empty, an error should be returned.
	cachedFee, err := estimator.getCachedFee(minTarget)
	require.Zero(t, cachedFee)
	require.ErrorIs(t, err, errEmptyCache)

	// Store a fee rate inside the cache. The cache map now looks like,
	// {2: 1000, 6: 100}
	estimator.feeByBlockTarget = map[uint32]uint32{
		minTarget: maxFeeRate,
		maxTarget: minFeeRate,
	}

	testCases := []struct {
		name        string
		confTarget  uint32
		expectedFee uint32
	}{
		{
			// When the target is cached, return it.
			name:        "return cached fee",
			confTarget:  minTarget,
			expectedFee: maxFeeRate,
		},
		{
			// When the target is not cached, return the next
			// lowest target that's cached. In this case,
			// requesting fee rate for target 7 will give the
			// result for target 6.
			name:        "return lowest cached fee",
			confTarget:  maxTarget + 1,
			expectedFee: minFeeRate,
		},
		{
			// When the target is not cached, return the next
			// lowest target that's cached. In this case,
			// requesting fee rate for target 5 will give the
			// result for target 2.
			name:        "return next cached fee",
			confTarget:  maxTarget - 1,
			expectedFee: maxFeeRate,
		},
		{
			// When the target is not cached, and the next lowest
			// target is not cached, return the nearest fee rate.
			// In this case, requesting fee rate for target 1 will
			// give the result for target 2.
			name:        "return highest cached fee",
			confTarget:  minTarget - 1,
			expectedFee: maxFeeRate,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			cachedFee, err := estimator.getCachedFee(tc.confTarget)

			require.NoError(t, err)
			require.Equal(t, tc.expectedFee, cachedFee)
		})
	}
}

func TestRandomFeeUpdateTimeout(t *testing.T) {
	t.Parallel()

	var (
		minFeeUpdateTimeout = 1 * time.Minute
		maxFeeUpdateTimeout = 2 * time.Minute
	)

	estimator, _ := NewWebAPIEstimator(
		nil, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)

	for i := 0; i < 1000; i++ {
		timeout := estimator.randomFeeUpdateTimeout()

		require.GreaterOrEqual(t, timeout, minFeeUpdateTimeout)
		require.LessOrEqual(t, timeout, maxFeeUpdateTimeout)
	}
}

func TestInvalidFeeUpdateTimeout(t *testing.T) {
	t.Parallel()

	var (
		minFeeUpdateTimeout = 2 * time.Minute
		maxFeeUpdateTimeout = 1 * time.Minute
	)

	_, err := NewWebAPIEstimator(
		nil, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)
	require.Error(t, err, "NewWebAPIEstimator should return an error "+
		"when minFeeUpdateTimeout > maxFeeUpdateTimeout")
}
