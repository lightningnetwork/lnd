package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
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

// TestValidateInitialBalances checks that a reservation is only rejected when
// both sides of the initial commitment transaction start out at or below the
// reserve the initiator set in open_channel, which is what BOLT#02 mandates
// the receiver of open_channel to fail on.
func TestValidateInitialBalances(t *testing.T) {
	t.Parallel()

	// initiatorReserve is the channel_reserve_satoshis the initiator
	// proposed in open_channel. BOLT#02 compares both initial commitment
	// outputs against this single value.
	const initiatorReserve = btcutil.Amount(10000)

	// acceptReserve is the reserve we name in accept_channel. It takes no
	// part in the spec comparison, which is about the initiator's value
	// and not the one we impose in return. It is deliberately below the
	// initiator's reserve, so that a regression to comparing each balance
	// against its own reserve accepts a channel the spec requires us to
	// reject.
	const acceptReserve = btcutil.Amount(5000)

	// chanCfg builds the minimal channel config validateInitialBalances
	// reads: the reserve the party it belongs to must maintain.
	chanCfg := func(reserve btcutil.Amount) *channeldb.ChannelConfig {
		bounds := channeldb.ChannelStateBounds{
			ChanReserve: reserve,
		}

		return &channeldb.ChannelConfig{
			ChannelStateBounds: bounds,
		}
	}

	tests := []struct {
		name             string
		initiatorReserve btcutil.Amount
		ourBalance       btcutil.Amount
		theirBalance     btcutil.Amount
		expectErr        bool
	}{
		{
			// The common case for a fundee: we start at zero with
			// no push, but the initiator holds nearly the full
			// capacity.
			name:             "only initiator above reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       0,
			theirBalance:     1000000,
		},
		{
			// The mirror image, which is what a full push looks
			// like.
			name:             "only fundee above reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       1000000,
			theirBalance:     0,
		},
		{
			name:             "both above reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       500000,
			theirBalance:     500000,
		},
		{
			// The reporter's example: an absurd commitment fee rate
			// burns the capacity before either party can use the
			// channel.
			name:             "both below reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       0,
			theirBalance:     9000,
			expectErr:        true,
		},
		{
			// Both balances sit above the reserve we name in
			// accept_channel, so comparing each side against its
			// own reserve would accept the channel. The spec
			// compares both against the initiator's reserve, and
			// by that measure the channel is still unusable.
			name:             "both above accept reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       9000,
			theirBalance:     9000,
			expectErr:        true,
		},
		{
			// Our balance is above the initiator's reserve, which
			// is enough for the channel to be usable whichever
			// reserve it is compared against.
			name:             "one side above initiator reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       10001,
			theirBalance:     0,
		},
		{
			// The spec fails on "less than or equal to", so a
			// balance sitting exactly at the reserve doesn't save
			// the channel: no HTLC could be added without dipping
			// below it.
			name:             "both exactly at reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       initiatorReserve,
			theirBalance:     initiatorReserve,
			expectErr:        true,
		},
		{
			// A single satoshi above the reserve is enough for the
			// channel to be considered usable.
			name:             "one satoshi above reserve",
			initiatorReserve: initiatorReserve,
			ourBalance:       initiatorReserve,
			theirBalance:     initiatorReserve + 1,
		},
		{
			// A zero reserve is legitimate, in which case a
			// channel with any balance at all is fine.
			name:             "zero reserve with balance",
			initiatorReserve: 0,
			ourBalance:       0,
			theirBalance:     1,
		},
		{
			// A zero reserve does not help a channel with no funds
			// anywhere: both balances are still at or below it.
			name:             "zero reserve without balance",
			initiatorReserve: 0,
			ourBalance:       0,
			theirBalance:     0,
			expectErr:        true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			commit := channeldb.ChannelCommitment{
				LocalBalance: lnwire.NewMSatFromSatoshis(
					test.ourBalance,
				),
				RemoteBalance: lnwire.NewMSatFromSatoshis(
					test.theirBalance,
				),
			}

			res := &ChannelReservation{
				ourContribution: &ChannelContribution{
					ChannelConfig: chanCfg(
						test.initiatorReserve,
					),
				},
				theirContribution: &ChannelContribution{
					ChannelConfig: chanCfg(acceptReserve),
				},
				partialState: &chanstate.OpenChannel{
					LocalCommitment: commit,
				},
			}

			err := res.validateInitialBalances()
			if !test.expectErr {
				require.NoError(t, err)
				return
			}

			// The rejection is identified by the sentinel rather
			// than by its message text, so the assertion does not
			// depend on the wording we send the remote peer.
			require.ErrorIs(t, err, ErrBalancesBelowReserveBase)
		})
	}
}
