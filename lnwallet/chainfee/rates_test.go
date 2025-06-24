package chainfee

import (
	"testing"

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
