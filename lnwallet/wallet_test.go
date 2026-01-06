package lnwallet

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestRegisterFundingIntent checks RegisterFundingIntent behaves as expected.
func TestRegisterFundingIntent(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a testing wallet.
	lw, err := NewLightningWallet(Config{})
	require.NoError(err)

	// Init an empty testing channel ID.
	var testID [32]byte

	// Call the method with empty ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrEmptyPendingChanID)

	// Modify the ID and call the method again should result in no error.
	testID[0] = 1
	err = lw.RegisterFundingIntent(testID, nil)
	require.NoError(err)

	// Call the method using the same ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrDuplicatePendingChanID)
}

// TestVerifyConstraintsMaxHTLCs tests that VerifyConstraints correctly
// validates max_accepted_htlcs based on commitment type:
// - 483 for non-zero-fee channels
// - 114 for zero-fee commitment channels (due to v3 10kvB limit).
func TestVerifyConstraintsMaxHTLCs(t *testing.T) {
	t.Parallel()

	// Create valid base constraints that will pass all other checks.
	validDustLimit := DustLimitForSize(input.UnknownWitnessSize)
	channelCapacity := btcutil.Amount(1_000_000)

	makeValidBounds := func(maxHtlcs uint16) *channeldb.ChannelStateBounds {
		return &channeldb.ChannelStateBounds{
			MaxPendingAmount: lnwire.MilliSatoshi(500_000_000),
			ChanReserve:      10_000,
			MinHTLC:          1_000,
			MaxAcceptedHtlcs: maxHtlcs,
		}
	}

	validParams := &channeldb.CommitmentParams{
		DustLimit: validDustLimit,
		CsvDelay:  144,
	}

	testCases := []struct {
		name       string
		commitType CommitmentType
		maxHtlcs   uint16
		wantErr    bool
		errContain string
	}{
		{
			// Legacy channel with max HTLCs at limit (483).
			name:       "legacy at limit",
			commitType: CommitmentTypeTweakless,
			maxHtlcs:   uint16(input.MaxHTLCNumber / 2),
			wantErr:    false,
		},
		{
			// Legacy channel with max HTLCs over limit.
			name:       "legacy over limit",
			commitType: CommitmentTypeTweakless,
			maxHtlcs:   uint16(input.MaxHTLCNumber/2) + 1,
			wantErr:    true,
			errContain: "maxHtlcs is too large",
		},
		{
			// Zero-fee channel with max HTLCs at limit (114).
			name:       "zero-fee at limit",
			commitType: CommitmentTypeZeroFee,
			maxHtlcs:   uint16(input.MaxHTLCNumberV3 / 2),
			wantErr:    false,
		},
		{
			// Zero-fee channel with max HTLCs over v3 limit.
			name:       "zero-fee over limit",
			commitType: CommitmentTypeZeroFee,
			maxHtlcs:   uint16(input.MaxHTLCNumberV3/2) + 1,
			wantErr:    true,
			errContain: "maxHtlcs is too large",
		},
		{
			// Zero-fee channel trying to use legacy limit.
			name:       "zero-fee at legacy limit",
			commitType: CommitmentTypeZeroFee,
			maxHtlcs:   uint16(input.MaxHTLCNumber / 2),
			wantErr:    true,
			errContain: "maxHtlcs is too large",
		},
		{
			// Anchor channel (non-zero-fee) with standard limit.
			name:       "anchor at limit",
			commitType: CommitmentTypeAnchorsZeroFeeHtlcTx,
			maxHtlcs:   uint16(input.MaxHTLCNumber / 2),
			wantErr:    false,
		},
		{
			// Simple taproot channel at limit.
			name:       "taproot at limit",
			commitType: CommitmentTypeSimpleTaproot,
			maxHtlcs:   uint16(input.MaxHTLCNumber / 2),
			wantErr:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bounds := makeValidBounds(tc.maxHtlcs)

			err := VerifyConstraints(
				bounds, validParams, 2016,
				channelCapacity, tc.commitType,
			)

			if tc.wantErr {
				require.Error(t, err)
				require.True(
					t,
					strings.Contains(err.Error(), tc.errContain),
					"expected error containing %q, got %v",
					tc.errContain, err,
				)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
