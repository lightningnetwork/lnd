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

// TestValidateInitialBalances checks the two conditions that fail a channel
// whose initial commitment leaves neither side able to spend.
//
// The first is the BOLT#02 MUST: both initial outputs at or below the single
// channel_reserve_satoshis the initiator named in open_channel, which is the
// reserve the initiator requires of us. The second is the usability check:
// each output at or below the reserve its own side must maintain. The
// configurations below are chosen so the two differ.
func TestValidateInitialBalances(t *testing.T) {
	t.Parallel()

	// initiatorReserve is the channel_reserve_satoshis the initiator
	// proposed in open_channel: the reserve they require of us.
	const initiatorReserve = btcutil.Amount(10000)

	// acceptReserve is the reserve we name in accept_channel: the reserve
	// we require of them. It is deliberately below the initiator's, which
	// is the configuration where the spec check rejects a channel the
	// usability check would accept.
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

		// acceptReserve overrides the package-level value for cases
		// that need our reserve to sit above the initiator's.
		acceptReserve btcutil.Amount

		ourBalance   btcutil.Amount
		theirBalance btcutil.Amount
		expectErr    bool
	}{
		{
			// The common case for a fundee: we start at zero with
			// no push, but the initiator holds nearly the full
			// capacity.
			name:             "only initiator above reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       0,
			theirBalance:     1000000,
		},
		{
			// The mirror image, which is what a full push looks
			// like.
			name:             "only fundee above reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       1000000,
			theirBalance:     0,
		},
		{
			name:             "both above reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       500000,
			theirBalance:     500000,
		},
		{
			// The reporter's example: an absurd commitment fee rate
			// burns the capacity before either party can use the
			// channel.
			name:             "both below reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       0,
			theirBalance:     9000,
			expectErr:        true,
		},
		{
			// Both balances sit above the reserve we name in
			// accept_channel, so the usability check alone would
			// accept. The spec check still fires, because both are
			// at or below the initiator's reserve.
			name:             "both above accept reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       9000,
			theirBalance:     9000,
			expectErr:        true,
		},
		{
			// The spec check rejects a USABLE channel
			// here, and that is intentional: it is a MUST,
			// not a heuristic. Our accept reserve is
			// 5000 and the initiator's balance is 8000,
			// so they can still spend, but both outputs
			// are at or below the initiator's 10000
			// reserve, so BOLT#02 requires us to fail the
			// channel.
			//
			// This case exists to pin that behaviour. A
			// future change that "fixes" the
			// over-rejection by dropping the spec
			// comparison must fail here.
			name:             "spec rejects one-sided",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       0,
			theirBalance:     8000,
			expectErr:        true,
		},
		{
			// Our balance is above the initiator's reserve, which
			// clears the spec check, and the initiator's balance
			// clears the reserve we named for them, so neither
			// check fires.
			name:             "one side above initiator reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       10001,
			theirBalance:     0,
		},
		{
			// The dead-channel case the spec check alone misses.
			// The initiator names a reserve of us BELOW the reserve
			// we name of them, so their balance sits above the
			// initiator's reserve and the spec check does not fire.
			// But it is at or below the reserve we
			// require of them, and our own balance is
			// at or below the initiator's, so neither
			// side can spend.
			name:             "dead channel above reserve",
			initiatorReserve: 1000,
			acceptReserve:    5000,
			ourBalance:       0,
			theirBalance:     5000,
			expectErr:        true,
		},
		{
			// The spec fails on "less than or equal to", so a
			// balance sitting exactly at the reserve doesn't save
			// the channel: no HTLC could be added without dipping
			// below it.
			name:             "both exactly at reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       initiatorReserve,
			theirBalance:     initiatorReserve,
			expectErr:        true,
		},
		{
			// A single satoshi above the reserve is enough for the
			// channel to be considered usable.
			name:             "one satoshi above reserve",
			initiatorReserve: initiatorReserve,
			acceptReserve:    acceptReserve,
			ourBalance:       initiatorReserve,
			theirBalance:     initiatorReserve + 1,
		},
		{
			// A zero reserve is legitimate, in which case a
			// channel with any balance at all is fine.
			name:             "zero reserve with balance",
			initiatorReserve: 0,
			acceptReserve:    0,
			ourBalance:       0,
			theirBalance:     1,
		},
		{
			// A zero reserve does not help a channel with no funds
			// anywhere: both balances are still at or below it.
			name:             "zero reserve without balance",
			initiatorReserve: 0,
			acceptReserve:    0,
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
					ChannelConfig: chanCfg(
						test.acceptReserve,
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

			// The rejection is identified by the sentinel rather
			// than by its message text, so the assertion does not
			// depend on the wording we send the remote peer.
			require.ErrorIs(t, err, ErrBalancesBelowReserveBase)
		})
	}
}

// TestValidateInitialBalancesUnionIsAdditive drives the real check across a
// sweep of reserves and balances and asserts the property that makes the
// usability comparison safe to layer onto the spec one: every channel it
// rejects beyond what the spec comparison alone would reject must be one where
// NEITHER side can spend. Adding it therefore costs us no usable channel.
//
// The predicate the spec comparison would apply is computed here independently,
// and the rejection itself comes from validateInitialBalances, so this test
// would fail if the implementation drifted from the property.
func TestValidateInitialBalancesUnionIsAdditive(t *testing.T) {
	t.Parallel()

	// msat is a shorthand for the long constructor below, so the
	// reservation builder stays within the column limit.
	msat := lnwire.NewMSatFromSatoshis

	chanCfg := func(reserve btcutil.Amount) *channeldb.ChannelConfig {
		return &channeldb.ChannelConfig{
			ChannelStateBounds: channeldb.ChannelStateBounds{
				ChanReserve: reserve,
			},
		}
	}

	amounts := []btcutil.Amount{
		0, 1, 4999, 5000, 5001, 9999, 10000, 10001, 20000,
	}

	// The reserve the initiator names for us, and the reserve we name for
	// them in accept_channel. Both directions of their relationship are
	// exercised, since that is what decides which comparison bites.
	reserves := []btcutil.Amount{1000, 5000, 10000, 20000}

	addedRejections := 0

	reservation := func(
		ourBalance, theirBalance btcutil.Amount,
		ourReserve, theirReserve btcutil.Amount,
	) *ChannelReservation {

		return &ChannelReservation{
			ourContribution: &ChannelContribution{
				ChannelConfig: chanCfg(ourReserve),
			},
			theirContribution: &ChannelContribution{
				ChannelConfig: chanCfg(theirReserve),
			},
			partialState: &chanstate.OpenChannel{
				LocalCommitment: channeldb.ChannelCommitment{
					LocalBalance:  msat(ourBalance),
					RemoteBalance: msat(theirBalance),
				},
			},
		}
	}

	for _, ourReserve := range reserves {
		for _, theirReserve := range reserves {
			for _, ourBalance := range amounts {
				for _, theirBalance := range amounts {
					res := reservation(
						ourBalance, theirBalance,
						ourReserve, theirReserve,
					)

					err := res.validateInitialBalances()

					// What the BOLT-02 comparison on its
					// own would have done, computed here
					// rather than read from the code under
					// test.
					specAlone := ourBalance <= ourReserve &&
						theirBalance <= ourReserve

					// A channel is usable if either side
					// can add an HTLC without dipping below
					// the reserve that side must maintain.
					usable := ourBalance > ourReserve ||
						theirBalance > theirReserve

					rejected := err != nil

					// The spec comparison must never be
					// bypassed: whenever it fires, so must
					// the check.
					if specAlone && !rejected {
						t.Fatalf("spec comparison did "+
							"not reject: our=%v "+
							"their=%v "+
							"ourReserve=%v "+
							"theirReserve=%v",
							ourBalance,
							theirBalance,
							ourReserve,
							theirReserve,
						)
					}

					// Any rejection beyond the spec's must
					// be a channel neither side can spend
					// on.
					if rejected && !specAlone {
						addedRejections++

						if usable {
							t.Fatalf("rejected a "+
								"usable "+
								"channel the "+
								"spec allows: "+
								"our=%v "+
								"their=%v "+
								"ourRes=%v "+
								"theirRes=%v",
								ourBalance,
								theirBalance,
								ourReserve,
								theirReserve,
							)
						}
					}

					// And the converse: a channel where
					// neither side can spend must not be
					// accepted.
					if !usable && !rejected {
						t.Fatalf("accepted a dead "+
							"channel: our=%v "+
							"their=%v "+
							"ourReserve=%v "+
							"theirReserve=%v",
							ourBalance,
							theirBalance,
							ourReserve,
							theirReserve,
						)
					}
				}
			}
		}
	}

	// The sweep has to actually reach the added-rejection band, or the
	// assertions above would vacuously hold.
	require.Positive(t, addedRejections,
		"the sweep never hit a channel the "+
			"usability check adds, so it did "+
			"not exercise the property")
}
