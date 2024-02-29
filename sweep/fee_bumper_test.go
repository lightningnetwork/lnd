package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

var (
	// Create  a taproot change script.
	changePkScript = []byte{
		0x51, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

// TestBumpResultValidate tests the validate method of the BumpResult struct.
func TestBumpResultValidate(t *testing.T) {
	t.Parallel()

	// An empty result will give an error.
	b := BumpResult{}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// Unknown event type will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: sentinalEvent,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A replacing event without a new tx will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxReplaced,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A failed event without a failure reason will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxFailed,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A confirmed event without fee info will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxConfirmed,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// Test a valid result.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxPublished,
	}
	require.NoError(t, b.Validate())
}

// TestCalcSweepTxWeight checks that the weight of the sweep tx is calculated
// correctly.
func TestCalcSweepTxWeight(t *testing.T) {
	t.Parallel()

	// Create an input.
	inp := createTestInput(100, input.WitnessKeyHash)

	// Use a wrong change script to test the error case.
	weight, err := calcSweepTxWeight([]input.Input{&inp}, []byte{0})
	require.Error(t, err)
	require.Zero(t, weight)

	// Use a correct change script to test the success case.
	weight, err = calcSweepTxWeight([]input.Input{&inp}, changePkScript)
	require.NoError(t, err)

	// BaseTxSize 8 bytes
	// InputSize 1+41 bytes
	// One P2TROutputSize 1+43 bytes
	// One P2WKHWitnessSize 2+109 bytes
	// Total weight = (8+42+44) * 4 + 111 = 487
	require.EqualValuesf(t, 487, weight, "unexpected weight %v", weight)
}

// TestBumpRequestMaxFeeRateAllowed tests the max fee rate allowed for a bump
// request.
func TestBumpRequestMaxFeeRateAllowed(t *testing.T) {
	t.Parallel()

	// Create a test input.
	inp := createTestInput(100, input.WitnessKeyHash)

	// The weight is 487.
	weight, err := calcSweepTxWeight([]input.Input{&inp}, changePkScript)
	require.NoError(t, err)

	// Define a test budget and calculates its fee rate.
	budget := btcutil.Amount(1000)
	budgetFeeRate := chainfee.NewSatPerKWeight(budget, weight)

	testCases := []struct {
		name               string
		req                *BumpRequest
		expectedMaxFeeRate chainfee.SatPerKWeight
		expectedErr        bool
	}{
		{
			// Use a wrong change script to test the error case.
			name: "error calc weight",
			req: &BumpRequest{
				DeliveryAddress: []byte{1},
			},
			expectedMaxFeeRate: 0,
			expectedErr:        true,
		},
		{
			// When the budget cannot give a fee rate that matches
			// the supplied MaxFeeRate, the max allowed feerate is
			// capped by the budget.
			name: "use budget as max fee rate",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
				Budget:          budget,
				MaxFeeRate:      budgetFeeRate + 1,
			},
			expectedMaxFeeRate: budgetFeeRate,
		},
		{
			// When the budget can give a fee rate that matches the
			// supplied MaxFeeRate, the max allowed feerate is
			// capped by the MaxFeeRate.
			name: "use config as max fee rate",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
				Budget:          budget,
				MaxFeeRate:      budgetFeeRate - 1,
			},
			expectedMaxFeeRate: budgetFeeRate - 1,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Check the method under test.
			maxFeeRate, err := tc.req.MaxFeeRateAllowed()

			// If we expect an error, check the error is returned
			// and the feerate is empty.
			if tc.expectedErr {
				require.Error(t, err)
				require.Zero(t, maxFeeRate)

				return
			}

			// Otherwise, check the max fee rate is as expected.
			require.NoError(t, err)
			require.Equal(t, tc.expectedMaxFeeRate, maxFeeRate)
		})
	}
}
