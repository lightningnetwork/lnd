package strayoutputpool

import (
	"github.com/lightningnetwork/lnd/lnwallet"
)

// CutStrayInput cuts input that has less amount than current fee
// needed to mine it.
func CutStrayInput(spool StrayOutputsPool, feeRate lnwallet.SatPerKWeight,
	input lnwallet.SpendableOutput) bool {
	isStrayInput := isNegativeAmount(feeRate, input)

	if isStrayInput {
		spool.AddSpendableOutput(input)
	}

	return isStrayInput
}

// isNegativeAmount verifies that input has negative amount
func isNegativeAmount(feeRate lnwallet.SatPerKWeight,
	input lnwallet.SpendableOutput) bool {
	var wEstimate lnwallet.TxWeightEstimator

	// We need to estimate also output, it is possible that all this inputs
	// could be swept separately.
	wEstimate.AddP2WKHOutput()

	// Weight of input according witness type.
	weight := wEstimate.AddWitnessInputByType(input.WitnessType()).Weight()

	return feeRate.FeeForWeight(int64(weight)) > input.Amount()
}
