package chainfee

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestSatPerVByteConversion checks that the conversion from sat/vb to either
// sat/kw or sat/kvb is correct.
func TestSatPerVByteConversion(t *testing.T) {
	t.Parallel()

	// Create a test fee rate of 1 sat/vb.
	rate := SatPerVByte(1)

	// 1 sat/vb should be equal to 1000 sat/kvb.
	require.Equal(t, SatPerKVByte(1000), rate.FeePerKVByte())

	// 1 sat/vb should be equal to 250 sat/kw.
	require.Equal(t, SatPerKWeight(250), rate.FeePerKWeight())
}

// TestFeeForWeightRoundUp checks that the FeeForWeightRoundUp method correctly
// rounds up the fee for a given weight.
func TestFeeForWeightRoundUp(t *testing.T) {
	feeRate := SatPerVByte(1).FeePerKWeight()
	txWeight := lntypes.WeightUnit(674) // 674 weight units is 168.5 vb.

	require.EqualValues(t, 168, feeRate.FeeForWeight(txWeight))
	require.EqualValues(t, 169, feeRate.FeeForWeightRoundUp(txWeight))
}

// TestNewSatPerKWeightFloor checks that converting a fee to a fee rate never
// produces a fee above the original amount.
func TestNewSatPerKWeightFloor(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		fee      btcutil.Amount
		weight   lntypes.WeightUnit
		expected SatPerKWeight
	}{
		{
			name:     "exact",
			fee:      500,
			weight:   1_000,
			expected: 500,
		},
		{
			name:     "fractional",
			fee:      500_000,
			weight:   14_144,
			expected: 35_350,
		},
		{
			name:     "less than one sat per kw",
			fee:      1,
			weight:   2_000,
			expected: 0,
		},
		{
			name:     "zero weight",
			fee:      500,
			weight:   0,
			expected: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rate := NewSatPerKWeightFloor(tc.fee, tc.weight)
			require.Equal(t, tc.expected, rate)
			require.LessOrEqual(t, rate.FeeForWeight(tc.weight),
				tc.fee)
		})
	}
}
