package strayoutputpool

import (
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

const (
	// minCommitFeePerKw is the smallest fee rate that we should propose
	// for a new fee update. We'll use this as a fee floor when proposing
	// and accepting updates.
	minCommitFeePerKw = 253
)

type DBStrayOutputsPool struct {
	cfg *PoolConfig
}

// NewDBStrayOutputsPool instantiate StrayOutputsPool with implementation
// of storing serialised outputs to database
func NewDBStrayOutputsPool(config *PoolConfig) StrayOutputsPool {
	return &DBStrayOutputsPool{
		cfg: config,
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

	strayInputs, err := d.FetchAllStrayOutputs()
	if err != nil {
		return nil, err
	}

	return d.genSweepTx(pkScript, strayInputs...)
}

// commitFeePerKw returns fee rate floor based on the widely used consensus fee
// rate floors.
func (d *DBStrayOutputsPool) commitFeePerKw(numBlocks uint32) (lnwallet.SatPerKWeight, error) {
	feePerVSize, err := d.cfg.Estimator.EstimateFeePerVSize(numBlocks)
	if err != nil {
		return 0, err
	}

	// If we obtain a fee rate quote below this, then we should bump the
	// rate to ensure we've above the floor.
	commitFeePerKw := feePerVSize.FeePerKWeight()
	if commitFeePerKw < minCommitFeePerKw {
		log.Infof("Proposed fee rate of %v sat/kw is below min "+
			"of %v sat/kw, using fee floor", int64(commitFeePerKw),
			int64(minCommitFeePerKw))

		commitFeePerKw = minCommitFeePerKw
	}

	return commitFeePerKw, nil
}

// genSweepTx
func (d *DBStrayOutputsPool) genSweepTx(pkScript []byte,
	strayOutputs ...*strayOutputEntity) (*btcutil.Tx, error) {

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

	for _, sOutput := range strayOutputs {
		txFee := feePerVSize.FeeForVSize(sOutput.txVSize)
		totalAmt += sOutput.totalAmt - txFee

		// Add all spendable outputs to transaction
		for _, output := range sOutput.outputs {
			txn.AddTxIn(&wire.TxIn{
				PreviousOutPoint: *output.OutPoint(),
			})
		}

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

		// Generate a witness for each output of the transaction.
		for i, output := range sOutput.outputs {
			if err := addWitness(i, output); err != nil {
				return nil, err
			}
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