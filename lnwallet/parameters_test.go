package lnwallet

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestDefaultRoutingFeeLimitForAmount tests that we use the correct default
// routing fee depending on the amount.
func TestDefaultRoutingFeeLimitForAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		amount        lnwire.MilliSatoshi
		expectedLimit lnwire.MilliSatoshi
	}{
		{
			amount:        1,
			expectedLimit: 1,
		},
		{
			amount:        lnwire.NewMSatFromSatoshis(1_000),
			expectedLimit: lnwire.NewMSatFromSatoshis(1_000),
		},
		{
			amount:        lnwire.NewMSatFromSatoshis(1_001),
			expectedLimit: 50_050,
		},
		{
			amount:        5_000_000_000,
			expectedLimit: 250_000_000,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(fmt.Sprintf("%d sats", test.amount), func(t *testing.T) {
			feeLimit := DefaultRoutingFeeLimitForAmount(test.amount)
			require.Equal(t, int64(test.expectedLimit), int64(feeLimit))
		})
	}
}

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
