package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestDustLimitForSize tests that we receive the expected dust limits for
// various script types from btcd's GetDustThreshold function.
func TestDustLimitForSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		size          int
		expectedLimit btcutil.Amount
	}{
		{
			name:          "p2pkh dust limit",
			size:          input.P2PKHSize,
			expectedLimit: btcutil.Amount(546),
		},
		{
			name:          "p2sh dust limit",
			size:          input.P2SHSize,
			expectedLimit: btcutil.Amount(540),
		},
		{
			name:          "p2wpkh dust limit",
			size:          input.P2WPKHSize,
			expectedLimit: btcutil.Amount(294),
		},
		{
			name:          "p2wsh dust limit",
			size:          input.P2WSHSize,
			expectedLimit: btcutil.Amount(330),
		},
		{
			name:          "unknown witness limit",
			size:          input.UnknownWitnessSize,
			expectedLimit: btcutil.Amount(354),
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			dustlimit := DustLimitForSize(test.size)
			require.Equal(t, test.expectedLimit, dustlimit)
		})
	}
}
