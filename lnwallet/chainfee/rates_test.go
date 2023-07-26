package chainfee

import (
	"testing"

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
