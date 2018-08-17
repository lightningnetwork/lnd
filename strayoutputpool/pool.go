package strayoutputpool

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/strayoutputpool/store"
)

const (
	// Duration when stray output pull tries to fetch all stored spendable
	// outputs and validate them with current fee rate and create sweep
	// transaction if output has positive amount compared with fee needed
	// to mine it.
	checkDuration = time.Hour
)

// PoolServer is pool which contains a list of stray outputs that
// can be manually or automatically swept back into wallet.
type PoolServer struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg    *PoolConfig
	store  store.OutputStore
	ticker *time.Ticker

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewPoolServer instantiate StrayOutputsPool with implementation
// of storing serialised outputs to database.
func NewPoolServer(config *PoolConfig) StrayOutputsPoolServer {
	return &PoolServer{
		cfg:   config,
		store: store.NewOutputDB(config.DB),
		quit:  make(chan struct{}),
	}
}

// AddSpendableOutput adds spendable output to stray outputs pool.
func (d *PoolServer) AddSpendableOutput(
	output lnwallet.SpendableOutput) error {
	return d.store.AddStrayOutput(
		store.NewOutputEntity(output),
	)
}

// Sweep generates transaction for all added previously outputs to the wallet
// output address and broadcast it to the network.
func (d *PoolServer) Sweep() error {
	// Retrieve all stray outputs that can be swept back to the wallet,
	// for all of them we need to recalculate fee based on current fee
	// rate in time of triggering sweeping function.
	strayInputs, err := d.getSpendableOutputs()
	if err != nil {
		return err
	}

	// If we have no inputs we should ignore generation of sweep transaction.
	if len(strayInputs) == 0 {
		log.Debug("there are no spendable outputs ready to be swept")

		return nil
	}

	// Generate transaction message with appropriate inputs.
	btx, err := d.GenSweepTx(strayInputs...)
	if err != nil {
		return err
	}

	// Calculate base amount of transaction, needs only to show in
	// log message.
	var amount btcutil.Amount
	for _, input := range strayInputs {
		amount += input.Amount()
	}

	log.Infof("publishing sweep transaction for a list of stray inputs with full amount: %v",
		amount)

	err = d.cfg.PublishTransaction(btx.MsgTx())
	if err != nil && err != lnwallet.ErrDoubleSpend {
		log.Errorf("unable to broadcast sweep tx: %v, %v",
			err, spew.Sdump(btx.MsgTx()))

		return err
	}

	return err
}

// GenSweepTx fetches all stray outputs from database and
// generates sweep transaction for them.
func (d *PoolServer) GenSweepTx(strayInputs ...lnwallet.SpendableOutput) (*btcutil.Tx, error) {
	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	pkScript, err := d.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	return d.genSweepTx(pkScript, strayInputs...)
}

// getSpendableOutputs returns the list of spendable outputs that are ready
// now to be swept back into wallet.
func (d *PoolServer) getSpendableOutputs() ([]lnwallet.SpendableOutput, error) {
	oEntities, err := d.store.FetchAllStrayOutputs()
	if err != nil {
		return nil, err
	}

	feePerKW, err := d.cfg.Estimator.EstimateFeePerKW(6)
	if err != nil {
		return nil, err
	}

	var outputsToSpend []lnwallet.SpendableOutput
	for _, oe := range oEntities {
		if !isNegativeAmount(feePerKW, oe.Output()) {
			outputsToSpend = append(outputsToSpend, oe.Output())
		}
	}

	return outputsToSpend, nil
}

// genSweepTx generates sweep transaction for the list of stray outputs.
func (d *PoolServer) genSweepTx(pkScript []byte,
	strayOutputs ...lnwallet.SpendableOutput) (*btcutil.Tx, error) {
	// Compute the total amount contained in all stored outputs
	// marked as strayed.
	var (
		totalAmt    btcutil.Amount
		txEstimator lnwallet.TxWeightEstimator
	)

	feePerKW, err := d.cfg.Estimator.EstimateFeePerKW(6)
	if err != nil {
		return nil, err
	}

	// With the fee calculated, we can now create the transaction using the
	// information gathered above and the provided retribution information.
	txn := wire.NewMsgTx(2)

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

	// Add standard output to our wallet.
	txEstimator.AddP2WKHOutput()

	for i, sOutput := range strayOutputs {
		txEstimator.AddWitnessInputByType(sOutput.WitnessType())

		totalAmt += sOutput.Amount()

		// Add spendable outputs to transaction.
		txn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *sOutput.OutPoint(),
		})

		// Generate a witness for each output of the transaction.
		if err := addWitness(i, sOutput); err != nil {
			return nil, err
		}
	}

	txFee := feePerKW.FeeForWeight(int64(txEstimator.Weight()))

	txn.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    int64(totalAmt - txFee),
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
func (d *PoolServer) Start() error {
	if !atomic.CompareAndSwapUint32(&d.started, 0, 1) {
		return nil
	}

	log.Tracef("Starting StrayOutputPool")

	d.ticker = time.NewTicker(checkDuration)

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.ticker.C:
				d.Sweep()

			case <-d.quit:
				return
			}
		}
	}()

	return nil
}

// Stop is launches checking of swept outputs by interval into database.
func (d *PoolServer) Stop() {
	if !atomic.CompareAndSwapUint32(&d.stopped, 0, 1) {
		return
	}

	log.Infof("StrayOutputPool is shutting down")

	d.ticker.Stop()
	close(d.quit)
	d.wg.Wait()
}
