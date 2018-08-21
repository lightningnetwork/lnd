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
	// checkDuration is duration when stray output pull tries to fetch all
	// stored spendable outputs and validate them with current fee rate and
	// create sweep transaction if output has positive amount compared
	// with fee needed to mine it.
	checkDuration = time.Hour

	// numConfs needs for estimation fee + monitoring transaction confirmations.
	numConfs = 6
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
	outputEntities, err := d.getOutputsToSweep()
	if err != nil {
		return err
	}

	// If we have no inputs we should ignore generation of sweep transaction.
	if len(outputEntities) == 0 {
		log.Debug("there are no spendable outputs ready to be swept")

		return nil
	}

	var outputs []lnwallet.SpendableOutput

	// Calculate base amount of transaction, needs only to show in
	// log message.
	var amount btcutil.Amount
	for _, oe := range outputEntities {
		outputs = append(outputs, oe.Output())
		amount += oe.Output().Amount()
	}

	// Generate transaction message with appropriate inputs.
	btx, err := d.GenSweepTx(outputs...)
	if err != nil {
		return err
	}

	log.Infof("publishing sweep transaction for a list of stray inputs with full amount: %v",
		amount)

	if err := d.cfg.PublishTransaction(btx.MsgTx()); err != nil && err != lnwallet.ErrDoubleSpend {
		log.Errorf("unable to broadcast sweep tx: %v, %v",
			err, spew.Sdump(btx.MsgTx()))

		return err
	}

	pub, err := d.store.PublishOutputs(btx, outputEntities)
	if err != nil {
		log.Errorf("couldn't move to 'Published' for outputs: %v",
			spew.Sdump(outputEntities))

		return err
	}

	_, bestHeight, err := d.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	return d.registerTimeoutConf(pub, uint32(bestHeight))
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

// getOutputsToSweep returns the list of spendable outputs that can be
// swept by current fee rate.
func (d *PoolServer) getOutputsToSweep() ([]store.OutputEntity, error) {
	oEntities, err := d.store.FetchAllStrayOutputs()
	if err != nil {
		return nil, err
	}

	feePerKW, err := d.cfg.Estimator.EstimateFeePerKW(numConfs)
	if err != nil {
		return nil, err
	}

	var outputsToSpend []store.OutputEntity
	for _, oe := range oEntities {
		if !isNegativeAmount(feePerKW, oe.Output()) {
			outputsToSpend = append(outputsToSpend, oe)
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

	feePerKW, err := d.cfg.Estimator.EstimateFeePerKW(numConfs)
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

// registerTimeoutConf is responsible for subscribing to confirmation
// notification for sweep transaction.
func (d *PoolServer) registerTimeoutConf(outputs store.PublishedOutputs,
	heightHint uint32) error {
	pkScript := outputs.Tx().TxOut[0].PkScript
	tx := btcutil.NewTx(outputs.Tx())

	// Register for the confirmation of published swept transaction.
	confChan, err := d.cfg.Notifier.RegisterConfirmationsNtfn(
		tx.Hash(), pkScript, numConfs,
		heightHint,
	)
	if err != nil {
		return err
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		select {
		case _, ok := <-confChan.Confirmed:
			if ok {
				if err := d.store.Sweep(outputs); err != nil {
					log.Errorf("couldn't mark outputs as swept: %v",
						err)
				}
			}

		case <-d.quit:
			return
		}
	}()

	return nil
}

// checkLastConfirmations restores confirmations watcher for already
// published transaction to the network with swept outputs.
func (d *PoolServer) checkLastConfirmations() error {
	pubOutputs, err := d.store.FetchPublishedOutputs()
	if err != nil {
		return nil
	}

	_, bestHeight, err := d.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	for _, pub := range pubOutputs {
		if err := d.registerTimeoutConf(pub, uint32(bestHeight)); err != nil {
			return err
		}
	}

	return nil
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
				if err := d.Sweep(); err != nil {
					log.Errorf("couldn't sweep outputs: %v",
						err)
				}

			case <-d.quit:
				return
			}
		}
	}()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		if err := d.checkLastConfirmations(); err != nil {
			log.Errorf("couldn't init checking last confirmation for published transactions: %v",
				err)
		}
	}()

	return nil
}

// Stop shutting down automatic sweeping function.
func (d *PoolServer) Stop() {
	if !atomic.CompareAndSwapUint32(&d.stopped, 0, 1) {
		return
	}

	log.Infof("StrayOutputPool shutting down")

	d.ticker.Stop()
	close(d.quit)
	d.wg.Wait()
}
