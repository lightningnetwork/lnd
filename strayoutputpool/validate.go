package strayoutputpool

import (
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/lightningnetwork/lnd/lnwallet"

)

func CutStrayInputs(pool StrayOutputsPool, tx *btcutil.Tx) error {
	var _ = pool
	return CheckTransactionSanity(tx)
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
