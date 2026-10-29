package models

import (
	"math"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestComputeFee checks that the forwarding fee is base + amt * rate / 1e6
// without wrapping around, for both policy types.
func TestComputeFee(t *testing.T) {
	t.Parallel()

	const maxMSat = lnwire.MilliSatoshi(math.MaxUint64)

	tests := []struct {
		name string
		base lnwire.MilliSatoshi
		rate lnwire.MilliSatoshi
		amt  lnwire.MilliSatoshi
		fee  lnwire.MilliSatoshi
	}{
		{
			name: "base only",
			base: 1000,
			amt:  5000000,
			fee:  1000,
		},
		{
			name: "rounds down",
			base: 1,
			rate: 500000,
			amt:  3,
			fee:  2,
		},
		{
			name: "1000 ppm of 1 BTC",
			base: 1000,
			rate: 1000,
			amt:  100000000000,
			fee:  100000000 + 1000,
		},

		// The product 4294967298 * 4294967295 does not fit in 64 bits.
		{
			name: "product wraps a uint64",
			rate: 4294967295,
			amt:  4294967298,
			fee:  18446744078004,
		},
		{
			name: "largest quotient that fits",
			rate: 1000000,
			amt:  maxMSat,
			fee:  maxMSat,
		},
		{
			name: "base pushes the fee over the maximum",
			base: 1,
			rate: 1000000,
			amt:  maxMSat,
			fee:  maxMSat,
		},
		{
			name: "quotient exceeds the maximum",
			rate: 1000001,
			amt:  maxMSat,
			fee:  maxMSat,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cached := &CachedEdgePolicy{
				FeeBaseMSat:               test.base,
				FeeProportionalMillionths: test.rate,
			}
			require.Equal(t, test.fee, cached.ComputeFee(test.amt))

			full := &ChannelEdgePolicy{
				FeeBaseMSat:               test.base,
				FeeProportionalMillionths: test.rate,
			}
			require.Equal(t, test.fee, full.ComputeFee(test.amt))
		})
	}
}
