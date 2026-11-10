package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestReservationAuxChanStatePopulatesNegotiatedConfigs asserts that the aux
// channel state view of a reservation carries the negotiated local and remote
// channel configs rather than the still empty configs of the partial state.
func TestReservationAuxChanStatePopulatesNegotiatedConfigs(t *testing.T) {
	t.Parallel()

	localCfg := &channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			ChanReserve:      btcutil.Amount(1200),
			MaxPendingAmount: lnwire.MilliSatoshi(100_000),
			MinHTLC:          lnwire.MilliSatoshi(1000),
			MaxAcceptedHtlcs: 30,
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: btcutil.Amount(600),
			CsvDelay:  144,
		},
	}
	remoteCfg := &channeldb.ChannelConfig{
		ChannelStateBounds: channeldb.ChannelStateBounds{
			ChanReserve:      btcutil.Amount(2200),
			MaxPendingAmount: lnwire.MilliSatoshi(200_000),
			MinHTLC:          lnwire.MilliSatoshi(2000),
			MaxAcceptedHtlcs: 40,
		},
		CommitmentParams: channeldb.CommitmentParams{
			DustLimit: btcutil.Amount(700),
			CsvDelay:  288,
		},
	}

	_, peerPub := btcec.PrivKeyFromBytes([]byte{1})
	reservation := &ChannelReservation{
		ourContribution: &ChannelContribution{
			ChannelConfig: localCfg,
		},
		theirContribution: &ChannelContribution{
			ChannelConfig: remoteCfg,
		},
		partialState: &channeldb.OpenChannel{
			IdentityPub: peerPub,
		},
	}

	auxState := reservation.AuxChanState()
	require.Equal(t, *localCfg, auxState.LocalChanCfg)
	require.Equal(t, *remoteCfg, auxState.RemoteChanCfg)
}

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
