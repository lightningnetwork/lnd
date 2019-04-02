package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	// retributionBucket stores retribution state on disk between detecting
	// a contract breach, broadcasting a justice transaction that sweeps the
	// channel, and finally witnessing the justice transaction confirm on
	// the blockchain. It is critical that such state is persisted on disk,
	// so that if our node restarts at any point during the retribution
	// procedure, we can recover and continue from the persisted state.
	retributionBucket = []byte("retribution")

	// justiceTxnBucket holds the finalized justice transactions for all
	// breached contracts. Entries are added to the justice txn bucket just
	// before broadcasting the sweep txn.
	justiceTxnBucket = []byte("justice-txn")

	// errBrarShuttingDown is an error returned if the breacharbiter has
	// been signalled to exit.
	errBrarShuttingDown = errors.New("breacharbiter shutting down")
)

// ContractBreachEvent is an event the breachArbiter will receive in case a
// contract breach is observed on-chain. It contains the necessary information
// to handle the breach, and a ProcessACK channel we will use to ACK the event
// when we have safely stored all the necessary information.
type ContractBreachEvent struct {
	// ChanPoint is the channel point of the breached channel.
	ChanPoint wire.OutPoint

	// ProcessACK is an error channel where a nil error should be sent
	// iff the breach retribution info is safely stored in the retribution
	// store. In case storing the information to the store fails, a non-nil
	// error should be sent.
	ProcessACK chan error

	// BreachRetribution is the information needed to act on this contract
	// breach.
	BreachRetribution *lnwallet.BreachRetribution
}

// BreachConfig bundles the required subsystems used by the breach arbiter. An
// instance of BreachConfig is passed to newBreachArbiter during instantiation.
type BreachConfig struct {
	// CloseLink allows the breach arbiter to shutdown any channel links for
	// which it detects a breach, ensuring now further activity will
	// continue across the link. The method accepts link's channel point and
	// a close type to be included in the channel close summary.
	CloseLink func(*wire.OutPoint, htlcswitch.ChannelCloseType)

	// DB provides access to the user's channels, allowing the breach
	// arbiter to determine the current state of a user's channels, and how
	// it should respond to channel closure.
	DB *channeldb.DB

	// Estimator is used by the breach arbiter to determine an appropriate
	// fee level when generating, signing, and broadcasting sweep
	// transactions.
	Estimator lnwallet.FeeEstimator

	// GenSweepScript generates the receiving scripts for swept outputs.
	GenSweepScript func() ([]byte, error)

	// Notifier provides a publish/subscribe interface for event driven
	// notifications regarding the confirmation of txids.
	Notifier chainntnfs.ChainNotifier

	// PublishTransaction facilitates the process of broadcasting a
	// transaction to the network.
	PublishTransaction func(*wire.MsgTx) error

	// ContractBreaches is a channel where the breachArbiter will receive
	// notifications in the event of a contract breach being observed. A
	// ContractBreachEvent must be ACKed by the breachArbiter, such that
	// the sending subsystem knows that the event is properly handed off.
	ContractBreaches <-chan *ContractBreachEvent

	// Signer is used by the breach arbiter to generate sweep transactions,
	// which move coins from previously open channels back to the user's
	// wallet.
	Signer input.Signer

	// Store is a persistent resource that maintains information regarding
	// breached channels. This is used in conjunction with DB to recover
	// from crashes, restarts, or other failures.
	Store RetributionStore
}

// breachArbiter is a special subsystem which is responsible for watching and
// acting on the detection of any attempted uncooperative channel breaches by
// channel counterparties. This file essentially acts as deterrence code for
// those attempting to launch attacks against the daemon. In practice it's
// expected that the logic in this file never gets executed, but it is
// important to have it in place just in case we encounter cheating channel
// counterparties.
// TODO(roasbeef): closures in config for subsystem pointers to decouple?
type breachArbiter struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *BreachConfig

	quit chan struct{}
	wg   sync.WaitGroup
	sync.Mutex
}

// newBreachArbiter creates a new instance of a breachArbiter initialized with
// its dependent objects.
func newBreachArbiter(cfg *BreachConfig) *breachArbiter {
	return &breachArbiter{
		cfg:  cfg,
		quit: make(chan struct{}),
	}
}

// Start is an idempotent method that officially starts the breachArbiter along
// with all other goroutines it needs to perform its functions.
func (b *breachArbiter) Start() error {
	if !atomic.CompareAndSwapUint32(&b.started, 0, 1) {
		return nil
	}

	brarLog.Tracef("Starting breach arbiter")

	// Load all retributions currently persisted in the retribution store.
	breachRetInfos := make(map[wire.OutPoint]retributionInfo)
	if err := b.cfg.Store.ForAll(func(ret *retributionInfo) error {
		breachRetInfos[ret.chanPoint] = *ret
		return nil
	}); err != nil {
		return err
	}

	// Load all currently closed channels from disk, we will use the
	// channels that have been marked fully closed to filter the retribution
	// information loaded from disk. This is necessary in the event that the
	// channel was marked fully closed, but was not removed from the
	// retribution store.
	closedChans, err := b.cfg.DB.FetchClosedChannels(false)
	if err != nil {
		brarLog.Errorf("Unable to fetch closing channels: %v", err)
		return err
	}

	// Using the set of non-pending, closed channels, reconcile any
	// discrepancies between the channeldb and the retribution store by
	// removing any retribution information for which we have already
	// finished our responsibilities. If the removal is successful, we also
	// remove the entry from our in-memory map, to avoid any further action
	// for this channel.
	// TODO(halseth): no need continue on IsPending once closed channels
	// actually means close transaction is confirmed.
	for _, chanSummary := range closedChans {
		if chanSummary.IsPending {
			continue
		}

		chanPoint := &chanSummary.ChanPoint
		if _, ok := breachRetInfos[*chanPoint]; ok {
			if err := b.cfg.Store.Remove(chanPoint); err != nil {
				brarLog.Errorf("Unable to remove closed "+
					"chanid=%v from breach arbiter: %v",
					chanPoint, err)
				return err
			}
			delete(breachRetInfos, *chanPoint)
		}
	}

	// Spawn the exactRetribution tasks to monitor and resolve any breaches
	// that were loaded from the retribution store.
	for chanPoint := range breachRetInfos {
		retInfo := breachRetInfos[chanPoint]

		// Register for a notification when the breach transaction is
		// confirmed on chain.
		breachTXID := retInfo.commitHash
		breachScript := retInfo.breachedOutputs[0].signDesc.Output.PkScript
		confChan, err := b.cfg.Notifier.RegisterConfirmationsNtfn(
			&breachTXID, breachScript, 1, retInfo.breachHeight,
		)
		if err != nil {
			brarLog.Errorf("Unable to register for conf updates "+
				"for txid: %v, err: %v", breachTXID, err)
			return err
		}

		// Launch a new goroutine which to finalize the channel
		// retribution after the breach transaction confirms.
		b.wg.Add(1)
		go b.exactRetribution(confChan, &retInfo)
	}

	// Start watching the remaining active channels!
	b.wg.Add(1)
	go b.contractObserver()

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

// IsBreached queries the breach arbiter's retribution store to see if it is
// aware of any channel breaches for a particular channel point.
func (b *breachArbiter) IsBreached(chanPoint *wire.OutPoint) (bool, error) {
	return b.cfg.Store.IsBreached(chanPoint)
}

// contractObserver is the primary goroutine for the breachArbiter. This
// goroutine is responsible for handling breach events coming from the
// contractcourt on the ContractBreaches channel. If a channel breach is
// detected, then the contractObserver will execute the retribution logic
// required to sweep ALL outputs from a contested channel into the daemon's
// wallet.
//
// NOTE: This MUST be run as a goroutine.
func (b *breachArbiter) contractObserver() {
	defer b.wg.Done()

	brarLog.Infof("Starting contract observer, watching for breaches.")

	for {
		select {
		case breachEvent := <-b.cfg.ContractBreaches:
			// We have been notified about a contract breach!
			// Handle the handoff, making sure we ACK the event
			// after we have safely added it to the retribution
			// store.
			b.wg.Add(1)
			go b.handleBreachHandoff(breachEvent)

		case <-b.quit:
			return
		}
	}
}

// convertToSecondLevelRevoke takes a breached output, and a transaction that
// spends it to the second level, and mutates the breach output into one that
// is able to properly sweep that second level output. We'll use this function
// when we go to sweep a breached commitment transaction, but the cheating
// party has already attempted to take it to the second level
func convertToSecondLevelRevoke(bo *breachedOutput, breachInfo *retributionInfo,
	spendDetails *chainntnfs.SpendDetail) {

	// In this case, we'll modify the witness type of this output to
	// actually prepare for a second level revoke.
	bo.witnessType = input.HtlcSecondLevelRevoke

	// We'll also redirect the outpoint to this second level output, so the
	// spending transaction updates it inputs accordingly.
	spendingTx := spendDetails.SpendingTx
	oldOp := bo.outpoint
	bo.outpoint = wire.OutPoint{
		Hash:  spendingTx.TxHash(),
		Index: 0,
	}

	// Next, we need to update the amount so we can do fee estimation
	// properly, and also so we can generate a valid signature as we need
	// to know the new input value (the second level transactions shaves
	// off some funds to fees).
	newAmt := spendingTx.TxOut[0].Value
	bo.amt = btcutil.Amount(newAmt)
	bo.signDesc.Output.Value = newAmt
	bo.signDesc.Output.PkScript = spendingTx.TxOut[0].PkScript

	// Finally, we'll need to adjust the witness program in the
	// SignDescriptor.
	bo.signDesc.WitnessScript = bo.secondLevelWitnessScript

	brarLog.Warnf("HTLC(%v) for ChannelPoint(%v) has been spent to the "+
		"second-level, adjusting -> %v", oldOp, breachInfo.chanPoint,
		bo.outpoint)
}

// waitForSpendEvent waits for any of the breached outputs to get spent, and
// mutates the breachInfo to be able to sweep it. This method should be used
// when we fail to publish the justice tx because of a double spend, indicating
// that the counter party has taken one of the breached outputs to the second
// level. The spendNtfns map is a cache used to store registered spend
// subscriptions, in case we must call this method multiple times.
func (b *breachArbiter) waitForSpendEvent(breachInfo *retributionInfo,
	spendNtfns map[wire.OutPoint]*chainntnfs.SpendEvent) error {

	inputs := breachInfo.breachedOutputs

	// spend is used to wrap the index of the output that gets spent
	// together with the spend details.
	type spend struct {
		index  int
		detail *chainntnfs.SpendDetail
	}

	// We create a channel the first goroutine that gets a spend event can
	// signal. We make it buffered in case multiple spend events come in at
	// the same time.
	anySpend := make(chan struct{}, len(inputs))

	// The allSpends channel will be used to pass spend events from all the
	// goroutines that detects a spend before they are signalled to exit.
	allSpends := make(chan spend, len(inputs))

	// exit will be used to signal the goroutines that they can exit.
	exit := make(chan struct{})
	var wg sync.WaitGroup

	// We'll now launch a goroutine for each of the HTLC outputs, that will
	// signal the moment they detect a spend event.
	for i := range inputs {
		breachedOutput := &inputs[i]

		brarLog.Infof("Checking spend from %v(%v) for ChannelPoint(%v)",
			breachedOutput.witnessType, breachedOutput.outpoint,
			breachInfo.chanPoint)

		// If we have already registered for a notification for this
		// output, we'll reuse it.
		spendNtfn, ok := spendNtfns[breachedOutput.outpoint]
		if !ok {
			var err error
			spendNtfn, err = b.cfg.Notifier.RegisterSpendNtfn(
				&breachedOutput.outpoint,
				breachedOutput.signDesc.Output.PkScript,
				breachInfo.breachHeight,
			)
			if err != nil {
				brarLog.Errorf("Unable to check for spentness "+
					"of outpoint=%v: %v",
					breachedOutput.outpoint, err)

				// Registration may have failed if we've been
				// instructed to shutdown. If so, return here
				// to avoid entering an infinite loop.
				select {
				case <-b.quit:
					return errBrarShuttingDown
				default:
					continue
				}
			}
			spendNtfns[breachedOutput.outpoint] = spendNtfn
		}

		// Launch a goroutine waiting for a spend event.
		b.wg.Add(1)
		wg.Add(1)
		go func(index int, spendEv *chainntnfs.SpendEvent) {
			defer b.wg.Done()
			defer wg.Done()

			select {
			// The output has been taken to the second level!
			case sp, ok := <-spendEv.Spend:
				if !ok {
					return
				}

				brarLog.Infof("Detected spend on %s(%v) by "+
					"txid(%v) for ChannelPoint(%v)",
					inputs[index].witnessType,
					inputs[index].outpoint,
					sp.SpenderTxHash,
					breachInfo.chanPoint)

				// First we send the spend event on the
				// allSpends channel, such that it can be
				// handled after all go routines have exited.
				allSpends <- spend{index, sp}

				// Finally we'll signal the anySpend channel
				// that a spend was detected, such that the
				// other goroutines can be shut down.
				anySpend <- struct{}{}
			case <-exit:
				return
			case <-b.quit:
				return
			}
		}(i, spendNtfn)
	}

	// We'll wait for any of the outputs to be spent, or that we are
	// signalled to exit.
	select {
	// A goroutine have signalled that a spend occurred.
	case <-anySpend:
		// Signal for the remaining goroutines to exit.
		close(exit)
		wg.Wait()

		// At this point all goroutines that can send on the allSpends
		// channel have exited. We can therefore safely close the
		// channel before ranging over its content.
		close(allSpends)

		doneOutputs := make(map[int]struct{})
		for s := range allSpends {
			breachedOutput := &inputs[s.index]
			delete(spendNtfns, breachedOutput.outpoint)

			switch breachedOutput.witnessType {
			case input.HtlcAcceptedRevoke:
				fallthrough
			case input.HtlcOfferedRevoke:
				brarLog.Infof("Spend on second-level"+
					"%s(%v) for ChannelPoint(%v) "+
					"transitions to second-level output",
					breachedOutput.witnessType,
					breachedOutput.outpoint,
					breachInfo.chanPoint)

				// In this case we'll morph our initial revoke
				// spend to instead point to the second level
				// output, and update the sign descriptor in the
				// process.
				convertToSecondLevelRevoke(
					breachedOutput, breachInfo, s.detail,
				)

				continue
			}

			brarLog.Infof("Spend on %s(%v) for ChannelPoint(%v) "+
				"transitions output to terminal state, "+
				"removing input from justice transaction",
				breachedOutput.witnessType,
				breachedOutput.outpoint, breachInfo.chanPoint)

			doneOutputs[s.index] = struct{}{}
		}

		// Filter the inputs for which we can no longer proceed.
		var nextIndex int
		for i := range inputs {
			if _, ok := doneOutputs[i]; ok {
				continue
			}

			inputs[nextIndex] = inputs[i]
			nextIndex++
		}

		// Update our remaining set of outputs before continuing with
		// another attempt at publication.
		breachInfo.breachedOutputs = inputs[:nextIndex]

	case <-b.quit:
		return errBrarShuttingDown
	}

	return nil
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
	var breachConfHeight uint32
	select {
	case breachConf, ok := <-confChan.Confirmed:
		// If the second value is !ok, then the channel has been closed
		// signifying a daemon shutdown, so we exit.
		if !ok {
			return
		}

		breachConfHeight = breachConf.BlockHeight

		// Otherwise, if this is a real confirmation notification, then
		// we fall through to complete our duty.
	case <-b.quit:
		return
	}

	brarLog.Debugf("Breach transaction %v has been confirmed, sweeping "+
		"revoked funds", breachInfo.commitHash)

	// We may have to wait for some of the HTLC outputs to be spent to the
	// second level before broadcasting the justice tx. We'll store the
	// SpendEvents between each attempt to not re-register uneccessarily.
	spendNtfns := make(map[wire.OutPoint]*chainntnfs.SpendEvent)

	finalTx, err := b.cfg.Store.GetFinalizedTxn(&breachInfo.chanPoint)
	if err != nil {
		brarLog.Errorf("Unable to get finalized txn for"+
			"chanid=%v: %v", &breachInfo.chanPoint, err)
		return
	}

	// If this retribution has not been finalized before, we will first
	// construct a sweep transaction and write it to disk. This will allow
	// the breach arbiter to re-register for notifications for the justice
	// txid.
justiceTxBroadcast:
	if finalTx == nil {
		// With the breach transaction confirmed, we now create the
		// justice tx which will claim ALL the funds within the
		// channel.
		finalTx, err = b.createJusticeTx(breachInfo)
		if err != nil {
			brarLog.Errorf("Unable to create justice tx: %v", err)
			return
		}

		// Persist our finalized justice transaction before making an
		// attempt to broadcast.
		err := b.cfg.Store.Finalize(&breachInfo.chanPoint, finalTx)
		if err != nil {
			brarLog.Errorf("Unable to finalize justice tx for "+
				"chanid=%v: %v", &breachInfo.chanPoint, err)
			return
		}
	}

	brarLog.Debugf("Broadcasting justice tx: %v", newLogClosure(func() string {
		return spew.Sdump(finalTx)
	}))

	// We'll now attempt to broadcast the transaction which finalized the
	// channel's retribution against the cheating counter party.
	err = b.cfg.PublishTransaction(finalTx)
	if err != nil {
		brarLog.Errorf("Unable to broadcast justice tx: %v", err)

		if err == lnwallet.ErrDoubleSpend {
			// Broadcasting the transaction failed because of a
			// conflict either in the mempool or in chain. We'll
			// now create spend subscriptions for all HTLC outputs
			// on the commitment transaction that could possibly
			// have been spent, and wait for any of them to
			// trigger.
			brarLog.Infof("Waiting for a spend event before " +
				"attempting to craft new justice tx.")
			finalTx = nil

			err := b.waitForSpendEvent(breachInfo, spendNtfns)
			if err != nil {
				if err != errBrarShuttingDown {
					brarLog.Errorf("error waiting for "+
						"spend event: %v", err)
				}
				return
			}

			if len(breachInfo.breachedOutputs) == 0 {
				brarLog.Debugf("No more outputs to sweep for "+
					"breach, marking ChannelPoint(%v) "+
					"fully resolved", breachInfo.chanPoint)

				err = b.cleanupBreach(&breachInfo.chanPoint)
				if err != nil {
					brarLog.Errorf("Failed to cleanup "+
						"breached ChannelPoint(%v): %v",
						breachInfo.chanPoint, err)
				}
				return
			}

			brarLog.Infof("Attempting another justice tx "+
				"with %d inputs",
				len(breachInfo.breachedOutputs))

			goto justiceTxBroadcast
		}
	}

	// As a conclusionary step, we register for a notification to be
	// dispatched once the justice tx is confirmed. After confirmation we
	// notify the caller that initiated the retribution workflow that the
	// deed has been done.
	justiceTXID := finalTx.TxHash()
	justiceScript := finalTx.TxOut[0].PkScript
	confChan, err = b.cfg.Notifier.RegisterConfirmationsNtfn(
		&justiceTXID, justiceScript, 1, breachConfHeight,
	)
	if err != nil {
		brarLog.Errorf("Unable to register for conf for txid(%v): %v",
			justiceTXID, err)
		return
	}

	select {
	case _, ok := <-confChan.Confirmed:
		if !ok {
			return
		}

		// Compute both the total value of funds being swept and the
		// amount of funds that were revoked from the counter party.
		var totalFunds, revokedFunds btcutil.Amount
		for _, inp := range breachInfo.breachedOutputs {
			totalFunds += inp.Amount()

			// If the output being revoked is the remote commitment
			// output or an offered HTLC output, it's amount
			// contributes to the value of funds being revoked from
			// the counter party.
			switch inp.WitnessType() {
			case input.CommitmentRevoke:
				revokedFunds += inp.Amount()
			case input.HtlcOfferedRevoke:
				revokedFunds += inp.Amount()
			default:
			}
		}

		brarLog.Infof("Justice for ChannelPoint(%v) has "+
			"been served, %v revoked funds (%v total) "+
			"have been claimed", breachInfo.chanPoint,
			revokedFunds, totalFunds)

		err = b.cleanupBreach(&breachInfo.chanPoint)
		if err != nil {
			brarLog.Errorf("Failed to cleanup breached "+
				"ChannelPoint(%v): %v", breachInfo.chanPoint,
				err)
		}

		// TODO(roasbeef): add peer to blacklist?

		// TODO(roasbeef): close other active channels with offending
		// peer

		return
	case <-b.quit:
		return
	}
}

// cleanupBreach marks the given channel point as fully resolved and removes the
// retribution for that the channel from the retribution store.
func (b *breachArbiter) cleanupBreach(chanPoint *wire.OutPoint) error {
	// With the channel closed, mark it in the database as such.
	err := b.cfg.DB.MarkChanFullyClosed(chanPoint)
	if err != nil {
		return fmt.Errorf("unable to mark chan as closed: %v", err)
	}

	// Justice has been carried out; we can safely delete the retribution
	// info from the database.
	err = b.cfg.Store.Remove(chanPoint)
	if err != nil {
		return fmt.Errorf("unable to remove retribution from db: %v",
			err)
	}

	return nil
}

// handleBreachHandoff handles a new breach event, by writing it to disk, then
// notifies the breachArbiter contract observer goroutine that a channel's
// contract has been breached by the prior counterparty. Once notified the
// breachArbiter will attempt to sweep ALL funds within the channel using the
// information provided within the BreachRetribution generated due to the
// breach of channel contract. The funds will be swept only after the breaching
// transaction receives a necessary number of confirmations.
//
// NOTE: This MUST be run as a goroutine.
func (b *breachArbiter) handleBreachHandoff(breachEvent *ContractBreachEvent) {
	defer b.wg.Done()

	chanPoint := breachEvent.ChanPoint
	brarLog.Debugf("Handling breach handoff for ChannelPoint(%v)",
		chanPoint)

	// A read from this channel indicates that a channel breach has been
	// detected! So we notify the main coordination goroutine with the
	// information needed to bring the counterparty to justice.
	breachInfo := breachEvent.BreachRetribution
	brarLog.Warnf("REVOKED STATE #%v FOR ChannelPoint(%v) "+
		"broadcast, REMOTE PEER IS DOING SOMETHING "+
		"SKETCHY!!!", breachInfo.RevokedStateNum,
		chanPoint)

	// Immediately notify the HTLC switch that this link has been
	// breached in order to ensure any incoming or outgoing
	// multi-hop HTLCs aren't sent over this link, nor any other
	// links associated with this peer.
	b.cfg.CloseLink(&chanPoint, htlcswitch.CloseBreach)

	// TODO(roasbeef): need to handle case of remote broadcast
	// mid-local initiated state-transition, possible
	// false-positive?

	// Acquire the mutex to ensure consistency between the call to
	// IsBreached and Add below.
	b.Lock()

	// We first check if this breach info is already added to the
	// retribution store.
	breached, err := b.cfg.Store.IsBreached(&chanPoint)
	if err != nil {
		b.Unlock()
		brarLog.Errorf("Unable to check breach info in DB: %v", err)

		select {
		case breachEvent.ProcessACK <- err:
		case <-b.quit:
		}
		return
	}

	// If this channel is already marked as breached in the retribution
	// store, we already have handled the handoff for this breach. In this
	// case we can safely ACK the handoff, and return.
	if breached {
		b.Unlock()

		select {
		case breachEvent.ProcessACK <- nil:
		case <-b.quit:
		}
		return
	}

	// Using the breach information provided by the wallet and the
	// channel snapshot, construct the retribution information that
	// will be persisted to disk.
	retInfo := newRetributionInfo(&chanPoint, breachInfo)

	// Persist the pending retribution state to disk.
	err = b.cfg.Store.Add(retInfo)
	b.Unlock()
	if err != nil {
		brarLog.Errorf("Unable to persist retribution "+
			"info to db: %v", err)
	}

	// Now that the breach has been persisted, try to send an
	// acknowledgment back to the close observer with the error. If
	// the ack is successful, the close observer will mark the
	// channel as pending-closed in the channeldb.
	select {
	case breachEvent.ProcessACK <- err:
		// Bail if we failed to persist retribution info.
		if err != nil {
			return
		}

	case <-b.quit:
		return
	}

	// Now that a new channel contract has been added to the retribution
	// store, we first register for a notification to be dispatched once
	// the breach transaction (the revoked commitment transaction) has been
	// confirmed in the chain to ensure we're not dealing with a moving
	// target.
	breachTXID := &retInfo.commitHash
	breachScript := retInfo.breachedOutputs[0].signDesc.Output.PkScript
	cfChan, err := b.cfg.Notifier.RegisterConfirmationsNtfn(
		breachTXID, breachScript, 1, retInfo.breachHeight,
	)
	if err != nil {
		brarLog.Errorf("Unable to register for conf updates for "+
			"txid: %v, err: %v", breachTXID, err)
		return
	}

	brarLog.Warnf("A channel has been breached with txid: %v. Waiting "+
		"for confirmation, then justice will be served!", breachTXID)

	// With the retribution state persisted, channel close persisted, and
	// notification registered, we launch a new goroutine which will
	// finalize the channel retribution after the breach transaction has
	// been confirmed.
	b.wg.Add(1)
	go b.exactRetribution(cfChan, retInfo)
}

// breachedOutput contains all the information needed to sweep a breached
// output. A breached output is an output that we are now entitled to due to a
// revoked commitment transaction being broadcast.
type breachedOutput struct {
	amt         btcutil.Amount
	outpoint    wire.OutPoint
	witnessType input.WitnessType
	signDesc    input.SignDescriptor
	confHeight  uint32

	secondLevelWitnessScript []byte

	witnessFunc input.WitnessGenerator
}

// makeBreachedOutput assembles a new breachedOutput that can be used by the
// breach arbiter to construct a justice or sweep transaction.
func makeBreachedOutput(outpoint *wire.OutPoint,
	witnessType input.WitnessType,
	secondLevelScript []byte,
	signDescriptor *input.SignDescriptor,
	confHeight uint32) breachedOutput {

	amount := signDescriptor.Output.Value

	return breachedOutput{
		amt:                      btcutil.Amount(amount),
		outpoint:                 *outpoint,
		secondLevelWitnessScript: secondLevelScript,
		witnessType:              witnessType,
		signDesc:                 *signDescriptor,
		confHeight:               confHeight,
	}
}

// Amount returns the number of satoshis contained in the breached output.
func (bo *breachedOutput) Amount() btcutil.Amount {
	return bo.amt
}

// OutPoint returns the breached output's identifier that is to be included as a
// transaction input.
func (bo *breachedOutput) OutPoint() *wire.OutPoint {
	return &bo.outpoint
}

// WitnessType returns the type of witness that must be generated to spend the
// breached output.
func (bo *breachedOutput) WitnessType() input.WitnessType {
	return bo.witnessType
}

// SignDesc returns the breached output's SignDescriptor, which is used during
// signing to compute the witness.
func (bo *breachedOutput) SignDesc() *input.SignDescriptor {
	return &bo.signDesc
}

// CraftInputScript computes a valid witness that allows us to spend from the
// breached output. It does so by first generating and memoizing the witness
// generation function, which parameterized primarily by the witness type and
// sign descriptor. The method then returns the witness computed by invoking
// this function on the first and subsequent calls.
func (bo *breachedOutput) CraftInputScript(signer input.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes, txinIdx int) (*input.Script, error) {

	// First, we ensure that the witness generation function has been
	// initialized for this breached output.
	bo.witnessFunc = bo.witnessType.GenWitnessFunc(
		signer, bo.SignDesc(),
	)

	// Now that we have ensured that the witness generation function has
	// been initialized, we can proceed to execute it and generate the
	// witness for this particular breached output.
	return bo.witnessFunc(txn, hashCache, txinIdx)
}

// BlocksToMaturity returns the relative timelock, as a number of blocks, that
// must be built on top of the confirmation height before the output can be
// spent.
func (bo *breachedOutput) BlocksToMaturity() uint32 {
	return 0
}

// HeightHint returns the minimum height at which a confirmed spending tx can
// occur.
func (bo *breachedOutput) HeightHint() uint32 {
	return bo.confHeight
}

// Add compile-time constraint ensuring breachedOutput implements the Input
// interface.
var _ input.Input = (*breachedOutput)(nil)

// retributionInfo encapsulates all the data needed to sweep all the contested
// funds within a channel whose contract has been breached by the prior
// counterparty. This struct is used to create the justice transaction which
// spends all outputs of the commitment transaction into an output controlled
// by the wallet.
type retributionInfo struct {
	commitHash   chainhash.Hash
	chanPoint    wire.OutPoint
	chainHash    chainhash.Hash
	breachHeight uint32

	breachedOutputs []breachedOutput
}

// newRetributionInfo constructs a retributionInfo containing all the
// information required by the breach arbiter to recover funds from breached
// channels.  The information is primarily populated using the BreachRetribution
// delivered by the wallet when it detects a channel breach.
func newRetributionInfo(chanPoint *wire.OutPoint,
	breachInfo *lnwallet.BreachRetribution) *retributionInfo {

	// Determine the number of second layer HTLCs we will attempt to sweep.
	nHtlcs := len(breachInfo.HtlcRetributions)

	// Initialize a slice to hold the outputs we will attempt to sweep. The
	// maximum capacity of the slice is set to 2+nHtlcs to handle the case
	// where the local, remote, and all HTLCs are not dust outputs.  All
	// HTLC outputs provided by the wallet are guaranteed to be non-dust,
	// though the commitment outputs are conditionally added depending on
	// the nil-ness of their sign descriptors.
	breachedOutputs := make([]breachedOutput, 0, nHtlcs+2)

	// First, record the breach information for the local channel point if
	// it is not considered dust, which is signaled by a non-nil sign
	// descriptor. Here we use CommitmentNoDelay since this output belongs
	// to us and has no time-based constraints on spending.
	if breachInfo.LocalOutputSignDesc != nil {
		localOutput := makeBreachedOutput(
			&breachInfo.LocalOutpoint,
			input.CommitmentNoDelay,
			// No second level script as this is a commitment
			// output.
			nil,
			breachInfo.LocalOutputSignDesc,
			breachInfo.BreachHeight)

		breachedOutputs = append(breachedOutputs, localOutput)
	}

	// Second, record the same information regarding the remote outpoint,
	// again if it is not dust, which belongs to the party who tried to
	// steal our money! Here we set witnessType of the breachedOutput to
	// CommitmentRevoke, since we will be using a revoke key, withdrawing
	// the funds from the commitment transaction immediately.
	if breachInfo.RemoteOutputSignDesc != nil {
		remoteOutput := makeBreachedOutput(
			&breachInfo.RemoteOutpoint,
			input.CommitmentRevoke,
			// No second level script as this is a commitment
			// output.
			nil,
			breachInfo.RemoteOutputSignDesc,
			breachInfo.BreachHeight)

		breachedOutputs = append(breachedOutputs, remoteOutput)
	}

	// Lastly, for each of the breached HTLC outputs, record each as a
	// breached output with the appropriate witness type based on its
	// directionality. All HTLC outputs provided by the wallet are assumed
	// to be non-dust.
	for i, breachedHtlc := range breachInfo.HtlcRetributions {
		// Using the breachedHtlc's incoming flag, determine the
		// appropriate witness type that needs to be generated in order
		// to sweep the HTLC output.
		var htlcWitnessType input.WitnessType
		if breachedHtlc.IsIncoming {
			htlcWitnessType = input.HtlcAcceptedRevoke
		} else {
			htlcWitnessType = input.HtlcOfferedRevoke
		}

		htlcOutput := makeBreachedOutput(
			&breachInfo.HtlcRetributions[i].OutPoint,
			htlcWitnessType,
			breachInfo.HtlcRetributions[i].SecondLevelWitnessScript,
			&breachInfo.HtlcRetributions[i].SignDesc,
			breachInfo.BreachHeight)

		breachedOutputs = append(breachedOutputs, htlcOutput)
	}

	return &retributionInfo{
		commitHash:      breachInfo.BreachTransaction.TxHash(),
		chainHash:       breachInfo.ChainHash,
		chanPoint:       *chanPoint,
		breachedOutputs: breachedOutputs,
		breachHeight:    breachInfo.BreachHeight,
	}
}

// createJusticeTx creates a transaction which exacts "justice" by sweeping ALL
// the funds within the channel which we are now entitled to due to a breach of
// the channel's contract by the counterparty. This function returns a *fully*
// signed transaction with the witness for each input fully in place.
func (b *breachArbiter) createJusticeTx(
	r *retributionInfo) (*wire.MsgTx, error) {

	// We will assemble the breached outputs into a slice of spendable
	// outputs, while simultaneously computing the estimated weight of the
	// transaction.
	var (
		spendableOutputs []input.Input
		weightEstimate   input.TxWeightEstimator
	)

	// Allocate enough space to potentially hold each of the breached
	// outputs in the retribution info.
	spendableOutputs = make([]input.Input, 0, len(r.breachedOutputs))

	// The justice transaction we construct will be a segwit transaction
	// that pays to a p2wkh output. Components such as the version,
	// nLockTime, and output are already included in the TxWeightEstimator.
	weightEstimate.AddP2WKHOutput()

	// Next, we iterate over the breached outputs contained in the
	// retribution info.  For each, we switch over the witness type such
	// that we contribute the appropriate weight for each input and witness,
	// finally adding to our list of spendable outputs.
	for i := range r.breachedOutputs {
		// Grab locally scoped reference to breached output.
		inp := &r.breachedOutputs[i]

		// First, select the appropriate estimated witness weight for
		// the give witness type of this breached output. If the witness
		// type is unrecognized, we will omit it from the transaction.
		var witnessWeight int
		switch inp.WitnessType() {
		case input.CommitmentNoDelay:
			witnessWeight = input.P2WKHWitnessSize

		case input.CommitmentRevoke:
			witnessWeight = input.ToLocalPenaltyWitnessSize

		case input.HtlcOfferedRevoke:
			witnessWeight = input.OfferedHtlcPenaltyWitnessSize

		case input.HtlcAcceptedRevoke:
			witnessWeight = input.AcceptedHtlcPenaltyWitnessSize

		case input.HtlcSecondLevelRevoke:
			witnessWeight = input.ToLocalPenaltyWitnessSize

		default:
			brarLog.Warnf("breached output in retribution info "+
				"contains unexpected witness type: %v",
				inp.WitnessType())
			continue
		}
		weightEstimate.AddWitnessInput(witnessWeight)

		// Finally, append this input to our list of spendable outputs.
		spendableOutputs = append(spendableOutputs, inp)
	}

	txWeight := int64(weightEstimate.Weight())
	return b.sweepSpendableOutputsTxn(txWeight, spendableOutputs...)
}

// sweepSpendableOutputsTxn creates a signed transaction from a sequence of
// spendable outputs by sweeping the funds into a single p2wkh output.
func (b *breachArbiter) sweepSpendableOutputsTxn(txWeight int64,
	inputs ...input.Input) (*wire.MsgTx, error) {

	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	// TODO(roasbeef): possibly create many outputs to minimize change in
	// the future?
	pkScript, err := b.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	// Compute the total amount contained in the inputs.
	var totalAmt btcutil.Amount
	for _, input := range inputs {
		totalAmt += btcutil.Amount(input.SignDesc().Output.Value)
	}

	// We'll actually attempt to target inclusion within the next two
	// blocks as we'd like to sweep these funds back into our wallet ASAP.
	feePerKw, err := b.cfg.Estimator.EstimateFeePerKW(2)
	if err != nil {
		return nil, err
	}
	txFee := feePerKw.FeeForWeight(txWeight)

	// TODO(roasbeef): already start to siphon their funds into fees
	sweepAmt := int64(totalAmt - txFee)

	// With the fee calculated, we can now create the transaction using the
	// information gathered above and the provided retribution information.
	txn := wire.NewMsgTx(2)

	// We begin by adding the output to which our funds will be deposited.
	txn.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    sweepAmt,
	})

	// Next, we add all of the spendable outputs as inputs to the
	// transaction.
	for _, input := range inputs {
		txn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *input.OutPoint(),
		})
	}

	// Before signing the transaction, check to ensure that it meets some
	// basic validity requirements.
	btx := btcutil.NewTx(txn)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, err
	}

	// Create a sighash cache to improve the performance of hashing and
	// signing SigHashAll inputs.
	hashCache := txscript.NewTxSigHashes(txn)

	// Create a closure that encapsulates the process of initializing a
	// particular output's witness generation function, computing the
	// witness, and attaching it to the transaction. This function accepts
	// an integer index representing the intended txin index, and the
	// breached output from which it will spend.
	addWitness := func(idx int, so input.Input) error {
		// First, we construct a valid witness for this outpoint and
		// transaction using the SpendableOutput's witness generation
		// function.
		inputScript, err := so.CraftInputScript(
			b.cfg.Signer, txn, hashCache, idx,
		)
		if err != nil {
			return err
		}

		// Then, we add the witness to the transaction at the
		// appropriate txin index.
		txn.TxIn[idx].Witness = inputScript.Witness

		return nil
	}

	// Finally, generate a witness for each output and attach it to the
	// transaction.
	for i, input := range inputs {
		if err := addWitness(i, input); err != nil {
			return nil, err
		}
	}

	return txn, nil
}

// RetributionStore provides an interface for managing a persistent map from
// wire.OutPoint -> retributionInfo. Upon learning of a breach, a BreachArbiter
// should record the retributionInfo for the breached channel, which serves a
// checkpoint in the event that retribution needs to be resumed after failure.
// A RetributionStore provides an interface for managing the persisted set, as
// well as mapping user defined functions over the entire on-disk contents.
//
// Calls to RetributionStore may occur concurrently. A concrete instance of
// RetributionStore should use appropriate synchronization primitives, or
// be otherwise safe for concurrent access.
type RetributionStore interface {
	// Add persists the retributionInfo to disk, using the information's
	// chanPoint as the key. This method should overwrite any existing
	// entries found under the same key, and an error should be raised if
	// the addition fails.
	Add(retInfo *retributionInfo) error

	// IsBreached queries the retribution store to see if the breach arbiter
	// is aware of any breaches for the provided channel point.
	IsBreached(chanPoint *wire.OutPoint) (bool, error)

	// Finalize persists the finalized justice transaction for a particular
	// channel.
	Finalize(chanPoint *wire.OutPoint, finalTx *wire.MsgTx) error

	// GetFinalizedTxn loads the finalized justice transaction, if any, from
	// the retribution store. The finalized transaction will be nil if
	// Finalize has not yet been called for this channel point.
	GetFinalizedTxn(chanPoint *wire.OutPoint) (*wire.MsgTx, error)

	// Remove deletes the retributionInfo from disk, if any exists, under
	// the given key. An error should be re raised if the removal fails.
	Remove(key *wire.OutPoint) error

	// ForAll iterates over the existing on-disk contents and applies a
	// chosen, read-only callback to each. This method should ensure that it
	// immediately propagate any errors generated by the callback.
	ForAll(cb func(*retributionInfo) error) error
}

// retributionStore handles persistence of retribution states to disk and is
// backed by a boltdb bucket. The primary responsibility of the retribution
// store is to ensure that we can recover from a restart in the middle of a
// breached contract retribution.
type retributionStore struct {
	db *channeldb.DB
}

// newRetributionStore creates a new instance of a retributionStore.
func newRetributionStore(db *channeldb.DB) *retributionStore {
	return &retributionStore{
		db: db,
	}
}

// Add adds a retribution state to the retributionStore, which is then persisted
// to disk.
func (rs *retributionStore) Add(ret *retributionInfo) error {
	return rs.db.Update(func(tx *bbolt.Tx) error {
		// If this is our first contract breach, the retributionBucket
		// won't exist, in which case, we just create a new bucket.
		retBucket, err := tx.CreateBucketIfNotExists(retributionBucket)
		if err != nil {
			return err
		}

		var outBuf bytes.Buffer
		if err := writeOutpoint(&outBuf, &ret.chanPoint); err != nil {
			return err
		}

		var retBuf bytes.Buffer
		if err := ret.Encode(&retBuf); err != nil {
			return err
		}

		return retBucket.Put(outBuf.Bytes(), retBuf.Bytes())
	})
}

// Finalize writes a signed justice transaction to the retribution store. This
// is done before publishing the transaction, so that we can recover the txid on
// startup and re-register for confirmation notifications.
func (rs *retributionStore) Finalize(chanPoint *wire.OutPoint,
	finalTx *wire.MsgTx) error {
	return rs.db.Update(func(tx *bbolt.Tx) error {
		justiceBkt, err := tx.CreateBucketIfNotExists(justiceTxnBucket)
		if err != nil {
			return err
		}

		var chanBuf bytes.Buffer
		if err := writeOutpoint(&chanBuf, chanPoint); err != nil {
			return err
		}

		var txBuf bytes.Buffer
		if err := finalTx.Serialize(&txBuf); err != nil {
			return err
		}

		return justiceBkt.Put(chanBuf.Bytes(), txBuf.Bytes())
	})
}

// GetFinalizedTxn loads the finalized justice transaction for the provided
// channel point. The finalized transaction will be nil if Finalize has yet to
// be called for this channel point.
func (rs *retributionStore) GetFinalizedTxn(
	chanPoint *wire.OutPoint) (*wire.MsgTx, error) {

	var finalTxBytes []byte
	if err := rs.db.View(func(tx *bbolt.Tx) error {
		justiceBkt := tx.Bucket(justiceTxnBucket)
		if justiceBkt == nil {
			return nil
		}

		var chanBuf bytes.Buffer
		if err := writeOutpoint(&chanBuf, chanPoint); err != nil {
			return err
		}

		finalTxBytes = justiceBkt.Get(chanBuf.Bytes())

		return nil
	}); err != nil {
		return nil, err
	}

	if finalTxBytes == nil {
		return nil, nil
	}

	finalTx := &wire.MsgTx{}
	err := finalTx.Deserialize(bytes.NewReader(finalTxBytes))

	return finalTx, err
}

// IsBreached queries the retribution store to discern if this channel was
// previously breached. This is used when connecting to a peer to determine if
// it is safe to add a link to the htlcswitch, as we should never add a channel
// that has already been breached.
func (rs *retributionStore) IsBreached(chanPoint *wire.OutPoint) (bool, error) {
	var found bool
	err := rs.db.View(func(tx *bbolt.Tx) error {
		retBucket := tx.Bucket(retributionBucket)
		if retBucket == nil {
			return nil
		}

		var chanBuf bytes.Buffer
		if err := writeOutpoint(&chanBuf, chanPoint); err != nil {
			return err
		}

		retInfo := retBucket.Get(chanBuf.Bytes())
		if retInfo != nil {
			found = true
		}

		return nil
	})

	return found, err
}

// Remove removes a retribution state and finalized justice transaction by
// channel point  from the retribution store.
func (rs *retributionStore) Remove(chanPoint *wire.OutPoint) error {
	return rs.db.Update(func(tx *bbolt.Tx) error {
		retBucket := tx.Bucket(retributionBucket)

		// We return an error if the bucket is not already created,
		// since normal operation of the breach arbiter should never try
		// to remove a finalized retribution state that is not already
		// stored in the db.
		if retBucket == nil {
			return errors.New("Unable to remove retribution " +
				"because the retribution bucket doesn't exist.")
		}

		// Serialize the channel point we are intending to remove.
		var chanBuf bytes.Buffer
		if err := writeOutpoint(&chanBuf, chanPoint); err != nil {
			return err
		}
		chanBytes := chanBuf.Bytes()

		// Remove the persisted retribution info and finalized justice
		// transaction.
		if err := retBucket.Delete(chanBytes); err != nil {
			return err
		}

		// If we have not finalized this channel breach, we can exit
		// early.
		justiceBkt := tx.Bucket(justiceTxnBucket)
		if justiceBkt == nil {
			return nil
		}

		return justiceBkt.Delete(chanBytes)
	})
}

// ForAll iterates through all stored retributions and executes the passed
// callback function on each retribution.
func (rs *retributionStore) ForAll(cb func(*retributionInfo) error) error {
	return rs.db.View(func(tx *bbolt.Tx) error {
		// If the bucket does not exist, then there are no pending
		// retributions.
		retBucket := tx.Bucket(retributionBucket)
		if retBucket == nil {
			return nil
		}

		// Otherwise, we fetch each serialized retribution info,
		// deserialize it, and execute the passed in callback function
		// on it.
		return retBucket.ForEach(func(_, retBytes []byte) error {
			ret := &retributionInfo{}
			err := ret.Decode(bytes.NewBuffer(retBytes))
			if err != nil {
				return err
			}

			return cb(ret)
		})
	})
}

// Encode serializes the retribution into the passed byte stream.
func (ret *retributionInfo) Encode(w io.Writer) error {
	var scratch [4]byte

	if _, err := w.Write(ret.commitHash[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, &ret.chanPoint); err != nil {
		return err
	}

	if _, err := w.Write(ret.chainHash[:]); err != nil {
		return err
	}

	binary.BigEndian.PutUint32(scratch[:], ret.breachHeight)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	nOutputs := len(ret.breachedOutputs)
	if err := wire.WriteVarInt(w, 0, uint64(nOutputs)); err != nil {
		return err
	}

	for _, output := range ret.breachedOutputs {
		if err := output.Encode(w); err != nil {
			return err
		}
	}

	return nil
}

// Dencode deserializes a retribution from the passed byte stream.
func (ret *retributionInfo) Decode(r io.Reader) error {
	var scratch [32]byte

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return err
	}
	hash, err := chainhash.NewHash(scratch[:])
	if err != nil {
		return err
	}
	ret.commitHash = *hash

	if err := readOutpoint(r, &ret.chanPoint); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return err
	}
	chainHash, err := chainhash.NewHash(scratch[:])
	if err != nil {
		return err
	}
	ret.chainHash = *chainHash

	if _, err := io.ReadFull(r, scratch[:4]); err != nil {
		return err
	}
	ret.breachHeight = binary.BigEndian.Uint32(scratch[:4])

	nOutputsU64, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return err
	}
	nOutputs := int(nOutputsU64)

	ret.breachedOutputs = make([]breachedOutput, nOutputs)
	for i := range ret.breachedOutputs {
		if err := ret.breachedOutputs[i].Decode(r); err != nil {
			return err
		}
	}

	return nil
}

// Encode serializes a breachedOutput into the passed byte stream.
func (bo *breachedOutput) Encode(w io.Writer) error {
	var scratch [8]byte

	binary.BigEndian.PutUint64(scratch[:8], uint64(bo.amt))
	if _, err := w.Write(scratch[:8]); err != nil {
		return err
	}

	if err := writeOutpoint(w, &bo.outpoint); err != nil {
		return err
	}

	err := input.WriteSignDescriptor(w, &bo.signDesc)
	if err != nil {
		return err
	}

	err = wire.WriteVarBytes(w, 0, bo.secondLevelWitnessScript)
	if err != nil {
		return err
	}

	binary.BigEndian.PutUint16(scratch[:2], uint16(bo.witnessType))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	return nil
}

// Decode deserializes a breachedOutput from the passed byte stream.
func (bo *breachedOutput) Decode(r io.Reader) error {
	var scratch [8]byte

	if _, err := io.ReadFull(r, scratch[:8]); err != nil {
		return err
	}
	bo.amt = btcutil.Amount(binary.BigEndian.Uint64(scratch[:8]))

	if err := readOutpoint(r, &bo.outpoint); err != nil {
		return err
	}

	if err := input.ReadSignDescriptor(r, &bo.signDesc); err != nil {
		return err
	}

	wScript, err := wire.ReadVarBytes(r, 0, 1000, "witness script")
	if err != nil {
		return err
	}
	bo.secondLevelWitnessScript = wScript

	if _, err := io.ReadFull(r, scratch[:2]); err != nil {
		return err
	}
	bo.witnessType = input.WitnessType(
		binary.BigEndian.Uint16(scratch[:2]),
	)

	return nil
}
