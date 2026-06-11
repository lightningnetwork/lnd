package btcwallet

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestSatPerVByteToBTCPerKvB checks the sat/vByte -> BTC/kvB conversion used to
// map an lnd fee-rate ceiling onto bitcoind's submitpackage maxfeerate
// argument. A regression here would silently relax or tighten the user's fee
// ceiling, so the known reference points are pinned.
func TestSatPerVByteToBTCPerKvB(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rate chainfee.SatPerVByte
		want float64
	}{
		{name: "zero", rate: 0, want: 0},
		{name: "1 sat/vByte", rate: 1, want: 0.00001},
		{name: "10 sat/vByte", rate: 10, want: 0.0001},
		{name: "250 sat/vByte", rate: 250, want: 0.0025},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.InDelta(
				t, tc.want, satPerVByteToBTCPerKvB(tc.rate),
				1e-12,
			)
		})
	}
}
