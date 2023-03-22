package chainfee

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

type mockSparseConfFeeSource struct {
	url  string
	fees map[uint32]uint32
}

func (e mockSparseConfFeeSource) GenQueryURL() string {
	return e.url
}

func (e mockSparseConfFeeSource) ParseResponse(r io.Reader) (map[uint32]uint32, error) {
	return e.fees, nil
}

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
	queryURL := feeSource.GenQueryURL()
	if queryURL != url {
		t.Fatalf("expected query URL of %v, got %v", url, queryURL)
	}

	// Test parsing a properly formatted JSON API response.
	// First, create the response as a bytes.Reader.
	testFees := map[uint32]uint32{
		1: 12345,
		2: 42,
		3: 54321,
	}
	testJSON := map[string]map[uint32]uint32{"fee_by_block_target": testFees}
	jsonResp, err := json.Marshal(testJSON)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader := bytes.NewReader(jsonResp)

	// Finally, ensure the expected map is returned without error.
	fees, err := feeSource.ParseResponse(reader)
	require.NoError(t, err, "unable to parse API response")
	if !reflect.DeepEqual(fees, testFees) {
		t.Fatalf("expected %v, got %v", testFees, fees)
	}

	// Test parsing an improperly formatted JSON API response.
	badFees := map[string]uint32{"hi": 12345, "hello": 42, "satoshi": 54321}
	badJSON := map[string]map[string]uint32{"fee_by_block_target": badFees}
	jsonResp, err = json.Marshal(badJSON)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader = bytes.NewReader(jsonResp)

	// Finally, ensure the improperly formatted fees error.
	_, err = feeSource.ParseResponse(reader)
	if err == nil {
		t.Fatalf("expected ParseResponse to fail")
	}
}

// TestWebAPIFeeEstimator checks that the WebAPIFeeEstimator returns fee rates
// as expected.
func TestWebAPIFeeEstimator(t *testing.T) {
	t.Parallel()
	feeFloor := uint32(FeePerKwFloor.FeePerKVByte())
	testFeeRate := feeFloor * 100

	testCases := []struct {
		name   string
		target uint32
		apiEst uint32
		est    uint32
		err    string
	}{
		{
			name:   "target_below_min",
			target: 0,
			apiEst: 0,
			est:    0,
			err:    "too low, minimum",
		},
		{
			name:   "target_w_too-low_fee",
			target: 100,
			apiEst: 42,
			est:    feeFloor,
			err:    "",
		},
		{
			name:   "API-omitted_target",
			target: 2,
			apiEst: 0,
			est:    testFeeRate,
			err:    "",
		},
		{
			name:   "valid_target",
			target: 20,
			apiEst: testFeeRate,
			est:    testFeeRate,
			err:    "",
		},
		{
			name:   "valid_target_extrapolated_fee",
			target: 25,
			apiEst: 0,
			est:    testFeeRate,
			err:    "",
		},
	}

	// Construct mock fee source for the Estimator to pull fees from.
	//
	// This will create a `feeByBlockTarget` map with the following values,
	// - 20: testFeeRate
	// - 100: 42, which will be floored to feeFloor.
	testFees := make(map[uint32]uint32)
	for _, tc := range testCases {
		if tc.apiEst != 0 {
			testFees[tc.target] = tc.apiEst
		}
	}

	feeSource := mockSparseConfFeeSource{
		url:  "https://www.github.com",
		fees: testFees,
	}

	estimator := NewWebAPIEstimator(feeSource, false)

	// Test that requesting a fee when no fees have been cached fails.
	feeRate, err := estimator.EstimateFeePerKW(5)
	require.NoErrorf(t, err, "expected no error")
	require.Equalf(t, FeePerKwFloor, feeRate, "expected fee rate floor "+
		"returned when no cached fee rate found")

	if err := estimator.Start(); err != nil {
		t.Fatalf("unable to start fee estimator, got: %v", err)
	}
	defer estimator.Stop()

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			est, err := estimator.EstimateFeePerKW(tc.target)

			// Test an error case.
			if tc.err != "" {
				require.Error(t, err, "expected error")
				require.ErrorContains(t, err, tc.err)

				return
			}

			// Test an non-error case.
			require.NoErrorf(t, err, "error from target %v",
				tc.target)

			exp := SatPerKVByte(tc.est).FeePerKWeight()
			require.Equalf(t, exp, est, "target %v failed, fee "+
				"map is %v", tc.target, feeSource.fees)
		})
	}
}

// TestGetCachedFee checks that the fee caching logic works as expected.
func TestGetCachedFee(t *testing.T) {
	target := uint32(2)
	fee := uint32(100)

	// Create a dummy estimator without WebAPIFeeSource.
	estimator := NewWebAPIEstimator(nil, false)

	// When the cache is empty, an error should be returned.
	cachedFee, err := estimator.getCachedFee(target)
	require.Zero(t, cachedFee)
	require.ErrorIs(t, err, errEmptyCache)

	// Store a fee rate inside the cache.
	estimator.feeByBlockTarget[target] = fee

	testCases := []struct {
		name        string
		confTarget  uint32
		expectedFee uint32
		expectErr   error
	}{
		{
			// When the target is cached, return it.
			name:        "return cached fee",
			confTarget:  target,
			expectedFee: fee,
			expectErr:   nil,
		},
		{
			// When the target is not cached, return the next
			// lowest target that's cached.
			name:        "return next cached fee",
			confTarget:  target + 1,
			expectedFee: fee,
			expectErr:   nil,
		},
		{
			// When the target is not cached, and the next lowest
			// target is not cached, return the nearest fee rate.
			name:        "return highest cached fee",
			confTarget:  target - 1,
			expectedFee: fee,
			expectErr:   nil,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			cachedFee, err := estimator.getCachedFee(tc.confTarget)

			require.Equal(t, tc.expectedFee, cachedFee)
			require.ErrorIs(t, err, tc.expectErr)
		})
	}
}
