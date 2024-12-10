package lnwallet

import (
	"strconv"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// genValidAmount generates valid bitcoin amounts (non-negative).
func genValidAmount(t *rapid.T, label string) btcutil.Amount {
	return btcutil.Amount(
		rapid.Int64Range(
			100_000, 21_000_000*100_000_000,
		).Draw(t, label),
	)
}

// genCoopCloseFee generates a reasonable non-zero cooperative close fee.
func genCoopCloseFee(t *rapid.T) btcutil.Amount {
	// Generate a fee between 250-10000 sats which is a reasonable range for
	// closing transactions
	return btcutil.Amount(
		rapid.Int64Range(250, 10_000).Draw(t, "coop_close_fee"),
	)
}

// genChannelType generates various channel types, ensuring good coverage of
// different channel configurations including anchor outputs and other features.
func genChannelType(t *rapid.T) channeldb.ChannelType {
	var chanType channeldb.ChannelType

	// For each bit, decide randomly if it should be set.
	bits := []channeldb.ChannelType{
		channeldb.DualFunderBit,
		channeldb.SingleFunderTweaklessBit,
		channeldb.NoFundingTxBit,
		channeldb.AnchorOutputsBit,
		channeldb.FrozenBit,
		channeldb.ZeroHtlcTxFeeBit,
		channeldb.LeaseExpirationBit,
		channeldb.ZeroConfBit,
		channeldb.ScidAliasChanBit,
		channeldb.ScidAliasFeatureBit,
		channeldb.SimpleTaprootFeatureBit,
		channeldb.TapscriptRootBit,
	}

	// Helper to bias towards setting specific bits more frequently.
	setBit := func(bit channeldb.ChannelType, probability int) {
		bitRange := rapid.IntRange(0, 100)
		label := "bit_" + strconv.FormatUint(uint64(bit), 2)
		if bitRange.Draw(t, label) < probability {
			chanType |= bit
		}
	}

	// We want to ensure good coverage of anchor outputs since they affect
	// the balance calculation directly. We'll set the anchor bit with a 50%
	// chance.
	setBit(channeldb.AnchorOutputsBit, 50)

	// For other bits, use varying probabilities to ensure good
	// distribution.
	for _, bit := range bits {
		// The anchor bit was already set above so we can skip it here.
		if bit == channeldb.AnchorOutputsBit {
			continue
		}

		// Some bits are related, so we'll make sure we capture that
		// dep.
		switch bit {
		case channeldb.TapscriptRootBit:
			// If we have TapscriptRootBit, we must have
			// SimpleTaprootFeatureBit.
			if chanType&channeldb.SimpleTaprootFeatureBit != 0 {
				// 70% chance if taproot is enabled.
				setBit(bit, 70)
			}

		case channeldb.DualFunderBit:
			// 40% chance of dual funding.
			setBit(bit, 40)

		default:
			// 30% chance for other bits.
			setBit(bit, 30)
		}
	}

	return chanType
}

// genFeePayer generates optional fee payer.
func genFeePayer(t *rapid.T) fn.Option[lntypes.ChannelParty] {
	if !rapid.Bool().Draw(t, "has_fee_payer") {
		return fn.None[lntypes.ChannelParty]()
	}

	if rapid.Bool().Draw(t, "is_local") {
		return fn.Some(lntypes.Local)
	}

	return fn.Some(lntypes.Remote)
}

// genCommitFee generates a reasonable non-zero commitment fee.
func genCommitFee(t *rapid.T) btcutil.Amount {
	// Generate a reasonable commit fee between 100-5000 sats
	return btcutil.Amount(
		rapid.Int64Range(100, 5_000).Draw(t, "commit_fee"),
	)
}

// TestCoopCloseBalance tests fundamental properties of CoopCloseBalance. This
// ensures that the closing fee is always subtracted from the correct balance,
// amongst other properties.
func TestCoopCloseBalance(tt *testing.T) {
	tt.Parallel()

	rapid.Check(tt, func(t *rapid.T) {
		require := require.New(t)

		// Generate test inputs
		chanType := genChannelType(t)
		isInitiator := rapid.Bool().Draw(t, "is_initiator")

		// Generate amounts using specific generators
		coopCloseFee := genCoopCloseFee(t)
		ourBalance := genValidAmount(t, "local balance")
		theirBalance := genValidAmount(t, "remote balance")
		feePayer := genFeePayer(t)
		commitFee := genCommitFee(t)

		ourFinal, theirFinal, err := CoopCloseBalance(
			chanType, isInitiator, coopCloseFee,
			ourBalance, theirBalance, commitFee, feePayer,
		)

		// Property 1: If inputs are non-negative, we either get valid
		// outputs or an error.
		if err != nil {
			// On error, final balances should be 0
			require.Zero(
				ourFinal,
				"expected zero our_balance on error",
			)
			require.Zero(
				theirFinal,
				"expected zero their_balance on error",
			)

			return
		}

		// Property 2: Final balances should be non-negative.
		require.GreaterOrEqual(
			ourFinal, btcutil.Amount(0),
			"our final balance should be non-negative",
		)
		require.GreaterOrEqual(
			theirFinal, btcutil.Amount(0),
			"their final balance should be non-negative",
		)

		// Property 3: Total balance should be conserved minus fees.
		initialTotal := ourBalance + theirBalance
		initialTotal += commitFee

		if chanType.HasAnchors() {
			initialTotal += 2 * AnchorSize
		}

		finalTotal := ourFinal + theirFinal + coopCloseFee
		require.Equal(
			initialTotal, finalTotal,
			"total balance should be conserved",
		)

		// Property 4: When feePayer is specified, that party's balance
		// should be reduced by exactly the coopCloseFee.
		if feePayer.IsSome() {
			payer := feePayer.UnwrapOrFail(tt)

			if payer == lntypes.Local {
				require.LessOrEqual(
					ourBalance-(ourFinal+coopCloseFee),
					btcutil.Amount(0),
					"local balance reduced by more than fee", //nolint:ll
				)
			} else {
				require.LessOrEqual(
					theirBalance-(theirFinal+coopCloseFee),
					btcutil.Amount(0),
					"remote balance reduced by more than fee", //nolint:ll
				)
			}
		}

		// Property 5: For anchor channels, verify the correct final
		// balance factors in the anchor amount.
		if chanType.HasAnchors() {
			// The initiator delta is the commit fee plus anchor
			// amount.
			initiatorDelta := commitFee + 2*AnchorSize

			// Default to initiator paying unless explicitly
			// specified.
			isLocalPaying := isInitiator
			if feePayer.IsSome() {
				isLocalPaying = feePayer.UnwrapOrFail(tt) ==
					lntypes.Local
			}

			if isInitiator {
				expectedBalance := ourBalance + initiatorDelta
				if isLocalPaying {
					expectedBalance -= coopCloseFee
				}

				require.Equal(expectedBalance, ourFinal,
					"initiator (local) balance incorrect")
			} else {
				// They are the initiator
				expectedBalance := theirBalance + initiatorDelta
				if !isLocalPaying {
					expectedBalance -= coopCloseFee
				}

				require.Equal(expectedBalance, theirFinal,
					"initiator (remote) balance incorrect")
			}
		}
	})
}
