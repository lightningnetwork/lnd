package strayoutputpool

import (
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/strayoutputpool/store"
)

type DBStrayOutputsPool struct {
	cfg   *PoolConfig
	store store.OutputStore
}

// NewDBStrayOutputsPool instantiate StrayOutputsPool with implementation
// of storing serialised outputs to database
func NewDBStrayOutputsPool(config *PoolConfig) StrayOutputsPoolServer {
	return &DBStrayOutputsPool{
		cfg: config,
		store: store.NewOutputDB(config.DB),
	}
}

func (d *DBStrayOutputsPool) AddSpendableOutput(
	output lnwallet.SpendableOutput) error {

	return nil
}

// Sweep generates transaction for all added previously outputs to the wallet
// output address and broadcast it to the network
func (d *DBStrayOutputsPool) Sweep() error {
	btx, err := d.GenSweepTx()
	if err != nil {
		return err
	}

	return d.cfg.PublishTransaction(btx.MsgTx())
}

// GenSweepTx
func (d *DBStrayOutputsPool) GenSweepTx() (*btcutil.Tx, error) {
	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	pkScript, err := d.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	strayInputs, err := d.store.FetchAllStrayOutputs()
	if err != nil {
		return nil, err
	}

	return d.genSweepTx(pkScript, strayInputs...)
}

// genSweepTx
func (d *DBStrayOutputsPool) genSweepTx(pkScript []byte,
	strayOutputs ...store.OutputEntity) (*btcutil.Tx, error) {

	feePerVSize, err := d.cfg.Estimator.EstimateFeePerVSize(2)
	if err != nil {
		return nil, err
	}

	// With the fee calculated, we can now create the transaction using the
	// information gathered above and the provided retribution information.
	txn := wire.NewMsgTx(2)

	// Compute the total amount contained in all stored outputs
	// marked as strayed.
	var totalAmt btcutil.Amount

	hashCache := txscript.NewTxSigHashes(txn)

	addWitness := func(idx int, so lnwallet.SpendableOutput) error {
		// Generate witness for this outpoint and transaction.
		witness, err := so.BuildWitness(d.cfg.Signer, txn, hashCache, idx)
		if err != nil {
			return err
		}

		txn.TxIn[idx].Witness = witness

		return nil
	}

	for i, sOutput := range strayOutputs {
		txFee := feePerVSize.FeeForVSize(sOutput.TxVSize())
		totalAmt += sOutput.Output().Amount() - txFee

		// Add spendable outputs to transaction
		txn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *sOutput.Output().OutPoint(),
		})

		// Generate a witness for each output of the transaction.
		if err := addWitness(i, sOutput.Output()); err != nil {
			return nil, err
		}
	}

	txn.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    int64(totalAmt),
	})

	// Validate the transaction before signing
	btx := btcutil.NewTx(txn)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, err
	}

	return btx, nil
}

// Start is launches checking of swept outputs by interval into database.
// It must be run as a goroutine.
func (d *DBStrayOutputsPool) Start() error {
	return nil
}

// Stop is launches checking of swept outputs by interval into database.
func (d *DBStrayOutputsPool) Stop() {

}