package esplora

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestNewFeeEstimator tests creating a new fee estimator.
func TestNewFeeEstimator(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	estimator := NewFeeEstimator(client, nil)
	require.NotNil(t, estimator)
	require.NotNil(t, estimator.cfg)
	require.NotNil(t, estimator.feeCache)
}

// TestFeeEstimatorDefaultConfig tests that default config values are applied.
func TestFeeEstimatorDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultFeeEstimatorConfig()

	require.NotNil(t, cfg)
	require.Greater(t, cfg.FallbackFeePerKW, chainfee.SatPerKWeight(0))
	require.Greater(t, cfg.MinFeePerKW, chainfee.SatPerKWeight(0))
	require.Greater(t, cfg.FeeUpdateInterval, time.Duration(0))
}

// TestSatPerVBToSatPerKW tests the fee rate conversion function.
func TestSatPerVBToSatPerKW(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		satPerVB float64
		minSatKW chainfee.SatPerKWeight
		maxSatKW chainfee.SatPerKWeight
	}{
		{
			name:     "1 sat/vbyte",
			satPerVB: 1.0,
			// 1 sat/vB * 250 = 250 sat/kw
			minSatKW: 245,
			maxSatKW: 255,
		},
		{
			name:     "10 sat/vbyte",
			satPerVB: 10.0,
			// 10 sat/vB * 250 = 2500 sat/kw
			minSatKW: 2450,
			maxSatKW: 2550,
		},
		{
			name:     "100 sat/vbyte",
			satPerVB: 100.0,
			// 100 sat/vB * 250 = 25000 sat/kw
			minSatKW: 24500,
			maxSatKW: 25500,
		},
		{
			name:     "zero fee",
			satPerVB: 0,
			minSatKW: 0,
			maxSatKW: 0,
		},
		{
			name:     "fractional fee",
			satPerVB: 1.5,
			// 1.5 sat/vB * 250 = 375 sat/kw
			minSatKW: 370,
			maxSatKW: 380,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := satPerVBToSatPerKW(tc.satPerVB)
			require.GreaterOrEqual(t, result, tc.minSatKW)
			require.LessOrEqual(t, result, tc.maxSatKW)
		})
	}
}

// TestFeeEstimatorRelayFeePerKW tests that RelayFeePerKW returns a valid
// value.
func TestFeeEstimatorRelayFeePerKW(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	estimator := NewFeeEstimator(client, nil)

	relayFee := estimator.RelayFeePerKW()
	require.Greater(t, relayFee, chainfee.SatPerKWeight(0))
}

// TestFeeEstimatorEstimateFeePerKWFallback tests that the estimator returns
// the fallback fee when the server is not available.
func TestFeeEstimatorEstimateFeePerKWFallback(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 1 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	feeCfg := &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: 5 * time.Minute,
	}

	estimator := NewFeeEstimator(client, feeCfg)

	// Without starting (and thus without a server), EstimateFeePerKW
	// should return the fallback fee.
	feeRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, feeCfg.FallbackFeePerKW, feeRate)
}

// TestFeeEstimatorCaching tests that fee estimates are properly cached.
func TestFeeEstimatorCaching(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	feeCfg := &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: 5 * time.Minute,
	}

	estimator := NewFeeEstimator(client, feeCfg)

	// Manually add a cached fee.
	estimator.feeCacheMtx.Lock()
	estimator.feeCache[6] = chainfee.SatPerKWeight(5000)
	estimator.feeCacheMtx.Unlock()

	// Should return the cached value, not the fallback.
	feeRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(5000), feeRate)
}

// TestFeeEstimatorInterface verifies that FeeEstimator implements the
// chainfee.Estimator interface.
func TestFeeEstimatorInterface(t *testing.T) {
	t.Parallel()

	// This is a compile-time check that FeeEstimator implements the
	// chainfee.Estimator interface.
	var _ chainfee.Estimator = (*FeeEstimator)(nil)
}

// TestFeeEstimatorStartStop tests starting and stopping the fee estimator.
func TestFeeEstimatorStartStop(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 1 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	feeCfg := &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: 5 * time.Minute,
	}

	estimator := NewFeeEstimator(client, feeCfg)

	// Start should succeed even without a connected server.
	err := estimator.Start()
	require.NoError(t, err)

	// Starting again should be a no-op.
	err = estimator.Start()
	require.NoError(t, err)

	// Stop should succeed.
	err = estimator.Stop()
	require.NoError(t, err)

	// Stopping again should be a no-op.
	err = estimator.Stop()
	require.NoError(t, err)
}

// TestFeeEstimatorClosestTarget tests that the estimator finds the closest
// cached target when the exact target is not available.
func TestFeeEstimatorClosestTarget(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	feeCfg := &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: 5 * time.Minute,
	}

	estimator := NewFeeEstimator(client, feeCfg)

	// Manually add some cached fees at different targets.
	estimator.feeCacheMtx.Lock()
	estimator.feeCache[1] = chainfee.SatPerKWeight(10000)
	estimator.feeCache[3] = chainfee.SatPerKWeight(5000)
	estimator.feeCache[6] = chainfee.SatPerKWeight(2500)
	estimator.feeCache[12] = chainfee.SatPerKWeight(1000)
	estimator.feeCacheMtx.Unlock()

	// Request target 4, should get closest lower target (3).
	feeRate, err := estimator.EstimateFeePerKW(4)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(5000), feeRate)

	// Request target 10, should get closest lower target (6).
	feeRate, err = estimator.EstimateFeePerKW(10)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(2500), feeRate)

	// Request target 2, should get closest lower target (1).
	feeRate, err = estimator.EstimateFeePerKW(2)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(10000), feeRate)
}

// TestFeeEstimatorMinTargetFallback tests that when no lower target exists,
// we fall back to the minimum cached target.
func TestFeeEstimatorMinTargetFallback(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	feeCfg := &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: 5 * time.Minute,
	}

	estimator := NewFeeEstimator(client, feeCfg)

	estimator.feeCacheMtx.Lock()
	estimator.feeCache[3] = chainfee.SatPerKWeight(5000)
	estimator.feeCache[6] = chainfee.SatPerKWeight(2500)
	estimator.feeCacheMtx.Unlock()

	// Request target 1, should get minimum cached target (3).
	feeRate, err := estimator.EstimateFeePerKW(1)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(5000), feeRate)
}

// TestFeeEstimatorClampToRelayFloor tests that fees are clamped to relay fee.
func TestFeeEstimatorClampToRelayFloor(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	feeCfg := &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: 5 * time.Minute,
	}

	estimator := NewFeeEstimator(client, feeCfg)
	estimator.relayFeePerKW = chainfee.SatPerKWeight(6000)

	estimator.feeCacheMtx.Lock()
	estimator.feeCache[6] = chainfee.SatPerKWeight(1000)
	estimator.feeCacheMtx.Unlock()

	feeRate, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, chainfee.SatPerKWeight(6000), feeRate)
}
