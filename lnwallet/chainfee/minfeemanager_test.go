package chainfee

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type mockChainBackend struct {
	minFee    SatPerKWeight
	callCount int
}

func (m *mockChainBackend) fetchFee() (SatPerKWeight, error) {
	m.callCount++
	return m.minFee, nil
}

// TestMinFeeManager tests that the minFeeManager returns an up to date min fee
// by querying the chain backend and that it returns a cached fee if the chain
// backend was recently queried.
func TestMinFeeManager(t *testing.T) {
	t.Parallel()

	// Initialize the mock backend and let it have a minimum fee rate
	// below our fee floor.
	chainBackend := &mockChainBackend{
		minFee: FeePerKwFloor - 1,
	}

	// Initialise the min fee manager with the standard floor. This should
	// call the chain backend once.
	feeManager, err := newMinFeeManager(
		100*time.Millisecond,
		chainBackend.fetchFee,
		FeePerKwFloor,
	)
	require.NoError(t, err)
	require.Equal(t, 1, chainBackend.callCount)

	// Check that the minimum fee rate is clamped by our fee floor.
	require.Equal(t, feeManager.minFeePerKW, FeePerKwFloor)

	// If the fee is requested again, the stored fee should be returned
	// and the chain backend should not be queried.
	chainBackend.minFee = SatPerKWeight(2000)
	minFee := feeManager.fetchMinFee()
	require.Equal(t, minFee, FeePerKwFloor)
	require.Equal(t, 1, chainBackend.callCount)

	// Fake the passing of time.
	feeManager.lastUpdatedTime = time.Now().Add(-200 * time.Millisecond)

	// If the fee is queried again after the backoff period has passed
	// then the chain backend should be queried again for the min fee.
	minFee = feeManager.fetchMinFee()
	require.Equal(t, SatPerKWeight(2000), minFee)
	require.Equal(t, 2, chainBackend.callCount)
}

// TestMinFeeManagerCustomFloor verifies that a custom feeFloor is respected,
// allowing fee rates below FeePerKwFloor when explicitly configured.
func TestMinFeeManagerCustomFloor(t *testing.T) {
	t.Parallel()

	// A custom floor of 1 sat/kw — effectively no floor.
	customFloor := SatPerKWeight(1)

	// Backend returns a fee rate well below the standard FeePerKwFloor.
	backendFee := SatPerKWeight(100) // ~0.4 sat/vb
	chainBackend := &mockChainBackend{minFee: backendFee}

	feeManager, err := newMinFeeManager(
		100*time.Millisecond,
		chainBackend.fetchFee,
		customFloor,
	)
	require.NoError(t, err)

	// The manager should store and return the backend fee since it is
	// above the custom floor, not clamped to FeePerKwFloor.
	require.Equal(t, backendFee, feeManager.minFeePerKW)

	minFee := feeManager.fetchMinFee()
	require.Equal(t, backendFee, minFee)
}

// TestMinFeeManagerFloorClampsBackend verifies that when the backend returns a
// fee below the configured feeFloor, the floor is enforced.
func TestMinFeeManagerFloorClampsBackend(t *testing.T) {
	t.Parallel()

	customFloor := SatPerKWeight(500) // custom floor: ~2 sat/vb

	// Backend returns something below the custom floor.
	chainBackend := &mockChainBackend{minFee: SatPerKWeight(100)}

	feeManager, err := newMinFeeManager(
		100*time.Millisecond,
		chainBackend.fetchFee,
		customFloor,
	)
	require.NoError(t, err)

	// The fee should be clamped to our custom floor.
	require.Equal(t, customFloor, feeManager.minFeePerKW)

	// Fake time passing and have the backend return a fee above the floor.
	feeManager.lastUpdatedTime = time.Now().Add(-200 * time.Millisecond)
	feeManager.fetchFeeFunc = (&mockChainBackend{
		minFee: SatPerKWeight(800),
	}).fetchFee

	minFee := feeManager.fetchMinFee()
	require.Equal(t, SatPerKWeight(800), minFee)
}
