package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestReservationChanStatePopulatesNegotiatedConfigs(t *testing.T) {
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

	reservation := &ChannelReservation{
		ourContribution: &ChannelContribution{
			ChannelConfig: localCfg,
		},
		theirContribution: &ChannelContribution{
			ChannelConfig: remoteCfg,
		},
		partialState: &channeldb.OpenChannel{},
	}

	chanState := reservation.ChanState()
	require.Equal(t, *localCfg, chanState.LocalChanCfg)
	require.Equal(t, *remoteCfg, chanState.RemoteChanCfg)
}
