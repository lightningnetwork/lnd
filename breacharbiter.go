package main

import (
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// breachArbiter is a special subsystem which is responsible for watching and
// acting on the detection of any attempted uncooperative channel breaches by
// channel counterparties. This file essentially acts as deterrence code for
// those attempting to launch attacks against the daemon. In practice it's
// expected that the logic in this file never gets executed, but it is
// important to have it in place just in case we encounter cheating channel
// counterparties.
// TODO(roasbeef): closures in config for subsystem pointers to decouple?
type breachArbiter struct {
	wallet     *lnwallet.LightningWallet
	db         *channeldb.DB
	notifier   chainntnfs.ChainNotifier
	htlcSwitch *htlcSwitch

	// breachObservers is a map which tracks all the active breach
	// observers we're currently managing. The key of the map is the
	// funding outpoint of the channel, and the value is a channel which
	// will be closed once we detect that the channel has been
	// cooperatively closed, thereby killing the goroutine and freeing up
	// resources.
	breachObservers map[wire.OutPoint]chan struct{}

	// breachedContracts is a channel which is used internally within the
	// struct to send the necessary information required to punish a
	// counterparty once a channel breach is detected. Breach observers
	// use this to communicate with the main contractObserver goroutine.
	breachedContracts chan *retributionInfo

	// newContracts is a channel which is used by outside subsystems to
	// notify the breachArbiter of a new contract (a channel) that should
	// be watched.
	newContracts chan *lnwallet.LightningChannel

	// settledContracts is a channel by outside subsystems to notify
	// the breachArbiter that a channel has peacefully been closed. Once a
	// channel has been closed the arbiter no longer needs to watch for
	// breach closes.
	settledContracts chan *wire.OutPoint

	started uint32
	stopped uint32
	quit    chan struct{}
	wg      sync.WaitGroup
}

// newBreachArbiter creates a new instance of a breachArbiter initialized with
// its dependent objects.
func newBreachArbiter(wallet *lnwallet.LightningWallet, db *channeldb.DB,
	notifier chainntnfs.ChainNotifier, h *htlcSwitch) *breachArbiter {

	return &breachArbiter{
		wallet:     wallet,
		db:         db,
		notifier:   notifier,
		htlcSwitch: h,

		breachObservers:   make(map[wire.OutPoint]chan struct{}),
		breachedContracts: make(chan *retributionInfo),
		newContracts:      make(chan *lnwallet.LightningChannel),
		settledContracts:  make(chan *wire.OutPoint),
		quit:              make(chan struct{}),
	}
}

// Start is an idempotent method that officially starts the breachArbiter along
// with all other goroutines it needs to perform its functions.
func (b *breachArbiter) Start() error {
	if !atomic.CompareAndSwapUint32(&b.started, 0, 1) {
		return nil
	}

	brarLog.Tracef("Starting breach arbiter")

	// First we need to query that database state for all currently active
	// channels, each of these channels will need a goroutine assigned to
	// it to watch for channel breaches.
	activeChannels, err := b.db.FetchAllChannels()
	if err != nil && err != channeldb.ErrNoActiveChannels {
		brarLog.Errorf("unable to fetch active channels: %v", err)
		return err
	}

	if len(activeChannels) > 0 {
		brarLog.Infof("Retrieved %v channels from database, watching "+
			"with vigilance!", len(activeChannels))
	}

	// For each of the channels read from disk, we'll create a channel
	// state machine in order to watch for any potential channel closures.
	channelsToWatch := make([]*lnwallet.LightningChannel,
		len(activeChannels))
	for i, chanState := range activeChannels {
		channel, err := lnwallet.NewLightningChannel(nil, b.notifier,
			chanState)
		if err != nil {
			brarLog.Errorf("unable to load channel from "+
				"disk: %v", err)
			return err
		}

		channelsToWatch[i] = channel
	}

	b.wg.Add(1)
	go b.contractObserver(channelsToWatch)

	return nil
}

// Stop is an idempotent method that signals the breachArbiter to execute a
// graceful shutdown. This function will block until all goroutines spawned by
// the breachArbiter have gracefully exited.
func (b *breachArbiter) Stop() error {
	if !atomic.CompareAndSwapUint32(&b.stopped, 0, 1) {
		return nil
	}

	brarLog.Infof("Breach arbiter shutting down")

	close(b.quit)
	b.wg.Wait()

	return nil
}

// contractObserver is the primary goroutine for the breachArbiter. This
// goroutine is responsible for managing goroutines that watch for breaches for
// all current active and newly created channels. If a channel breach is
// detected by a spawned child goroutine, then the contractObserver will
// execute the retribution logic required to sweep ALL outputs from a contested
// channel into the daemon's wallet.
//
// NOTE: This MUST be run as a goroutine.
func (b *breachArbiter) contractObserver(activeChannels []*lnwallet.LightningChannel) {
	defer b.wg.Done()

	// For each active channel found within the database, we launch a
	// detected breachObserver goroutine for that channel and also track
	// the new goroutine within the breachObservers map so we can cancel it
	// later if necessary.
	for _, channel := range activeChannels {
		settleSignal := make(chan struct{})
		chanPoint := channel.ChannelPoint()
		b.breachObservers[*chanPoint] = settleSignal

		b.wg.Add(1)
		go b.breachObserver(channel, settleSignal)
	}

out:
	for {
		select {
		case breachInfo := <-b.breachedContracts:
			// A new channel contract has just been breached! We
			// first register for a notification to be dispatched
			// once the breach transaction (the revoked commitment
			// transaction) has been confirmed in the chain to
			// ensure we're not dealing with a moving target.
			breachTXID := &breachInfo.commitHash
			confChan, err := b.notifier.RegisterConfirmationsNtfn(breachTXID, 1)
			if err != nil {
				brarLog.Errorf("unable to register for conf for txid: %v",
					breachTXID)
				continue
			}

			brarLog.Warnf("A channel has been breached with tx: %v. "+
				"Waiting for confirmation, then justice will be served!",
				breachTXID)

			// With the notification registered, we launch a new
			// goroutine which will finalize the channel
			// retribution after the breach transaction has been
			// confirmed.
			b.wg.Add(1)
			go b.exactRetribution(confChan, breachInfo)

			delete(b.breachObservers, breachInfo.chanPoint)
		case contract := <-b.newContracts:
			// A new channel has just been opened within the
			// daemon, so we launch a new breachObserver to handle
			// the detection of attempted contract breaches.
			settleSignal := make(chan struct{})
			chanPoint := contract.ChannelPoint()

			// If the contract is already being watched, then an
			// additional send indicates we have a stale version of
			// the contract. So we'll cancel active watcher
			// goroutine to create a new instance with the latest
			// contract reference.
			if oldSignal, ok := b.breachObservers[*chanPoint]; ok {
				brarLog.Infof("ChannelPoint(%v) is now live, "+
					"abandoning state contract for live "+
					"version", chanPoint)
				close(oldSignal)
			}

			b.breachObservers[*chanPoint] = settleSignal

			brarLog.Debugf("New contract detected, launching " +
				"breachObserver")

			b.wg.Add(1)
			go b.breachObserver(contract, settleSignal)

			// TODO(roasbeef): add doneChan to signal to peer continue
			//  * peer send over to us on loadActiveChanenls, sync
			//  until we're aware so no state transitions
		case chanPoint := <-b.settledContracts:
			// A new channel has been closed either unilaterally or
			// cooperatively, as a result we no longer need a
			// breachObserver detected to the channel.
			killSignal, ok := b.breachObservers[*chanPoint]
			if !ok {
				brarLog.Errorf("Unable to find contract: %v",
					chanPoint)
				continue
			}

			brarLog.Debugf("ChannelPoint(%v) has been settled, "+
				"cancelling breachObserver", chanPoint)

			// If we had a breachObserver active, then we signal it
			// for exit and also delete its state from our tracking
			// map.
			close(killSignal)
			delete(b.breachObservers, *chanPoint)
		case <-b.quit:
			break out
		}
	}

	return
}

// exactRetribution is a goroutine which is executed once a contract breach has
// been detected by a breachObserver. This function is responsible for
// punishing a counterparty for violating the channel contract by sweeping ALL
// the lingering funds within the channel into the daemon's wallet.
//
// NOTE: This MUST be run as a goroutine.
func (b *breachArbiter) exactRetribution(confChan *chainntnfs.ConfirmationEvent,
	breachInfo *retributionInfo) {

	defer b.wg.Done()

	// TODO(roasbeef): state needs to be checkpointed here

	select {
	case _, ok := <-confChan.Confirmed:
		// If the second value is !ok, then the channel has been closed
		// signifying a daemon shutdown, so we exit.
		if !ok {
			return
		}

		// Otherwise, if this is a real confirmation notification, then
		// we fall through to complete our duty.
	case <-b.quit:
		return
	}

	brarLog.Debugf("Breach transaction %v has been confirmed, sweeping "+
		"revoked funds", breachInfo.commitHash)

	// With the breach transaction confirmed, we now create the justice tx
	// which will claim ALL the funds within the channel.
	justiceTx, err := b.createJusticeTx(breachInfo)
	if err != nil {
		brarLog.Errorf("unable to create justice tx: %v", err)
		return
	}

	brarLog.Debugf("Broadcasting justice tx: %v", newLogClosure(func() string {
		return spew.Sdump(justiceTx)
	}))

	// Finally, broadcast the transaction, finalizing the channels'
	// retribution against the cheating counterparty.
	if err := b.wallet.PublishTransaction(justiceTx); err != nil {
		brarLog.Errorf("unable to broadcast "+
			"justice tx: %v", err)
		return
	}

	// As a conclusionary step, we register for a notification to be
	// dispatched once the justice tx is confirmed. After confirmation we
	// notify the caller that initiated the retribution workflow that the
	// deed has been done.
	justiceTXID := justiceTx.TxHash()
	confChan, err = b.notifier.RegisterConfirmationsNtfn(&justiceTXID, 1)
	if err != nil {
		brarLog.Errorf("unable to register for conf for txid: %v",
			justiceTXID)
		return
	}

	select {
	case _, ok := <-confChan.Confirmed:
		if !ok {
			return
		}

		// TODO(roasbeef): factor in HTLCs
		revokedFunds := breachInfo.revokedOutput.amt
		totalFunds := revokedFunds + breachInfo.selfOutput.amt

		brarLog.Infof("Justice for ChannelPoint(%v) has "+
			"been served, %v revoked funds (%v total) "+
			"have been claimed", breachInfo.chanPoint,
			revokedFunds, totalFunds)

		// TODO(roasbeef): add peer to blacklist?

		// TODO(roasbeef): close other active channels with offending peer

		close(breachInfo.doneChan)

		return
	case <-b.quit:
		return
	}
}

// breachObserver notifies the breachArbiter contract observer goroutine that a
// channel's contract has been breached by the prior counterparty. Once
// notified the breachArbiter will attempt to sweep ALL funds within the
// channel using the information provided within the BreachRetribution
// generated due to the breach of channel contract. The funds will be swept
// only after the breaching transaction receives a necessary number of
// confirmations.
func (b *breachArbiter) breachObserver(contract *lnwallet.LightningChannel,
	settleSignal chan struct{}) {

	defer b.wg.Done()

	chanPoint := contract.ChannelPoint()

	brarLog.Debugf("Breach observer for ChannelPoint(%v) started", chanPoint)

	select {
	// A read from this channel indicates that the contract has been
	// settled cooperatively so we exit as our duties are no longer needed.
	case <-settleSignal:
		contract.Stop()
		return

	// The channel has been closed by a normal means: force closing with
	// the latest commitment transaction.
	case closeInfo := <-contract.UnilateralClose:
		// Launch a goroutine to cancel out this contract within the
		// breachArbiter's main goroutine.
		go func() {
			b.settledContracts <- chanPoint
		}()

		// Next, we'll launch a goroutine to wait until the closing
		// transaction has been confirmed so we can mark the contract
		// as resolved in the database.
		//
		// TODO(roasbeef): also notify utxoNursery, might've had
		// outbound HTLC's in flight
		go waitForChanToClose(b.notifier, nil, chanPoint, closeInfo.SpenderTxHash, func() {
			brarLog.Infof("Force closed ChannelPoint(%v) is "+
				"fully closed, updating DB", chanPoint)

			if err := b.db.MarkChanFullyClosed(chanPoint); err != nil {
				brarLog.Errorf("unable to mark chan as closed: %v", err)
			}
		})

	// A read from this channel indicates that a channel breach has been
	// detected! So we notify the main coordination goroutine with the
	// information needed to bring the counterparty to justice.
	case breachInfo := <-contract.ContractBreach:
		brarLog.Warnf("REVOKED STATE #%v FOR ChannelPoint(%v) "+
			"broadcast, REMOTE PEER IS DOING SOMETHING "+
			"SKETCHY!!!", breachInfo.RevokedStateNum,
			chanPoint)

		// Immediately notify the HTLC switch that this link has been
		// breached in order to ensure any incoming or outgoing
		// multi-hop HTLCs aren't sent over this link, nor any other
		// links associated with this peer.
		b.htlcSwitch.CloseLink(chanPoint, CloseBreach)
		closeInfo := &channeldb.ChannelCloseSummary{
			ChanPoint:   *chanPoint,
			ClosingTXID: breachInfo.BreachTransaction.TxHash(),
			OurBalance:  contract.StateSnapshot().LocalBalance,
			IsPending:   true,
			CloseType:   channeldb.BreachClose,
		}
		if err := contract.DeleteState(closeInfo); err != nil {
			brarLog.Errorf("unable to delete channel state: %v", err)
		}

		// TODO(roasbeef): need to handle case of remote broadcast
		// mid-local initiated state-transition, possible false-positive?

		// First we generate the witness generation function which will
		// be used to sweep the output only we can satisfy on the
		// commitment transaction. This output is just a regular p2wkh
		// output.
		localSignDesc := breachInfo.LocalOutputSignDesc
		localWitness := func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
			inputIndex int) ([][]byte, error) {

			desc := localSignDesc
			desc.SigHashes = hc
			desc.InputIndex = inputIndex

			return lnwallet.CommitSpendNoDelay(b.wallet.Signer, desc, tx)
		}

		// Next we create the witness generation function that will be
		// used to sweep the cheating counterparty's output by taking
		// advantage of the revocation clause within the output's
		// witness script.
		remoteSignDesc := breachInfo.RemoteOutputSignDesc
		remoteWitness := func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
			inputIndex int) ([][]byte, error) {

			desc := breachInfo.RemoteOutputSignDesc
			desc.SigHashes = hc
			desc.InputIndex = inputIndex

			return lnwallet.CommitSpendRevoke(b.wallet.Signer, desc, tx)
		}

		// Finally, with the two witness generation funcs created, we
		// send the retribution information to the utxo nursery.
		// TODO(roasbeef): populate htlc breaches
		b.breachedContracts <- &retributionInfo{
			commitHash: breachInfo.BreachTransaction.TxHash(),
			chanPoint:  *chanPoint,

			selfOutput: &breachedOutput{
				amt:         btcutil.Amount(localSignDesc.Output.Value),
				outpoint:    breachInfo.LocalOutpoint,
				witnessFunc: localWitness,
			},

			revokedOutput: &breachedOutput{
				amt:         btcutil.Amount(remoteSignDesc.Output.Value),
				outpoint:    breachInfo.RemoteOutpoint,
				witnessFunc: remoteWitness,
			},

			doneChan: make(chan struct{}),
		}

	case <-b.quit:
		return
	}
}

// breachedOutput contains all the information needed to sweep a breached
// output. A breached output is an output that we are now entitled to due to a
// revoked commitment transaction being broadcast.
type breachedOutput struct {
	amt         btcutil.Amount
	outpoint    wire.OutPoint
	witnessFunc witnessGenerator

	twoStageClaim bool
}

// retributionInfo encapsulates all the data needed to sweep all the contested
// funds within a channel whose contract has been breached by the prior
// counterparty. This struct is used by the utxoNursery to create the justice
// transaction which spends all outputs of the commitment transaction into an
// output controlled by the wallet.
type retributionInfo struct {
	commitHash chainhash.Hash
	chanPoint  wire.OutPoint

	selfOutput *breachedOutput

	revokedOutput *breachedOutput

	htlcOutputs *[]breachedOutput

	doneChan chan struct{}
}

// createJusticeTx creates a transaction which exacts "justice" by sweeping ALL
// the funds within the channel which we are now entitled to due to a breach of
// the channel's contract by the counterparty. This function returns a *fully*
// signed transaction with the witness for each input fully in place.
func (b *breachArbiter) createJusticeTx(r *retributionInfo) (*wire.MsgTx, error) {
	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	// TODO(roasbeef): possibly create many outputs to minimize change in
	// the future?
	pkScriptOfJustice, err := newSweepPkScript(b.wallet)
	if err != nil {
		return nil, err
	}

	// Before creating the actual TxOut, we'll need to calculate the proper fee
	// to attach to the transaction to ensure a timely confirmation.
	// TODO(roasbeef): remove hard-coded fee
	totalAmt := r.selfOutput.amt + r.revokedOutput.amt
	sweepedAmt := int64(totalAmt - 5000)

	// With the fee calculated, we can now create the justice transaction
	// using the information gathered above.
	justiceTx := wire.NewMsgTx(2)
	justiceTx.AddTxOut(&wire.TxOut{
		PkScript: pkScriptOfJustice,
		Value:    sweepedAmt,
	})
	justiceTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: r.selfOutput.outpoint,
	})
	justiceTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: r.revokedOutput.outpoint,
	})

	hashCache := txscript.NewTxSigHashes(justiceTx)

	// Finally, using the witness generation functions attached to the
	// retribution information, we'll populate the inputs with fully valid
	// witnesses for both commitment outputs, and all the pending HTLCs at
	// this state in the channel's history.
	// TODO(roasbeef): handle the 2-layer HTLCs
	localWitness, err := r.selfOutput.witnessFunc(justiceTx, hashCache, 0)
	if err != nil {
		return nil, err
	}
	justiceTx.TxIn[0].Witness = localWitness

	remoteWitness, err := r.revokedOutput.witnessFunc(justiceTx, hashCache, 1)
	if err != nil {
		return nil, err
	}
	justiceTx.TxIn[1].Witness = remoteWitness

	return justiceTx, nil
}
