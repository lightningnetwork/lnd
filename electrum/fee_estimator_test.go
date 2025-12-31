package electrum

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// mockFeeClient is a mock implementation of the fee-related methods needed
// by the FeeEstimator for testing.
type mockFeeClient struct {
	relayFee    float32
	feeEstimate float32
	failRelay   bool
	failFee     bool

	mu sync.RWMutex
}

func newMockFeeClient() *mockFeeClient {
	return &mockFeeClient{
		relayFee:    0.00001, // 1 sat/byte in BTC/kB
		feeEstimate: 0.0001,  // 10 sat/byte in BTC/kB
	}
}

func (m *mockFeeClient) GetRelayFee(ctx context.Context) (float32, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.failRelay {
		return 0, ErrNotConnected
	}

	return m.relayFee, nil
}

func (m *mockFeeClient) EstimateFee(ctx context.Context,
	targetBlocks int) (float32, error) {

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.failFee {
		return -1, nil
	}

	return m.feeEstimate, nil
}

func (m *mockFeeClient) setRelayFee(fee float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.relayFee = fee
}

func (m *mockFeeClient) setFeeEstimate(fee float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.feeEstimate = fee
}

func (m *mockFeeClient) setFailRelay(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failRelay = fail
}

func (m *mockFeeClient) setFailFee(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failFee = fail
}

// testFeeEstimator wraps FeeEstimator with a mock client for testing.
type testFeeEstimator struct {
	*FeeEstimator
	mockClient *mockFeeClient
}

// newTestFeeEstimator creates a FeeEstimator with a mock client for testing.
func newTestFeeEstimator(cfg *FeeEstimatorConfig) *testFeeEstimator {
	mockClient := newMockFeeClient()

	// Create a real client config (won't actually connect).
	clientCfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    30 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
	}
	client := NewClient(clientCfg)

	estimator := NewFeeEstimator(client, cfg)

	return &testFeeEstimator{
		FeeEstimator: estimator,
		mockClient:   mockClient,
	}
}

// TestNewFeeEstimator tests creating a new fee estimator.
func TestNewFeeEstimator(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    30 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        3,
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

// TestBtcPerKBToSatPerKW tests the fee rate conversion function.
func TestBtcPerKBToSatPerKW(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		btcPerKB float64
		minSatKW chainfee.SatPerKWeight
		maxSatKW chainfee.SatPerKWeight
	}{
		{
			name:     "1 sat/vbyte",
			btcPerKB: 0.00001,
			// 1 sat/vbyte = 1000 sat/kvB = 250 sat/kw
			minSatKW: 240,
			maxSatKW: 260,
		},
		{
			name:     "10 sat/vbyte",
			btcPerKB: 0.0001,
			// 10 sat/vbyte = 10000 sat/kvB = 2500 sat/kw
			minSatKW: 2400,
			maxSatKW: 2600,
		},
		{
			name:     "100 sat/vbyte",
			btcPerKB: 0.001,
			// 100 sat/vbyte = 100000 sat/kvB = 25000 sat/kw
			minSatKW: 24000,
			maxSatKW: 26000,
		},
		{
			name:     "zero fee",
			btcPerKB: 0,
			minSatKW: 0,
			maxSatKW: 0,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := btcPerKBToSatPerKW(tc.btcPerKB)
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
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    30 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
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
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    1 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
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
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    30 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
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
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    1 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
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
