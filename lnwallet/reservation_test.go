package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestVerifyConstraintsCsvDelay checks that a CSV delay is accepted only when
// it is non-zero and within our maximum.
func TestVerifyConstraintsCsvDelay(t *testing.T) {
	t.Parallel()

	const (
		capacity = btcutil.Amount(1_000_000)
		maxDelay = uint16(2016)
	)

	dustLimit := DustLimitForSize(input.UnknownWitnessSize)
	bounds := &channeldb.ChannelStateBounds{
		ChanReserve:      capacity / 100,
		MaxPendingAmount: lnwire.NewMSatFromSatoshis(capacity),
		MinHTLC:          1,
		MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
	}

	tests := []struct {
		name  string
		delay uint16
		err   string
	}{
		{
			name:  "zero",
			delay: 0,
			err:   "CSV delay must be greater than zero",
		},
		{
			name:  "one",
			delay: 1,
		},
		{
			name:  "max",
			delay: maxDelay,
		},
		{
			name:  "above max",
			delay: maxDelay + 1,
			err:   "CSV delay too large",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			params := &channeldb.CommitmentParams{
				DustLimit: dustLimit,
				CsvDelay:  tc.delay,
			}

			err := VerifyConstraints(
				bounds, params, maxDelay, capacity,
			)
			if tc.err == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tc.err)
			require.ErrorAs(t, err, &ReservationError{})
		})
	}
}
