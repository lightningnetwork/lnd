package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
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
