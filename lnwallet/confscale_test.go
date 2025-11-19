package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestScaleNumConfsProperties tests various properties that ScaleNumConfs
// should satisfy using property-based testing.
func TestScaleNumConfsProperties(t *testing.T) {
	t.Parallel()

	// The result should always be bounded between the minimum and maximum
	// number of confirmations regardless of input values.
	t.Run("bounded_result", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			// Generate random channel amount and push amount.
			chanAmt := rapid.Uint64Range(
				0, maxChannelSize*10,
			).Draw(t, "chanAmt")
			pushAmtSats := rapid.Uint64Range(
				0, chanAmt,
			).Draw(t, "pushAmtSats")
			pushAmt := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmtSats),
			)

			result := ScaleNumConfs(
				btcutil.Amount(chanAmt), pushAmt,
			)

			// Check bounds
			require.GreaterOrEqual(
				t, result, uint16(minRequiredConfs),
				"result should be >= minRequiredConfs",
			)
			require.LessOrEqual(
				t, result, uint16(maxRequiredConfs),
				"result should be <= maxRequiredConfs",
			)
		})
	})

	// Larger channel amounts and push amounts should require equal or more
	// confirmations, ensuring the function is monotonically increasing.
	t.Run("monotonicity", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			// Generate two channel amounts where amt1 <= amt2.
			amt1 := rapid.Uint64Range(
				0, maxChannelSize,
			).Draw(t, "amt1")
			amt2 := rapid.Uint64Range(
				amt1, maxChannelSize,
			).Draw(t, "amt2")

			// Generate push amounts proportional to channel size.
			pushAmt1Sats := rapid.Uint64Range(
				0, amt1,
			).Draw(t, "pushAmt1")
			pushAmt2Sats := rapid.Uint64Range(
				pushAmt1Sats, amt2,
			).Draw(t, "pushAmt2")

			pushAmt1 := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmt1Sats),
			)
			pushAmt2 := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmt2Sats),
			)

			confs1 := ScaleNumConfs(btcutil.Amount(amt1), pushAmt1)
			confs2 := ScaleNumConfs(btcutil.Amount(amt2), pushAmt2)

			// Larger or equal stake should require equal or more
			// confirmations.
			require.GreaterOrEqual(
				t, confs2, confs1,
				"larger amount should require equal or "+
					"more confirmations",
			)
		})
	})

	// Wumbo channels (those exceeding the max standard channel size) should
	// always require the maximum number of confirmations for safety.
	t.Run("wumbo_max_confs", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			// Generate wumbo channel amount (above maxChannelSize).
			wumboAmt := rapid.Uint64Range(
				maxChannelSize+1, maxChannelSize*100,
			).Draw(t, "wumboAmt")
			pushAmtSats := rapid.Uint64Range(
				0, wumboAmt,
			).Draw(t, "pushAmtSats")
			pushAmt := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmtSats),
			)

			result := ScaleNumConfs(
				btcutil.Amount(wumboAmt), pushAmt,
			)

			require.Equal(
				t, uint16(maxRequiredConfs), result,
				"wumbo channels should always get "+
					"max confirmations",
			)
		})
	})

	// Zero channel amounts should always result in the minimum number of
	// confirmations since there's no value at risk.
	t.Run("zero_gets_min", func(t *testing.T) {
		result := ScaleNumConfs(0, 0)
		require.Equal(
			t, uint16(minRequiredConfs), result,
			"zero amount should get minimum confirmations",
		)
	})

	// The function should be deterministic, always returning the same
	// output for the same input values.
	t.Run("determinism", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			chanAmt := rapid.Uint64Range(
				0, maxChannelSize*2,
			).Draw(t, "chanAmt")
			pushAmtSats := rapid.Uint64Range(
				0, chanAmt,
			).Draw(t, "pushAmtSats")
			pushAmt := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmtSats),
			)

			// Call multiple times with same inputs.
			result1 := ScaleNumConfs(
				btcutil.Amount(chanAmt), pushAmt,
			)
			result2 := ScaleNumConfs(
				btcutil.Amount(chanAmt), pushAmt,
			)
			result3 := ScaleNumConfs(
				btcutil.Amount(chanAmt), pushAmt,
			)

			require.Equal(
				t, result1, result2,
				"function should be deterministic",
			)
			require.Equal(
				t, result2, result3,
				"function should be deterministic",
			)
		})
	})

	// Adding a push amount to a channel should require equal or more
	// confirmations compared to the same channel without a push amount.
	t.Run("push_amount_effect", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			// Fix channel amount, vary push amount
			chanAmt := rapid.Uint64Range(
				1, maxChannelSize,
			).Draw(t, "chanAmt")
			pushAmt1Sats := rapid.Uint64Range(
				0, chanAmt/2,
			).Draw(t, "pushAmt1")
			pushAmt2Sats := rapid.Uint64Range(
				pushAmt1Sats, chanAmt,
			).Draw(t, "pushAmt2")

			pushAmt1 := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmt1Sats),
			)
			pushAmt2 := lnwire.NewMSatFromSatoshis(
				btcutil.Amount(pushAmt2Sats),
			)

			confs1 := ScaleNumConfs(
				btcutil.Amount(chanAmt), pushAmt1,
			)
			confs2 := ScaleNumConfs(
				btcutil.Amount(chanAmt), pushAmt2,
			)

			// More push amount should require equal or more
			// confirmations.
			require.GreaterOrEqual(
				t, confs2, confs1,
				"larger push amount should "+
					"require equal or more confirmations",
			)
		})
	})
}

// TestScaleNumConfsKnownValues tests ScaleNumConfs with specific known values
// to ensure the scaling formula works as expected.
func TestScaleNumConfsKnownValues(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		chanAmt  btcutil.Amount
		pushAmt  lnwire.MilliSatoshi
		expected uint16
	}{
		{
			name:     "zero amounts",
			chanAmt:  0,
			pushAmt:  0,
			expected: minRequiredConfs,
		},
		{
			name:     "tiny channel",
			chanAmt:  1000,
			pushAmt:  0,
			expected: minRequiredConfs,
		},
		{
			name:     "small channel no push",
			chanAmt:  100_000,
			pushAmt:  0,
			expected: minRequiredConfs,
		},
		{
			name:     "half max channel no push",
			chanAmt:  maxChannelSize / 2,
			pushAmt:  0,
			expected: 2,
		},
		{
			name:     "max channel no push",
			chanAmt:  maxChannelSize,
			pushAmt:  0,
			expected: maxRequiredConfs,
		},
		{
			name:     "wumbo channel",
			chanAmt:  maxChannelSize * 2,
			pushAmt:  0,
			expected: maxRequiredConfs,
		},
		{
			name:     "small channel with push",
			chanAmt:  100_000,
			pushAmt:  lnwire.NewMSatFromSatoshis(50_000),
			expected: minRequiredConfs,
		},
		{
			name:     "medium channel with significant push",
			chanAmt:  maxChannelSize / 4,
			pushAmt:  lnwire.NewMSatFromSatoshis(maxChannelSize / 4),
			expected: 2,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result := ScaleNumConfs(tc.chanAmt, tc.pushAmt)

			require.Equal(
				t, tc.expected, result,
				"chanAmt=%d, pushAmt=%d", tc.chanAmt,
				tc.pushAmt,
			)
		})
	}
}

// TestFundingConfsForAmounts verifies that FundingConfsForAmounts is a simple
// wrapper around ScaleNumConfs.
func TestFundingConfsForAmounts(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		chanAmt := rapid.Uint64Range(
			0, maxChannelSize*2,
		).Draw(t, "chanAmt")
		pushAmtSats := rapid.Uint64Range(
			0, chanAmt,
		).Draw(t, "pushAmtSats")
		pushAmt := lnwire.NewMSatFromSatoshis(
			btcutil.Amount(pushAmtSats),
		)

		// Both functions should return the same result.
		scaleResult := ScaleNumConfs(btcutil.Amount(chanAmt), pushAmt)
		fundingResult := FundingConfsForAmounts(
			btcutil.Amount(chanAmt), pushAmt,
		)

		require.Equal(
			t, scaleResult, fundingResult,
			"FundingConfsForAmounts should return "+
				"same result as ScaleNumConfs",
		)
	})
}

// TestCloseConfsForCapacity verifies that CloseConfsForCapacity correctly
// wraps ScaleNumConfs with zero push amount and enforces a minimum of 3
// confirmations for reorg safety.
func TestCloseConfsForCapacity(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		capacity := rapid.Uint64Range(
			0, maxChannelSize*2,
		).Draw(t, "capacity")

		// CloseConfsForCapacity should be equivalent to ScaleNumConfs
		// with 0 push, but with a minimum of 3 confirmations enforced
		// for reorg safety.
		closeConfs := CloseConfsForCapacity(btcutil.Amount(capacity))
		scaleConfs := ScaleNumConfs(btcutil.Amount(capacity), 0)

		// The result should be at least the scaled value, but with a
		// minimum of 3 confirmations.
		const minCoopCloseConfs = 3
		expectedConfs := uint32(scaleConfs)
		if expectedConfs < minCoopCloseConfs {
			expectedConfs = minCoopCloseConfs
		}

		require.Equal(
			t, expectedConfs, closeConfs,
			"CloseConfsForCapacity should match "+
				"ScaleNumConfs with 0 push amount, "+
				"but with minimum of 3 confs",
		)
	})
}
