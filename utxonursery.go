package main

import (
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// utxoNursery is a system dedicated to incubating time-locked outputs created
// by the broadcast of a commitment transaction either by us, or the remote
// peer. The nursery accepts outputs and "incubates" them until they've reached
// maturity, then sweep the outputs into the source wallet. An output is
// considered mature after the relative time-lock within the pkScript has
// passed. As outputs reach their maturity age, they're swept in batches into
// the source wallet, returning the outputs so they can be used within future
// channels, or regular Bitcoin transactions.
type utxoNursery struct {
	sync.RWMutex

	notifier chainntnfs.ChainNotifier
	wallet   *lnwallet.LightningWallet

	db channeldb.DB

	requests chan *incubationRequest

	// TODO(roasbeef): persist to disk afterwards
	unstagedOutputs map[wire.OutPoint]*immatureOutput
	stagedOutputs   map[uint32][]*immatureOutput

	started uint32
	stopped uint32
	quit    chan struct{}
	wg      sync.WaitGroup
}

// newUtxoNursery creates a new instance of the utxoNursery from a
// ChainNotifier and LightningWallet instance.
func newUtxoNursery(notifier chainntnfs.ChainNotifier,
	wallet *lnwallet.LightningWallet) *utxoNursery {

	return &utxoNursery{
		notifier:        notifier,
		wallet:          wallet,
		requests:        make(chan *incubationRequest),
		unstagedOutputs: make(map[wire.OutPoint]*immatureOutput),
		stagedOutputs:   make(map[uint32][]*immatureOutput),
		quit:            make(chan struct{}),
	}
}

// Start launches all goroutines the utxoNursery needs to properly carry out
// its duties.
func (u *utxoNursery) Start() error {
	if !atomic.CompareAndSwapUint32(&u.started, 0, 1) {
		return nil
	}

	utxnLog.Tracef("Starting UTXO nursery")

	u.wg.Add(1)
	go u.incubator()

	return nil
}

// Stop gracefully shutsdown any lingering goroutines launched during normal
// operation of the utxoNursery.
func (u *utxoNursery) Stop() error {
	if !atomic.CompareAndSwapUint32(&u.stopped, 0, 1) {
		return nil
	}

	utxnLog.Infof("UTXO nursery shutting down")

	close(u.quit)
	u.wg.Wait()

	return nil
}

// incubator is tasked with watching over all immature outputs until they've
// reached "maturity", after which they'll be swept into the underlying wallet
// in batches within a single transaction. Immature outputs can be divided into
// three stages: early stage, mid stage, and final stage. During the early
// stage, the transaction containing the output has not yet been confirmed.
// Once the txn creating the output is confirmed, then output moves to the mid
// stage wherein a dedicated goroutine waits until it has reached "maturity".
// Once an output is mature, it will be swept into the wallet at the earlier
// possible height.
func (u *utxoNursery) incubator() {
	defer u.wg.Done()

	// Register with the notifier to receive notifications for each newly
	// connected block.
	newBlocks, err := u.notifier.RegisterBlockEpochNtfn()
	if err != nil {
		utxnLog.Errorf("unable to register for block epoch "+
			"notifications: %v", err)
	}

	// Outputs that are transitioning from early to mid-stage are sent over
	// this channel by each output's dedicated watcher goroutine.
	midStageOutputs := make(chan *immatureOutput)
out:
	for {
		select {
		case earlyStagers := <-u.requests:
			utxnLog.Infof("Incubating %v new outputs",
				len(earlyStagers.outputs))

			for _, immatureUtxo := range earlyStagers.outputs {
				outpoint := immatureUtxo.outPoint
				sourceTXID := outpoint.Hash

				// Register for a confirmation once the
				// generating txn has been confirmed.
				confChan, err := u.notifier.RegisterConfirmationsNtfn(&sourceTXID, 1)
				if err != nil {
					utxnLog.Errorf("unable to register for confirmations "+
						"for txid: %v", sourceTXID)
					continue
				}

				// TODO(roasbeef): should be an on-disk
				// at-least-once task queue
				u.unstagedOutputs[outpoint] = immatureUtxo

				// Launch a dedicated goroutine which will send
				// the output back to the incubator once the
				// source txn has been confirmed.
				go func() {
					confHeight, ok := <-confChan.Confirmed
					if !ok {
						utxnLog.Errorf("notification chan "+
							"closed, can't advance output %v", outpoint)
						return
					}

					utxnLog.Infof("Outpoint %v confirmed in "+
						"block %v moving to mid-stage",
						outpoint, confHeight)
					immatureUtxo.confHeight = uint32(confHeight)
					midStageOutputs <- immatureUtxo
				}()
			}
			// TODO(roasbeef): rename to preschool and kindergarden
		case midUtxo := <-midStageOutputs:
			// The transaction creating the output has been
			// created, so we move it from early stage to
			// mid-stage.
			delete(u.unstagedOutputs, midUtxo.outPoint)

			// TODO(roasbeef): your off-by-one sense are tingling...
			maturityHeight := midUtxo.confHeight + midUtxo.blocksToMaturity
			u.stagedOutputs[maturityHeight] = append(u.stagedOutputs[maturityHeight], midUtxo)

			utxnLog.Infof("Outpoint %v now mid-stage, will mature "+
				"at height %v (delay of %v)", midUtxo.outPoint,
				maturityHeight, midUtxo.blocksToMaturity)
		case epoch, ok := <-newBlocks.Epochs:
			// If the epoch channel has been closed, then the
			// ChainNotifier is exiting which means the daemon is
			// as well. Therefore, we exit early also in order to
			// ensure the daemon shutsdown gracefully, yet swiftly.
			if !ok {
				return
			}

			// A new block has just been connected, check to see if
			// we have any new outputs that can be swept into the
			// wallet.
			newHeight := uint32(epoch.Height)
			matureOutputs, ok := u.stagedOutputs[newHeight]
			if !ok {
				continue
			}

			utxnLog.Infof("New block: height=%v hash=%v, "+
				"sweeping %v mature outputs", newHeight,
				epoch.Hash, len(matureOutputs))

			// Create a transation which sweeps all the newly
			// mature outputs into a output controlled by the
			// wallet.
			// TODO(roasbeef): can be more intelligent about
			// buffering outputs to be more efficient on-chain.
			sweepTx, err := u.createSweepTx(matureOutputs)
			if err != nil {
				// TODO(roasbeef): retry logic?
				utxnLog.Errorf("unable to create sweep tx: %v", err)
				continue
			}

			utxnLog.Infof("Sweeping %v time-locked outputs "+
				"with sweep tx: %v", len(matureOutputs),
				newLogClosure(func() string {
					return spew.Sdump(sweepTx)
				}))

			// With the sweep transaction fully signed, broadcast
			// the transaction to the network. Additionally, we can
			// stop tracking these outputs as they've just been
			// sweeped.
			err = u.wallet.PublishTransaction(sweepTx)
			if err != nil {
				utxnLog.Errorf("unable to broadcast sweep tx: %v, %v",
					err, spew.Sdump(sweepTx))
				continue
			}
			delete(u.stagedOutputs, newHeight)
		case <-u.quit:
			break out
		}
	}
}

// createSweepTx creates a final sweeping transaction with all witnesses in
// place for all inputs. The created transaction has a single output sending
// all the funds back to the source wallet.
func (u *utxoNursery) createSweepTx(matureOutputs []*immatureOutput) (*wire.MsgTx, error) {
	pkScript, err := newSweepPkScript(u.wallet)
	if err != nil {
		return nil, err
	}

	var totalSum btcutil.Amount
	for _, o := range matureOutputs {
		totalSum += o.amt
	}

	sweepTx := wire.NewMsgTx()
	sweepTx.Version = 2
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    int64(totalSum - 5000),
	})
	for _, utxo := range matureOutputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: utxo.outPoint,
			// TODO(roasbeef): assumes pure block delays
			Sequence: utxo.blocksToMaturity,
		})
	}

	// TODO(roasbeef): insert fee calculation
	//  * remove hardcoded fee above

	// With all the inputs in place, use each output's unique witness
	// function to generate the final witness required for spending.
	hashCache := txscript.NewTxSigHashes(sweepTx)
	for i, txIn := range sweepTx.TxIn {
		witness, err := matureOutputs[i].witnessFunc(sweepTx, hashCache, i)
		if err != nil {
			return nil, err
		}

		txIn.Witness = witness
	}

	return sweepTx, nil
}

// witnessGenerator represents a function which is able to generate the final
// witness for a particular public key script. This function acts as an
// abstraction layer, hiding the details of the underlying script from the
// utxoNursery.
type witnessGenerator func(tx *wire.MsgTx, hc *txscript.TxSigHashes, inputIndex int) ([][]byte, error)

// immatureOutput encapsulates an immature output. The struct includes a
// witnessGenerator closure which will be used to generate the witness required
// to sweep the output once it's mature.
// TODO(roasbeef): make into interface?  can't gob functions
type immatureOutput struct {
	amt      btcutil.Amount
	outPoint wire.OutPoint

	witnessFunc witnessGenerator

	// TODO(roasbeef): using block timeouts everywhere currently, will need
	// to modify logic later to account for MTP based timeouts.
	blocksToMaturity uint32
	confHeight       uint32
}

// incubationRequest is a request to the utxoNursery to incubate a set of
// outputs until their mature, finally sweeping them into the wallet once
// available.
type incubationRequest struct {
	outputs []*immatureOutput
}

// incubateOutputs sends a request to utxoNursery to incubate the outputs
// defined within the summary of a closed channel. Individually, as all outputs
// reach maturity they'll be swept back into the wallet.
func (u *utxoNursery) incubateOutputs(closeSummary *lnwallet.ForceCloseSummary) {
	// TODO(roasbeef): should use factory func here based on an interface
	//  * interface type stored on disk next to record
	//  * spend here also assumes delay is blocked bsaed, and in range
	witnessFunc := func(tx *wire.MsgTx, hc *txscript.TxSigHashes, inputIndex int) ([][]byte, error) {
		desc := closeSummary.SelfOutputSignDesc
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		return lnwallet.CommitSpendTimeout(u.wallet.Signer, desc, tx)
	}

	outputAmt := btcutil.Amount(closeSummary.SelfOutputSignDesc.Output.Value)
	selfOutput := &immatureOutput{
		amt:              outputAmt,
		outPoint:         closeSummary.SelfOutpoint,
		witnessFunc:      witnessFunc,
		blocksToMaturity: closeSummary.SelfOutputMaturity,
	}

	u.requests <- &incubationRequest{
		outputs: []*immatureOutput{selfOutput},
	}
}

// newSweepPkScript creates a new public key script which should be used to
// sweep any time-locked, or contested channel funds into the wallet.
// Specifically, the script generated is a version 0,
// pay-to-witness-pubkey-hash (p2wkh) output.
func newSweepPkScript(wallet lnwallet.WalletController) ([]byte, error) {
	sweepAddr, err := wallet.NewAddress(lnwallet.WitnessPubKey, false)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(sweepAddr)
}
