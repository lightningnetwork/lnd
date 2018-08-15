package strayoutputpool

import (
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcutil"

	"github.com/lightningnetwork/lnd/lnwallet"
)

// CutStrayInput cuts input that has less amount than current fee
// needed to mine it.
func CutStrayInput(spool StrayOutputsPool, feeRate lnwallet.SatPerVByte,
	input lnwallet.SpendableOutput) bool {
	var wEstimate lnwallet.TxWeightEstimator

	// We need to estimate also output, it is possible that all this inputs
	// could be swept separately.
	wEstimate.AddP2WKHOutput()

	vSize := wEstimate.AddWitnessInputByType(input.WitnessType()).VSize()

	isStrayInput := feeRate.FeeForVSize(int64(vSize)) > input.Amount()

	if isStrayInput {
		spool.AddSpendableOutput(input)
	}

	return isStrayInput
}

// CheckTransactionSanity does sanity check of transaction
func CheckTransactionSanity(tx *btcutil.Tx) error {
	return blockchain.CheckTransactionSanity(tx)
}
