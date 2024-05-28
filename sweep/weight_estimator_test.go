package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestWeightEstimator tests weight estimation for inputs with and without
// unconfirmed parents.
func TestWeightEstimator(t *testing.T) {
	testFeeRate := chainfee.SatPerKWeight(20000)

	w := newWeightEstimator(testFeeRate, 0)

	// Add an input without unconfirmed parent tx.
	input1 := input.MakeBaseInput(
		&wire.OutPoint{}, input.CommitmentAnchor,
		&input.SignDescriptor{}, 0, nil,
	)

	require.NoError(t, w.add(&input1))

	// The expectations is that this input is added.
	const expectedWeight1 lntypes.WeightUnit = 322
	require.Equal(t, expectedWeight1, w.weight())
	require.Equal(t, testFeeRate.FeeForWeight(expectedWeight1),
		w.feeWithParent())

	// Define a parent transaction that pays a fee of 30000 sat/kw.
	parentTxHighFee := &input.TxInfo{
		Weight: 100,
		Fee:    3000,
	}

	// Add an output of the parent tx above.
	input2 := input.MakeBaseInput(
		&wire.OutPoint{}, input.CommitmentAnchor,
		&input.SignDescriptor{}, 0,
		parentTxHighFee,
	)

	require.NoError(t, w.add(&input2))

	// Pay for parent isn't possible because the parent pays a higher fee
	// rate than the child. We expect no additional fee on the child.
	const expectedWeight2 lntypes.WeightUnit = expectedWeight1 + 280
	require.Equal(t, expectedWeight2, w.weight())
	require.Equal(t, testFeeRate.FeeForWeight(expectedWeight2),
		w.feeWithParent())

	// Define a parent transaction that pays a fee of 10000 sat/kw.
	parentTxLowFee := &input.TxInfo{
		Weight: 100,
		Fee:    1000,
	}

	// Add an output of the low-fee parent tx above.
	input3 := input.MakeBaseInput(
		&wire.OutPoint{}, input.CommitmentAnchor,
		&input.SignDescriptor{}, 0,
		parentTxLowFee,
	)
	require.NoError(t, w.add(&input3))

	// Expect the weight to increase because of the third input.
	const expectedWeight3 lntypes.WeightUnit = expectedWeight2 + 280
	require.Equal(t, expectedWeight3, w.weight())

	// Expect the fee to cover the child and the parent transaction at 20
	// sat/kw after subtraction of the fee that was already paid by the
	// parent.
	expectedFee := testFeeRate.FeeForWeight(
		expectedWeight3+parentTxLowFee.Weight,
	) - parentTxLowFee.Fee

	require.Equal(t, expectedFee, w.feeWithParent())
}

// TestWeightEstimatorMaxFee tests that the weight estimator correctly caps the
// fee at the maximum allowed fee.
func TestWeightEstimatorMaxFee(t *testing.T) {
	t.Parallel()

	testFeeRate := chainfee.SatPerKWeight(9_000)
	maxFeeRate := chainfee.SatPerKWeight(10_000)

	w := newWeightEstimator(testFeeRate, maxFeeRate)

	// Define a parent transaction that pays a fee of 1000 sat/kw.
	parentTxLowFee := &input.TxInfo{
		Weight: 100,
		Fee:    100,
	}

	// Add an output of the low-fee parent tx above.
	childInput := input.MakeBaseInput(
		&wire.OutPoint{}, input.CommitmentAnchor,
		&input.SignDescriptor{}, 0, parentTxLowFee,
	)
	require.NoError(t, w.add(&childInput))

	// The child weight should be 322 weight uints.
	const childWeight lntypes.WeightUnit = 322
	require.Equal(t, childWeight, w.weight())

	// Check the fee is capped at the maximum allowed fee. The
	// calculations,
	//
	// totalWeight = childWeight + parentWeight = 422
	// fee = totalWeight * testFeeRate - parentsFee =
	// 	422 * 9_000 / 1000 - 100 = 3598
	// maxFee = childWeight * maxFeeRate = 422 * 10_000 / 1000 = 3220
	//
	// Thus we cap at the maxFee.
	expectedFee := maxFeeRate.FeeForWeight(childWeight)
	require.Equal(t, expectedFee, w.feeWithParent())
}

// TestWeightEstimatorAddOutput tests that adding the raw P2WKH output to the
// estimator yield the same result as an estimated add.
func TestWeightEstimatorAddOutput(t *testing.T) {
	testFeeRate := chainfee.SatPerKWeight(20000)

	p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	p2wkhScript, err := txscript.PayToAddrScript(p2wkhAddr)
	require.NoError(t, err)

	// Create two estimators, add the raw P2WKH out to one.
	txOut := &wire.TxOut{
		PkScript: p2wkhScript,
		Value:    10000,
	}

	w1 := newWeightEstimator(testFeeRate, 0)
	w1.addOutput(txOut)

	w2 := newWeightEstimator(testFeeRate, 0)
	w2.addP2WKHOutput()

	// Estimate hhould be the same.
	require.Equal(t, w1.weight(), w2.weight())
}
