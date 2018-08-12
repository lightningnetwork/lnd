package strayoutputpool

import (
	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/lightningnetwork/lnd/lnwallet"
)

// CutStrayInput cuts input that has less amount than current fee
// needed to mine it
func CutStrayInput(spool StrayOutputsPool, feeRate lnwallet.SatPerVByte,
	input lnwallet.SpendableOutput) bool {
	var wEstimate lnwallet.TxWeightEstimator

	vSize := wEstimate.AddWitnessInputByType(input.WitnessType()).VSize()
	isStrayInput := feeRate.FeeForVSize(int64(vSize)) > input.Amount()

	if isStrayInput {
		spool.AddSpendableOutput(input)
	}

	return isStrayInput
}

func (d *DBStrayOutputsPool) CheckTransactionSanity(tx *btcutil.Tx) error {
	var err error

	txInputs := tx.MsgTx().TxIn
	tx.MsgTx().TxIn = make([]*wire.TxIn, len(txInputs))
	prunedTxInputs := make([]*wire.TxIn, len(txInputs))

	var wEstimate lnwallet.TxWeightEstimator

	// we should get transaction by outpoint and calculate witness size

	// Our sweep transaction will pay to a single segwit p2wkh address,
	// ensure it contributes to our weight estimate.
	wEstimate.AddP2WKHOutput()

	//wEstimate.AddWitnessInput()
	//wEstimate.AddNestedP2WSHInput()
	wEstimate.AddP2PKHInput()
	wEstimate.AddNestedP2WKHInput()
	wEstimate.AddP2WKHInput()
	wEstimate.VSize()


	feePerVSize, err := d.cfg.Estimator.EstimateFeePerVSize(6)
	if err != nil {
		return err
	}

	feePerKw := feePerVSize.FeePerKWeight()

	_ = feePerKw.FeeForWeight(blockchain.GetTransactionWeight(tx))

	// Using a weight estimator, we'll compute the total
	// fee required, and from that the value we'll end up
	// with.
	totalFees := feePerVSize.FeeForVSize(int64(wEstimate.VSize()))


	// Amount of money that will be swept, if its negative this output is dust
	sweepAmt := 100 - int64(totalFees)
	// or
	if d.cfg.DustLimit >= totalFees {
		return errors.New("under dust limit")
	}

	_ = sweepAmt
	_ = prunedTxInputs


	tx.MsgTx().Command()


	return nil
}

// CheckTransactionSanity does sanity check of transaction and tries
// to find outputs which can be prune to satisfy validity, otherwise
// output must be stored for late broadcast.
func CheckTransactionSanity(tx *btcutil.Tx) error {
	var err error

	txInputs := tx.MsgTx().TxIn
	tx.MsgTx().TxIn = make([]*wire.TxIn, len(txInputs))
	prunedTxInputs := make([]*wire.TxIn, len(txInputs))

	var wEstimate lnwallet.TxWeightEstimator

	for _, txIn := range txInputs {

		outpoint := txIn.PreviousOutPoint

		_ = outpoint

		wEstimate.AddP2PKHInput()

		err = blockchain.CheckTransactionSanity(tx)
		if err != nil {
			// This output should be separated and stored locally until it
			// won'txIn be triggered by schedule.
			prunedTxInputs = append(prunedTxInputs, txIn)
			continue
		}

		// Simple checks validity of transaction with each outputs.
		tx.MsgTx().AddTxIn(txIn)
	}

	// Sanity check of composed transaction
	err = blockchain.CheckTransactionSanity(tx)

	if err != nil && len(tx.MsgTx().TxIn) == 0 {
		// Broadcasting must be ignored, because all outputs were pruned.
		return err
	}

	return nil
}
