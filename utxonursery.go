package main

import (
	"sync"

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
//
// On a part-time basis, the utxoNursery also acts as an adjudicator in the
// scenario that we detect a peer breaching the contract of a channel by
// broadcasting a prior revoked state.
type utxoNursery struct {
	sync.RWMutex

	notifier chainntnfs.ChainNotifier
	wallet   *lnwallet.LightningWallet

	db channeldb.DB

	requests chan *incubationRequest

	// TODO(roasbeef): persist to disk afterwards
	unstagedOutputs map[wire.OutPoint]*immatureOutput
	stagedOutputs   map[uint32][]*immatureOutput

	breachedContracts chan *retributionInfo

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
		notifier:          notifier,
		wallet:            wallet,
		requests:          make(chan *incubationRequest),
		breachedContracts: make(chan *retributionInfo),
		unstagedOutputs:   make(map[wire.OutPoint]*immatureOutput),
		stagedOutputs:     make(map[uint32][]*immatureOutput),
		quit:              make(chan struct{}),
	}
}

// Start launches all goroutines the utxoNursery needs to properly carry out
// its duties.
func (u *utxoNursery) Start() error {
	u.wg.Add(1)
	go u.incubator()

	return nil
}

// Stop gracefully shutsdown any lingering goroutines launched during normal
// operation of the utxoNursery.
func (u *utxoNursery) Stop() error {
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
		case epoch := <-newBlocks.Epochs:
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
		case breachInfo := <-u.breachedContracts:
			// A new channel contract has just been breached! We
			// first register for a notification to be dispatched
			// once the breach transaction (the revoked commitment
			// transaction) has been confirmed in the chain to
			// ensure we're not dealing with a moving target.
			breachTXID := &breachInfo.commitHash
			confChan, err := u.notifier.RegisterConfirmationsNtfn(breachTXID, 1)
			if err != nil {
				utxnLog.Errorf("unable to register for conf for txid: ",
					breachTXID)
				continue
			}

			utxnLog.Infof("A channel has been breached with tx: %v. "+
				"Waiting for confirmation, then justice will be served!",
				breachTXID)

			// With the notification registered, we launch a new
			// goroutine which will finalize the channel
			// retribution after the breach transaction has been
			// confirmed.
			go func() {
				// If the second value is !ok, then the channel
				// has been closed signifying a daemon
				// shutdown, so we exit.
				if _, ok := <-confChan.Confirmed; !ok {
					// TODO(roasbeef): should check-point
					// state above
					return
				}

				utxnLog.Infof("Breach transaction %v has been "+
					"confirmed, sweeping revoked funds", breachTXID)

				// With the breach transaction confirmed, we
				// now create the justice tx which will claim
				// ALL the funds within the channel.
				justiceTx, err := u.createJusticeTx(breachInfo)
				if err != nil {
					utxnLog.Errorf("unable to create "+
						"justice tx: %v", err)
					return
				}

				utxnLog.Infof("Broadcasting justice tx: %v",
					newLogClosure(func() string {
						return spew.Sdump(justiceTx)
					}))

				// Finally, broadcast the transaction,
				// finalizing the channels' retribution against
				// the cheating counter-party.
				err = u.wallet.PublishTransaction(justiceTx)
				if err != nil {
					utxnLog.Errorf("unable to broadcast "+
						"justice tx: %v", err)
					return
				}

				// As a conclusionary step, we register for a
				// notification to be dispatched once the
				// justice tx is confirmed. After confirmation
				// we notify the caller that initiated the
				// retribution work low that the deed has been
				// done.
				justiceTXID := justiceTx.TxSha()
				confChan, err := u.notifier.RegisterConfirmationsNtfn(&justiceTXID, 1)
				if err != nil {
					utxnLog.Errorf("unable to register for conf for txid: ",
						justiceTXID)
					return
				}

				if _, ok := <-confChan.Confirmed; !ok {
					return
				}

				close(breachInfo.doneChan)
			}()
		case <-u.quit:
			break out
		}
	}

	u.wg.Done()
}

// newSweepPkScript creates a new public key script which should be used to
// sweep any time-locked, or contested channel funds into the wallet.
// Specifically, the script generted is a version 0, pay-to-witness-pubkey-hash
// (p2wkh) output.
func (u *utxoNursery) newSweepPkScript() ([]byte, error) {
	sweepAddr, err := u.wallet.NewAddress(lnwallet.WitnessPubKey, false)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(sweepAddr)
}

// createSweepTx creates a final sweeping transaction with all witnesses
// inplace for all inputs. The created transaction has a single output sending
// all the funds back to the source wallet.
func (u *utxoNursery) createSweepTx(matureOutputs []*immatureOutput) (*wire.MsgTx, error) {
	pkScript, err := u.newSweepPkScript()
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

// retributionInfo encapsulates all the data needed to sweep all the contested
// funds within a channel whose contract has been breached by the prior
// counter-party. This struct is used by the utxoNursery to create the justice
// transaction which spends all outputs of the commitment transaction into an
// output controlled by the wallet.
type retributionInfo struct {
	commitHash wire.ShaHash

	localAmt         btcutil.Amount
	localOutpoint    wire.OutPoint
	localWitnessFunc witnessGenerator

	remoteAmt        btcutil.Amount
	remoteOutpoint   wire.OutPoint
	remotWitnessFunc witnessGenerator

	htlcWitnessFuncs []witnessGenerator

	doneChan chan struct{}
}

// createJusticeTx creates a transaction which exacts "justice" by sweeping ALL
// the funds within the channel which we are now entitled to due to a breach of
// the channel's contract by the counter-party. This function returns a *fully*
// signed transaction with the witness for each input fully in place.
func (u *utxoNursery) createJusticeTx(r *retributionInfo) (*wire.MsgTx, error) {
	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	// TODO(roasbeef): possibly create many outputs to minimize change in
	// the future?
	pkScriptOfJustice, err := u.newSweepPkScript()
	if err != nil {
		return nil, err
	}

	// Before creating the actual TxOut, we'll need to calculate proper fee
	// to attach to the transaction to ensure a timely confirmation.
	// TODO(roasbeef): remove hard-coded fee
	totalAmt := r.localAmt + r.remoteAmt
	sweepedAmt := int64(totalAmt - 5000)

	// With the fee calculate, we can now create the justice transaction
	// using the information gathered above.
	justiceTx := wire.NewMsgTx()
	justiceTx.AddTxOut(&wire.TxOut{
		PkScript: pkScriptOfJustice,
		Value:    sweepedAmt,
	})
	justiceTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: r.localOutpoint,
	})
	justiceTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: r.remoteOutpoint,
	})

	hashCache := txscript.NewTxSigHashes(justiceTx)

	// Finally, using the witness generation functions attached to the
	// retribution information, we'll populate the inputs with fully valid
	// witnesses for both commitment outputs, and all the pending HTLC's at
	// this state in the channel's history.
	// TODO(roasbeef): handle the 2-layer HTLC's
	localWitness, err := r.localWitnessFunc(justiceTx, hashCache, 0)
	if err != nil {
		return nil, err
	}
	justiceTx.TxIn[0].Witness = localWitness

	remoteWitness, err := r.remotWitnessFunc(justiceTx, hashCache, 1)
	if err != nil {
		return nil, err
	}
	justiceTx.TxIn[1].Witness = remoteWitness

	return justiceTx, nil
}

// sweepRevokedFunds notifies the utxoNursery that a channel's contract has
// been breached by the prior counter party. Once notified the utxoNursery will
// attempt to sweep ALL funds within the channel using the information provided
// within the BreachRetribution generated due to the breach of channel
// contract. The funds will be swept only after the breaching transaction
// receives a necessary number of confirmations. A channel is immediately
// returned which will be closed once the funds have been successful swept into
// the wallet.
func (u *utxoNursery) sweepRevokedFunds(breachInfo *lnwallet.BreachRetribution) chan struct{} {
	// First we generate the witness generation function which will be used
	// to sweep the output only we can satisfy on the commitment
	// transaction. This output is just a regular p2wkh output.
	localSignDesc := breachInfo.LocalOutputSignDesc
	localWitness := func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
		inputIndex int) ([][]byte, error) {

		desc := localSignDesc
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		return lnwallet.CommitSpendNoDelay(u.wallet.Signer, desc, tx)
	}

	// Next we create the witness generation function that will be used to
	// sweep the cheating counter party's output by taking advantage of the
	// revocation clause within the output's witness script.
	remoteSignDesc := breachInfo.RemoteOutputSignDesc
	remoteWitness := func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
		inputIndex int) ([][]byte, error) {

		desc := breachInfo.RemoteOutputSignDesc
		desc.SigHashes = hc
		desc.InputIndex = inputIndex

		return lnwallet.CommitSpendRevoke(u.wallet.Signer, desc, tx)
	}

	// Finally, with the two witness generation funcs created, we send the
	// retribution information to the utxo nursery. The created doneChan
	// will be closed once the nursery sweeps all outputs inti the wallet.
	doneChan := make(chan struct{})
	u.breachedContracts <- &retributionInfo{
		commitHash: breachInfo.BreachTransaction.TxSha(),

		localAmt:         btcutil.Amount(localSignDesc.Output.Value),
		localOutpoint:    breachInfo.LocalOutpoint,
		localWitnessFunc: localWitness,

		remoteAmt:        btcutil.Amount(remoteSignDesc.Output.Value),
		remoteOutpoint:   breachInfo.RemoteOutpoint,
		remotWitnessFunc: remoteWitness,

		doneChan: doneChan,
	}

	return doneChan
}
