package chanfunding

import (
	"encoding/hex"
	"regexp"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

var (
	p2wkhScript, _ = hex.DecodeString(
		"001411034bdcb6ccb7744fdfdeea958a6fb0b415a032",
	)

	np2wkhScript, _ = hex.DecodeString(
		"a914f7bd5b8077b9549653dacf96f824af9d931663e687",
	)

	p2khScript, _ = hex.DecodeString(
		"76a91411034bdcb6ccb7744fdfdeea958a6fb0b415a03288ac",
	)

	defaultChanFundingChangeType = P2TRChangeAddress
)

// fundingFee is a helper method that returns the fee estimate used for a tx
// with the given number of inputs and the optional change output. This matches
// the estimate done by the wallet.
func fundingFee(feeRate chainfee.SatPerKWeight, numInput int, // nolint:unparam
	change bool) btcutil.Amount {

	var weightEstimate input.TxWeightEstimator

	// All inputs.
	for i := 0; i < numInput; i++ {
		weightEstimate.AddP2WKHInput()
	}

	// The multisig funding output.
	weightEstimate.AddP2WSHOutput()

	// Optionally count a change output.
	if change {
		weightEstimate.AddP2TROutput()
	}

	totalWeight := weightEstimate.Weight()
	return feeRate.FeeForWeight(totalWeight)
}

// TestCalculateFees tests that the helper function to calculate the fees
// both with and without applying a change output is done correctly for
// (N)P2WKH inputs, and should raise an error otherwise.
func TestCalculateFees(t *testing.T) {
	t.Parallel()

	const feeRate = chainfee.SatPerKWeight(1000)

	type testCase struct {
		name  string
		utxos []wallet.Coin

		expectedFeeNoChange   btcutil.Amount
		expectedFeeWithChange btcutil.Amount
		expectedErr           error
	}

	testCases := []testCase{
		{
			name: "one P2WKH input",
			utxos: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1,
					},
				},
			},

			expectedFeeNoChange:   487,
			expectedFeeWithChange: 659,
			expectedErr:           nil,
		},

		{
			name: "one NP2WKH input",
			utxos: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: np2wkhScript,
						Value:    1,
					},
				},
			},

			expectedFeeNoChange:   579,
			expectedFeeWithChange: 751,
			expectedErr:           nil,
		},

		{
			name: "not supported P2KH input",
			utxos: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2khScript,
						Value:    1,
					},
				},
			},

			expectedErr: &errUnsupportedInput{p2khScript},
		},
	}

	fundingOutputEstimate := input.TxWeightEstimator{}
	fundingOutputEstimate.AddP2WSHOutput()

	for _, test := range testCases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			feeNoChange, feeWithChange, err := calculateFees(
				test.utxos, feeRate, fundingOutputEstimate,
				defaultChanFundingChangeType,
			)
			require.Equal(t, test.expectedErr, err)

			// Note: The error-case will have zero values returned
			// for fees and therefore anyway pass the following
			// requirements.
			require.Equal(t, test.expectedFeeNoChange, feeNoChange)
			require.Equal(t, test.expectedFeeWithChange, feeWithChange)
		})
	}
}

// TestCoinSelect tests that we pick coins adding up to the expected amount
// when creating a funding transaction, and that the calculated change is the
// expected amount.
//
// NOTE: coinSelect will always attempt to add a change output (unless the
// ExistingChangeAddress change address type is selected), so we must account
// for this in the tests.
func TestCoinSelect(t *testing.T) {
	t.Parallel()

	const (
		feeRate   = chainfee.SatPerKWeight(100)
		dustLimit = btcutil.Amount(1000)
		fullCoin  = btcutil.SatoshiPerBitcoin
	)

	type testCase struct {
		name        string
		outputValue btcutil.Amount
		coins       []wallet.Coin
		changeType  ChangeAddressType

		expectedInput  []btcutil.Amount
		expectedChange btcutil.Amount
		expectErr      bool
	}

	testCases := []testCase{
		{
			// We have 1.0 BTC available, and wants to send 0.5.
			// This will obviously lead to a change output of
			// almost 0.5 BTC.
			name: "big change",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * fullCoin,
					},
				},
			},
			outputValue: 0.5 * fullCoin,
			changeType:  defaultChanFundingChangeType,

			// The one and only input will be selected.
			expectedInput: []btcutil.Amount{
				1 * fullCoin,
			},
			// Change will be what's left minus the fee.
			expectedChange: 0.5*fullCoin -
				fundingFee(feeRate, 1, true),
		},
		{
			// We have 1 BTC available, and we want to send 1 BTC.
			// This should lead to an error, as we don't have
			// enough funds to pay the fee.
			name: "nothing left for fees",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * fullCoin,
					},
				},
			},
			outputValue: 1 * fullCoin,
			changeType:  defaultChanFundingChangeType,

			expectErr: true,
		},
		{
			// We have a 1 BTC input, and want to create an output
			// as big as possible, such that the remaining change
			// would be dust but instead goes to fees.
			name: "dust change",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * fullCoin,
					},
				},
			},
			// We tune the output value by subtracting the expected
			// fee and the dust limit.
			outputValue: 1*fullCoin -
				fundingFee(feeRate, 1, false) - dustLimit,
			changeType: defaultChanFundingChangeType,

			expectedInput: []btcutil.Amount{
				1 * fullCoin,
			},

			// Change must be zero.
			expectedChange: 0,
		},
		{
			// We got just enough funds to create a change output
			// above the dust limit.
			name: "change right above dust limit",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value: int64(fundingFee(
							feeRate, 1, true,
						) + 2*(dustLimit+1)),
					},
				},
			},
			// We tune the output value to be just above the dust
			// limit.
			outputValue: dustLimit + 1,
			changeType:  defaultChanFundingChangeType,

			expectedInput: []btcutil.Amount{
				fundingFee(feeRate, 1, true) + 2*(dustLimit+1),
			},

			// After paying for the fee the change output should be
			// just above the dust limit.
			expectedChange: dustLimit + 1,
		},
		{
			// If more than 20% of funds goes to fees, it should
			// fail.
			name: "high fee",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value: int64(5 * fundingFee(
							feeRate, 1, false,
						)),
					},
				},
			},
			outputValue: 4 * fundingFee(feeRate, 1, false),
			changeType:  defaultChanFundingChangeType,

			expectErr: true,
		},
		{
			// Fees go to an existing change output.
			name: "existing change output",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value: 1000 + int64(fundingFee(
							feeRate, 1, false,
						)) + 1,
					},
				},
			},
			outputValue: 1000,
			changeType:  ExistingChangeAddress,

			expectedInput: []btcutil.Amount{
				1000 + fundingFee(feeRate, 1, false) + 1,
			},
			expectedChange: 1,
		},
	}

	fundingOutputEstimate := input.TxWeightEstimator{}
	fundingOutputEstimate.AddP2WSHOutput()

	for _, test := range testCases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			selected, changeAmt, err := CoinSelect(
				feeRate, test.outputValue, dustLimit,
				test.coins, wallet.CoinSelectionLargest,
				fundingOutputEstimate, test.changeType,
				DefaultMaxFeeRatio,
			)

			if test.expectErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			// Check that the selected inputs match what we expect.
			require.Len(t, selected, len(test.expectedInput))

			for i, coin := range selected {
				require.EqualValues(
					t, test.expectedInput[i], coin.Value,
				)
			}

			// Assert we got the expected change amount.
			require.EqualValues(t, test.expectedChange, changeAmt)
		})
	}
}

// TestCalculateChangeAmount tests that the change amount calculation performs
// correctly, taking into account the type of change output and whether we want
// to create a change output in the first place.
func TestCalculateChangeAmount(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		totalInputAmt btcutil.Amount
		requiredAmt   btcutil.Amount
		feeNoChange   btcutil.Amount
		feeWithChange btcutil.Amount
		dustLimit     btcutil.Amount
		changeType    ChangeAddressType
		maxFeeRatio   float64

		expectErr       string
		expectChangeAmt btcutil.Amount
		expectNeedMore  btcutil.Amount
	}{{
		// Coin selection returned a coin larger than the required
		// amount, but still not enough to pay for the fees. This should
		// trigger another round of coin selection with a larger
		// required amount.
		name:          "need to select more",
		totalInputAmt: 500,
		requiredAmt:   490,
		feeNoChange:   12,
		maxFeeRatio:   DefaultMaxFeeRatio,

		expectNeedMore: 502,
	}, {
		// We are using an existing change output, so we'll only want
		// to make sure to select enough for a TX _without_ a change
		// output added. Because we're using an existing output, the
		// dust limit calculation should also be skipped.
		name:          "sufficiently large for existing change output",
		totalInputAmt: 500,
		requiredAmt:   400,
		feeNoChange:   10,
		feeWithChange: 10,
		dustLimit:     100,
		changeType:    ExistingChangeAddress,
		maxFeeRatio:   DefaultMaxFeeRatio,

		expectChangeAmt: 90,
	}, {
		name:          "sufficiently large for adding a change output",
		totalInputAmt: 500,
		requiredAmt:   300,
		feeNoChange:   40,
		feeWithChange: 50,
		dustLimit:     100,
		maxFeeRatio:   DefaultMaxFeeRatio,

		expectChangeAmt: 150,
	}, {
		name: "sufficiently large for tx without change " +
			"amount",
		totalInputAmt: 500,
		requiredAmt:   460,
		feeNoChange:   40,
		feeWithChange: 50,
		maxFeeRatio:   DefaultMaxFeeRatio,

		expectChangeAmt: 0,
	}, {
		name:          "fee percent too large",
		totalInputAmt: 100,
		requiredAmt:   50,
		feeNoChange:   10,
		feeWithChange: 45,
		dustLimit:     5,
		maxFeeRatio:   DefaultMaxFeeRatio,

		expectErr: "fee 0.00000045 BTC exceeds max fee (0.00000011 " +
			"BTC) on total output value 0.00000055 BTC",
	}, {
		name:          "fee percent ok",
		totalInputAmt: 100,
		requiredAmt:   50,
		feeNoChange:   10,
		feeWithChange: 45,
		dustLimit:     5,
		maxFeeRatio:   0.95,

		expectChangeAmt: 5,
	}, {
		name:          "invalid max fee ratio",
		totalInputAmt: 100,
		requiredAmt:   50,
		feeNoChange:   10,
		feeWithChange: 45,
		dustLimit:     5,
		maxFeeRatio:   3.14,

		expectErr: "maxFeeRatio must be between 0.00 and 1.00",
	}, {
		name:          "invalid usage of function",
		feeNoChange:   5,
		feeWithChange: 10,
		changeType:    ExistingChangeAddress,
		maxFeeRatio:   DefaultMaxFeeRatio,

		expectErr: "fees for with or without change must be the same",
	}}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(tt *testing.T) {
			changeAmt, needMore, err := CalculateChangeAmount(
				tc.totalInputAmt, tc.requiredAmt,
				tc.feeNoChange, tc.feeWithChange, tc.dustLimit,
				tc.changeType, tc.maxFeeRatio,
			)

			if tc.expectErr != "" {
				require.ErrorContains(tt, err, tc.expectErr)
				return
			}

			require.EqualValues(tt, tc.expectChangeAmt, changeAmt)
			require.EqualValues(tt, tc.expectNeedMore, needMore)
		})
	}
}

// TestCoinSelectSubtractFees tests that we pick coins adding up to the
// expected amount when creating a funding transaction, and that a change
// output is created only when necessary.
func TestCoinSelectSubtractFees(t *testing.T) {
	t.Parallel()

	const feeRate = chainfee.SatPerKWeight(100)
	const highFeeRate = chainfee.SatPerKWeight(1000)
	const dustLimit = btcutil.Amount(1000)
	const dust = btcutil.Amount(100)

	// removeAmounts replaces any amounts in string with "<amt>".
	removeAmounts := func(s string) string {
		re := regexp.MustCompile(`[[:digit:]]+\.?[[:digit:]]*`)
		return re.ReplaceAllString(s, "<amt>")
	}

	type testCase struct {
		name       string
		highFee    bool
		spendValue btcutil.Amount
		coins      []wallet.Coin

		expectedInput      []btcutil.Amount
		expectedFundingAmt btcutil.Amount
		expectedChange     btcutil.Amount
		expectErr          string
	}

	testCases := []testCase{
		{
			// We have 1.0 BTC available, spend them all. This
			// should lead to a funding TX with one output, the
			// rest goes to fees.
			name: "spend all",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * btcutil.SatoshiPerBitcoin,
					},
				},
			},
			spendValue: 1 * btcutil.SatoshiPerBitcoin,

			// The one and only input will be selected.
			expectedInput: []btcutil.Amount{
				1 * btcutil.SatoshiPerBitcoin,
			},
			expectedFundingAmt: 1*btcutil.SatoshiPerBitcoin - fundingFee(feeRate, 1, false),
			expectedChange:     0,
		},
		{
			// We have 1.0 BTC available and spend half of it. This
			// should lead to a funding TX with a change output.
			name: "spend with change",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * btcutil.SatoshiPerBitcoin,
					},
				},
			},
			spendValue: 0.5 * btcutil.SatoshiPerBitcoin,

			// The one and only input will be selected.
			expectedInput: []btcutil.Amount{
				1 * btcutil.SatoshiPerBitcoin,
			},
			expectedFundingAmt: 0.5*btcutil.SatoshiPerBitcoin - fundingFee(feeRate, 1, true),
			expectedChange:     0.5 * btcutil.SatoshiPerBitcoin,
		},
		{
			// The total funds available is below the dust limit
			// after paying fees.
			name: "dust output",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    int64(fundingFee(feeRate, 1, false) + dustLimit - 1),
					},
				},
			},
			spendValue: fundingFee(feeRate, 1, false) + dust,

			expectErr: "output amount(<amt> BTC) after subtracting " +
				"fees(<amt> BTC) below dust limit(<amt> BTC)",
		},
		{
			// After subtracting fees, the resulting change output
			// is below the dust limit. The remainder should go
			// towards the funding output.
			name: "dust change",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * btcutil.SatoshiPerBitcoin,
					},
				},
			},
			spendValue: 1*btcutil.SatoshiPerBitcoin - dust,

			expectedInput: []btcutil.Amount{
				1 * btcutil.SatoshiPerBitcoin,
			},
			expectedFundingAmt: 1*btcutil.SatoshiPerBitcoin - fundingFee(feeRate, 1, false),
			expectedChange:     0,
		},
		{
			// We got just enough funds to create an output above the dust limit.
			name: "output right above dustlimit",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    int64(fundingFee(feeRate, 1, false) + dustLimit + 1),
					},
				},
			},
			spendValue: fundingFee(feeRate, 1, false) + dustLimit + 1,

			expectedInput: []btcutil.Amount{
				fundingFee(feeRate, 1, false) + dustLimit + 1,
			},
			expectedFundingAmt: dustLimit + 1,
			expectedChange:     0,
		},
		{
			// Amount left is below dust limit after paying fee for
			// a change output, resulting in a no-change tx.
			name: "no amount to pay fee for change",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    int64(fundingFee(feeRate, 1, false) + 2*(dustLimit+1)),
					},
				},
			},
			spendValue: fundingFee(feeRate, 1, false) + dustLimit + 1,

			expectedInput: []btcutil.Amount{
				fundingFee(feeRate, 1, false) + 2*(dustLimit+1),
			},
			expectedFundingAmt: 2 * (dustLimit + 1),
			expectedChange:     0,
		},
		{
			// If more than 20% of funds goes to fees, it should fail.
			name:    "high fee",
			highFee: true,
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    int64(5 * fundingFee(highFeeRate, 1, false)),
					},
				},
			},
			spendValue: 5 * fundingFee(highFeeRate, 1, false),

			expectErr: "fee <amt> BTC exceeds max fee (<amt> " +
				"BTC) on total output value <amt> BTC with " +
				"max fee ratio of <amt>",
		},
	}

	fundingOutputEstimate := input.TxWeightEstimator{}
	fundingOutputEstimate.AddP2WSHOutput()

	for _, test := range testCases {
		test := test

		t.Run(test.name, func(t *testing.T) {
			feeRate := feeRate
			if test.highFee {
				feeRate = highFeeRate
			}

			selected, localFundingAmt, changeAmt, err := CoinSelectSubtractFees(
				feeRate, test.spendValue, dustLimit, test.coins,
				wallet.CoinSelectionLargest,
				fundingOutputEstimate,
				defaultChanFundingChangeType,
				DefaultMaxFeeRatio,
			)
			if err != nil {
				switch {
				case test.expectErr == "":
					t.Fatal(err)

				case test.expectErr != removeAmounts(err.Error()):
					t.Fatalf("expected error '%v', got '%v'",
						test.expectErr,
						removeAmounts(err.Error()))

				// If we got an expected error, there is
				// nothing more to test.
				default:
					return
				}
			}

			// Check that there was no expected error we missed.
			if test.expectErr != "" {
				t.Fatalf("expected error")
			}

			// Check that the selected inputs match what we expect.
			if len(selected) != len(test.expectedInput) {
				t.Fatalf("expected %v inputs, got %v",
					len(test.expectedInput), len(selected))
			}

			for i, coin := range selected {
				if coin.Value != int64(test.expectedInput[i]) {
					t.Fatalf("expected input %v to have value %v, "+
						"had %v", i, test.expectedInput[i],
						coin.Value)
				}
			}

			// Assert we got the expected funding amount.
			if localFundingAmt != test.expectedFundingAmt {
				t.Fatalf("expected %v local funding amt, got %v",
					test.expectedFundingAmt, localFundingAmt)
			}

			// Assert we got the expected change amount.
			require.EqualValues(
				t, test.expectedChange, changeAmt,
			)
		})
	}
}

// TestCoinSelectUpToAmount tests that we pick coins adding up to the
// expected amount when creating a funding transaction, and that a change
// output is created only when necessary.
func TestCoinSelectUpToAmount(t *testing.T) {
	t.Parallel()

	const (
		feeRate   = chainfee.SatPerKWeight(100)
		dustLimit = btcutil.Amount(1000)
		dust      = btcutil.Amount(100)
		coin      = btcutil.SatoshiPerBitcoin
		minValue  = 20_000
	)

	type testCase struct {
		name     string
		minValue btcutil.Amount
		maxValue btcutil.Amount
		reserved btcutil.Amount
		coins    []wallet.Coin

		expectedInput      []btcutil.Amount
		expectedFundingAmt btcutil.Amount
		expectedChange     btcutil.Amount
		expectErr          string
	}

	testCases := []testCase{{
		// We have 1.0 BTC available, spend them all.
		// This should lead to a funding TX with one output, the rest
		// goes to fees.
		name: "spend exactly all",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * coin,
			},
		}},
		minValue: minValue,
		maxValue: 1 * coin,

		// The one and only input will be selected.
		expectedInput:      []btcutil.Amount{1 * coin},
		expectedFundingAmt: 1*coin - fundingFee(feeRate, 1, false),
		expectedChange:     0,
	}, {
		// We have 1.0 BTC available and want to spend up to 2 BTC.
		// This should lead to a funding TX with one output, the rest
		// goes to fees.
		name: "spend more",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * coin,
			},
		}},
		minValue: minValue,
		maxValue: 2 * coin,

		// The one and only input will be selected.
		expectedInput:      []btcutil.Amount{1 * coin},
		expectedFundingAmt: 1*coin - fundingFee(feeRate, 1, false),
		expectedChange:     0,
	}, {
		// We have 1.0 BTC available and want to spend up to 0.5 BTC.
		// This should lead to a funding TX with one output and a
		// change to subtract the fees from.
		name: "spend far below",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * coin,
			},
		}},
		minValue: minValue,
		maxValue: 0.5 * coin,

		// The one and only input will be selected.
		expectedInput:      []btcutil.Amount{1 * coin},
		expectedFundingAmt: 0.5 * coin,
		expectedChange:     0.5*coin - fundingFee(feeRate, 1, true),
	}, {
		// We have 1.0 BTC available and want to spend just 1 Satoshi
		// below that amount.
		// This should lead to a funding TX with one output where the
		// fee is subtracted from the total 1 BTC input value.
		name: "spend little below",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * coin,
			},
		}},
		minValue: minValue,
		maxValue: 1*coin - 1,

		// The one and only input will be selected.
		expectedInput: []btcutil.Amount{
			1 * coin,
		},
		expectedFundingAmt: 1*coin - fundingFee(feeRate, 1, false),
		expectedChange:     0,
	}, {
		// The total funds available is below the dust limit after
		// paying fees.
		name: "dust output",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value: int64(
					fundingFee(feeRate, 1, false) + dust,
				),
			},
		}},
		minValue: minValue,
		maxValue: fundingFee(feeRate, 1, false) + dust,

		expectErr: "output amount(0.00000100 BTC) after subtracting " +
			"fees(0.00000048 BTC) below dust limit(0.00001000 BTC)",
	}, {
		// If more than 20% of available wallet funds goes to fees, it
		// should fail.
		name: "high fee",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value: int64(
					20 * fundingFee(feeRate, 1, false),
				),
			},
		}},
		minValue: minValue,
		maxValue: 16 * fundingFee(feeRate, 1, false),

		expectErr: "fee 0.00000192 BTC exceeds max fee (0.00000153 " +
			"BTC) on total output value 0.00000768 BTC with max " +
			"fee ratio of 0.20",
	}, {
		// This test makes sure that the implementation detail of using
		// CoinSelect and CoinSelectSubtractFees is done correctly.
		// CoinSelect always defaults to use a fee for single input -
		// one change tx, whereas CoinSelectSubtractFees will use a fee
		// of single input - no change output, which without a sanity
		// check could result in a local amount higher than the maximum
		// amount that was expected.
		name: "sanity check for correct maximum amount",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * coin,
			},
		}},
		minValue: minValue,
		maxValue: 1*coin - fundingFee(feeRate, 1, false) - 1,

		expectedInput:      []btcutil.Amount{1 * coin},
		expectedFundingAmt: 1*coin - fundingFee(feeRate, 1, false) - 1,
		expectedChange:     0,
	}, {
		// This test makes sure that if a reserved value is required
		// then it is handled correctly by leaving exactly the reserved
		// value as change and still maxing out the funding amount.
		name: "sanity check for correct reserved amount subtract " +
			"from total",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * coin,
			},
		}},
		minValue: minValue,
		maxValue: 1*coin - 9000,
		reserved: 10000,

		expectedInput: []btcutil.Amount{1 * coin},
		expectedFundingAmt: 1*coin -
			fundingFee(feeRate, 1, true) - 10000,
		expectedChange: 10000,
	}}

	fundingOutputEstimate := input.TxWeightEstimator{}
	fundingOutputEstimate.AddP2WSHOutput()

	for _, test := range testCases {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selected, localFundingAmt, changeAmt,
				err := CoinSelectUpToAmount(
				feeRate, test.minValue, test.maxValue,
				test.reserved, dustLimit, test.coins,
				wallet.CoinSelectionLargest,
				fundingOutputEstimate,
				defaultChanFundingChangeType,
				DefaultMaxFeeRatio,
			)
			if len(test.expectErr) == 0 && err != nil {
				t.Fatal(err.Error())
			}
			if changeAmt != test.expectedChange {
				t.Fatalf("expected %v change amt, got %v",
					test.expectedChange, changeAmt)
			}
			if len(test.expectErr) > 0 && err == nil {
				t.Fatalf("expected error: %v", test.expectErr)
			}
			if len(test.expectErr) > 0 && err != nil {
				require.EqualError(t, err, test.expectErr)
			}

			// Check that the selected inputs match what we expect.
			require.Equal(t, len(test.expectedInput), len(selected))

			for i, coin := range selected {
				require.EqualValues(
					t, test.expectedInput[i], coin.Value,
				)
			}

			// Assert we got the expected funding amount.
			require.Equal(
				t, test.expectedFundingAmt, localFundingAmt,
			)
			// Assert we got the expected change amount.
			require.Equal(
				t, test.expectedChange, changeAmt,
			)
		})
	}
}
