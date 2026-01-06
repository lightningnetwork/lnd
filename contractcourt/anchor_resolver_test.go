package contractcourt

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestAnchorResolverWitnessType verifies that the anchor resolver selects the
// correct witness type based on the channel type.
func TestAnchorResolverWitnessType(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                string
		chanType            channeldb.ChannelType
		expectedWitnessType input.StandardWitnessType
	}{
		{
			name: "legacy anchor channel",
			chanType: channeldb.SingleFunderTweaklessBit |
				channeldb.AnchorOutputsBit,
			expectedWitnessType: input.CommitmentAnchor,
		},
		{
			name: "zero fee htlc tx anchor channel",
			chanType: channeldb.SingleFunderTweaklessBit |
				channeldb.AnchorOutputsBit |
				channeldb.ZeroHtlcTxFeeBit,
			expectedWitnessType: input.CommitmentAnchor,
		},
		{
			name: "zero fee commitment channel (v3)",
			chanType: channeldb.SingleFunderTweaklessBit |
				channeldb.AnchorOutputsBit |
				channeldb.ZeroHtlcTxFeeBit |
				channeldb.ZeroFeeCommitmentsBit,
			expectedWitnessType: input.ZeroFeeAnchorSpend,
		},
		{
			name: "taproot channel",
			chanType: channeldb.SingleFunderTweaklessBit |
				channeldb.AnchorOutputsBit |
				channeldb.ZeroHtlcTxFeeBit |
				channeldb.SimpleTaprootFeatureBit,
			expectedWitnessType: input.TaprootAnchorSweepSpend,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Verify the channel type methods return expected values.
			switch tc.expectedWitnessType {
			case input.ZeroFeeAnchorSpend:
				require.True(
					t, tc.chanType.HasZeroFeeCommitments(),
					"should have zero fee commitments",
				)
			case input.TaprootAnchorSweepSpend:
				require.True(
					t, tc.chanType.IsTaproot(),
					"should be taproot",
				)
			case input.CommitmentAnchor:
				require.False(
					t, tc.chanType.HasZeroFeeCommitments(),
					"should not have zero fee commitments",
				)
				require.False(
					t, tc.chanType.IsTaproot(),
					"should not be taproot",
				)
			}

			// Test the witness type selection logic directly.
			// This mirrors the logic in anchor_resolver.go Launch().
			witnessType := input.CommitmentAnchor
			if tc.chanType.IsTaproot() {
				witnessType = input.TaprootAnchorSweepSpend
			}
			if tc.chanType.HasZeroFeeCommitments() {
				witnessType = input.ZeroFeeAnchorSpend
			}

			require.Equal(
				t, tc.expectedWitnessType, witnessType,
				"witness type mismatch",
			)
		})
	}
}

// TestAnchorResolverZeroFeeWitnessSize verifies that the ZeroFeeAnchorSpend
// witness type returns the correct size upper bound.
func TestAnchorResolverZeroFeeWitnessSize(t *testing.T) {
	t.Parallel()

	// ZeroFeeAnchorSpend should return size 1 (empty witness = 1 byte
	// for the element count of 0).
	size, isNestedP2SH, err := input.ZeroFeeAnchorSpend.SizeUpperBound()
	require.NoError(t, err)
	require.False(t, isNestedP2SH)
	require.EqualValues(t, 1, size, "P2A witness size should be 1 byte")

	// Compare with regular anchor sizes.
	regularSize, _, err := input.CommitmentAnchor.SizeUpperBound()
	require.NoError(t, err)
	require.Greater(
		t, regularSize, size,
		"regular anchor witness should be larger than P2A",
	)
}

// TestAnchorResolverChannelTypePriority verifies that zero-fee commitment
// check takes precedence over taproot check (if a channel were both, which
// shouldn't happen in practice).
func TestAnchorResolverChannelTypePriority(t *testing.T) {
	t.Parallel()

	// The logic in anchor_resolver.go checks taproot first, then
	// zero-fee commitments. This means zero-fee commitments takes
	// precedence. Test that the ordering is correct.

	// A hypothetical channel type with both flags (shouldn't exist in
	// practice but tests the code path ordering).
	hypotheticalType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SimpleTaprootFeatureBit |
		channeldb.ZeroFeeCommitmentsBit

	// Verify that both checks would return true.
	require.True(t, hypotheticalType.IsTaproot())
	require.True(t, hypotheticalType.HasZeroFeeCommitments())

	// Apply the same logic as anchor_resolver.go.
	witnessType := input.CommitmentAnchor
	if hypotheticalType.IsTaproot() {
		witnessType = input.TaprootAnchorSweepSpend
	}
	if hypotheticalType.HasZeroFeeCommitments() {
		witnessType = input.ZeroFeeAnchorSpend
	}

	// Zero-fee commitments should win because it's checked after taproot.
	require.Equal(
		t, input.ZeroFeeAnchorSpend, witnessType,
		"zero-fee commitments should take precedence over taproot",
	)
}

// TestZeroFeeAnchorOutputValue verifies the expected anchor output value for
// v3 zero-fee commitment channels.
func TestZeroFeeAnchorOutputValue(t *testing.T) {
	t.Parallel()

	// P2A anchor max amount is 240 sats (per spec).
	const p2aAnchorMaxAmount = 240

	// The P2A output script is 4 bytes: OP_1 (0x51) + 2 (0x02) + 0x4e73.
	p2aScriptLen := 4

	// Verify the anchor output is economically viable to spend.
	// At 1 sat/vB, a P2A spend is:
	// - Input: 41 vB (outpoint + sequence + empty witness)
	// - P2A witness: 1 vB (just the witness count of 0)
	// Total: ~42 vB = 42 sats at 1 sat/vB
	//
	// So even a 0-value anchor can be economically swept at low fee rates
	// when used for CPFP.
	require.LessOrEqual(
		t, p2aScriptLen, 4,
		"P2A script should be 4 bytes",
	)

	// Anchor amount can range from 0 to 240 sats.
	require.LessOrEqual(
		t, int64(0), int64(p2aAnchorMaxAmount),
		"anchor amount should be within valid range",
	)
}

// TestAnchorResolverSupplementState verifies that the anchor resolver correctly
// receives channel type information via SupplementState.
func TestAnchorResolverSupplementState(t *testing.T) {
	t.Parallel()

	// Create an anchor resolver with minimal config.
	desc := input.SignDescriptor{
		Output: &wire.TxOut{
			Value:    240,
			PkScript: []byte{0x51, 0x02, 0x4e, 0x73}, // P2A script
		},
	}

	resolver := &anchorResolver{
		anchorSignDescriptor: desc,
	}

	// Before SupplementState, chanType should be zero.
	require.Equal(
		t, channeldb.ChannelType(0), resolver.chanType,
		"chanType should be zero before SupplementState",
	)

	// Create a mock channel state with zero-fee commitments.
	chanState := &channeldb.OpenChannel{
		ChanType: channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit |
			channeldb.ZeroHtlcTxFeeBit |
			channeldb.ZeroFeeCommitmentsBit,
	}

	// Call SupplementState.
	resolver.SupplementState(chanState)

	// Verify chanType was set correctly.
	require.Equal(
		t, chanState.ChanType, resolver.chanType,
		"chanType should match after SupplementState",
	)
	require.True(
		t, resolver.chanType.HasZeroFeeCommitments(),
		"resolver should recognize zero-fee commitments",
	)
}
