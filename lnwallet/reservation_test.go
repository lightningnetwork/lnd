package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestValidateInitialBalances checks that a reservation is only rejected when
// both sides of the initial commitment transaction start out at or below their
// respective channel reserves, which is what BOLT#02 mandates the receiver of
// open_channel to fail on.
func TestValidateInitialBalances(t *testing.T) {
	t.Parallel()

	const reserve = btcutil.Amount(10000)

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
		name         string
		ourBalance   btcutil.Amount
		ourReserve   btcutil.Amount
		theirBalance btcutil.Amount
		theirReserve btcutil.Amount
		expectErr    bool
	}{
		{
			// The common case for a fundee: we start at zero with
			// no push, but the initiator holds nearly the full
			// capacity.
			name:         "only initiator above reserve",
			ourBalance:   0,
			ourReserve:   reserve,
			theirBalance: 1000000,
			theirReserve: reserve,
		},
		{
			// The mirror image, which is what a full push looks
			// like.
			name:         "only fundee above reserve",
			ourBalance:   1000000,
			ourReserve:   reserve,
			theirBalance: 0,
			theirReserve: reserve,
		},
		{
			name:         "both above reserve",
			ourBalance:   500000,
			ourReserve:   reserve,
			theirBalance: 500000,
			theirReserve: reserve,
		},
		{
			// An absurd commitment fee rate burns the capacity
			// before either party can use the channel.
			name:         "both below reserve",
			ourBalance:   0,
			ourReserve:   reserve,
			theirBalance: 9000,
			theirReserve: reserve,
			expectErr:    true,
		},
		{
			// The spec fails on "less than or equal to", so a
			// balance sitting exactly at the reserve doesn't save
			// the channel: no HTLC could be added without dipping
			// below it.
			name:         "both exactly at reserve",
			ourBalance:   reserve,
			ourReserve:   reserve,
			theirBalance: reserve,
			theirReserve: reserve,
			expectErr:    true,
		},
		{
			// A single satoshi above the reserve is enough for the
			// channel to be considered usable.
			name:         "one satoshi above reserve",
			ourBalance:   reserve,
			ourReserve:   reserve,
			theirBalance: reserve + 1,
			theirReserve: reserve,
		},
		{
			// Both reserves may legitimately be zero, in which
			// case a channel with any balance at all is fine.
			name:         "zero reserves with balance",
			ourBalance:   0,
			ourReserve:   0,
			theirBalance: 1,
			theirReserve: 0,
		},
		{
			// With zero reserves and no funds anywhere, the
			// channel is still useless.
			name:         "zero reserves without balance",
			ourBalance:   0,
			ourReserve:   0,
			theirBalance: 0,
			theirReserve: 0,
			expectErr:    true,
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
					ChannelConfig: chanCfg(test.ourReserve),
				},
				theirContribution: &ChannelContribution{
					ChannelConfig: chanCfg(
						test.theirReserve,
					),
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

			require.ErrorContains(
				t, err, "both initial balances are below "+
					"their channel reserve",
			)
		})
	}
}
