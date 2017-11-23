package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// retributionBucket stores retribution state on disk between detecting a
// contract breach, broadcasting a justice transaction that sweeps the channel,
// and finally witnessing the justice transaction confirm on the blockchain. It
// is critical that such state is persisted on disk, so that if our node
// restarts at any point during the retribution procedure, we can recover and
// continue from the persisted state.
var retributionBucket = []byte("retribution")

// BreachConfig bundles the required subsystems used by the breach arbiter. An
// instance of BreachConfig is passed to newBreachArbiter during instantiation.
type BreachConfig struct {
	// ChainIO is used by the breach arbiter to determine the current height
	// of the blockchain, which is required to subscribe for spend
	// notifications from Notifier.
	ChainIO lnwallet.BlockChainIO

	// CloseLink allows the breach arbiter to shutdown any channel links for
	// which it detects a breach, ensuring now further activity will
	// continue across the link. The method accepts link's channel point and a
	// close type to be included in the channel close summary.
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

	// Signer is used by the breach arbiter to generate sweep transactions,
	// which move coins from previously open channels back to the user's
	// wallet.
	Signer lnwallet.Signer

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
	cfg *BreachConfig

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
func newBreachArbiter(cfg *BreachConfig) *breachArbiter {
	return &breachArbiter{
		cfg: cfg,

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

	// We load all pending retributions from the database and
	// deterministically reconstruct a channel close summary for each. In
	// the event that a channel is still open after being breached, we can
	// use the close summary to reinitiate a channel close so that the
	// breach is reflected in channeldb.
	breachRetInfos := make(map[wire.OutPoint]retributionInfo)
	closeSummaries := make(map[wire.OutPoint]channeldb.ChannelCloseSummary)
	err := b.cfg.Store.ForAll(func(ret *retributionInfo) error {
		// Extract emitted retribution information.
		breachRetInfos[ret.chanPoint] = *ret

		// Deterministically reconstruct channel close summary from
		// persisted retribution information and record in breach close
		// summaries map under the corresponding channel point.
		closeSummary := channeldb.ChannelCloseSummary{
			ChanPoint:      ret.chanPoint,
			ClosingTXID:    ret.commitHash,
			RemotePub:      ret.remoteIdentity,
			Capacity:       ret.capacity,
			SettledBalance: ret.settledBalance,
			CloseType:      channeldb.BreachClose,
			IsPending:      true,
		}
		closeSummaries[ret.chanPoint] = closeSummary

		return nil
	})
	if err != nil {
		return err
	}

	// We need to query that database state for all currently active
	// channels, each of these channels will need a goroutine assigned to
	// it to watch for channel breaches.
	activeChannels, err := b.cfg.DB.FetchAllChannels()
	if err != nil && err != channeldb.ErrNoActiveChannels {
		brarLog.Errorf("unable to fetch active channels: %v", err)
		return err
	}

	nActive := len(activeChannels)
	if nActive > 0 {
		brarLog.Infof("Retrieved %v channels from database, watching "+
			"with vigilance!", nActive)
	}

	// Here we will determine a set of channels that will need to be managed
	// by the contractObserver.  For each of the open channels read from
	// disk, we will create a channel state machine that can be used to
	// watch for any potential channel closures.  We must first exclude any
	// channel whose retribution process has been initiated, and proceed to
	// mark them as closed.  The state machines generated for these filtered
	// channels can be discarded, as their fate will be placed in the hands
	// of an exactRetribution task spawned later.
	//
	// NOTE: Spawning of the exactRetribution task is intentionally
	// postponed until after this step in order to ensure that the all
	// breached channels are reflected as closed in channeldb and consistent
	// with what is checkpointed by the breach arbiter. Instead of treating
	// the breached-and-closed and breached-but-still-active channels as
	// separate sets of channels, we first ensure that all
	// breached-but-still-active channels are promoted to
	// breached-and-closed during restart, allowing us to treat them as a
	// single set from here on out. This approach also has the added benefit
	// of minimizing the likelihood that the wrong number of tasks are
	// spawned per breached channel, and prevents us from being in a
	// position where retribution has completed but the channel is still
	// marked as open in channeldb.
	channelsToWatch := make([]*lnwallet.LightningChannel, 0, nActive)
	for _, chanState := range activeChannels {
		// Initialize active channel from persisted channel state.
		channel, err := lnwallet.NewLightningChannel(nil,
			b.cfg.Notifier, b.cfg.Estimator, chanState)
		if err != nil {
			brarLog.Errorf("unable to load channel from "+
				"disk: %v", err)
			return err
		}

		// Before marking this as an active channel that the breach
		// arbiter should watch, check to see if this channel was
		// previously breached. If so, we attempt to reflect this in the
		// channeldb by closing the channel. Upon success, we continue
		// because the channel is no longer open, and thus does not need
		// to be managed by the contractObserver.
		chanPoint := chanState.FundingOutpoint
		if closeSummary, ok := closeSummaries[chanPoint]; ok {
			// Since this channel should not be open, we
			// immediately notify the HTLC switch that this link
			// should be closed, and that all activity on the link
			// should cease.
			b.cfg.CloseLink(&chanState.FundingOutpoint,
				htlcswitch.CloseBreach)

			// Ensure channeldb is consistent with the persisted
			// breach.
			err := channel.DeleteState(&closeSummary)
			if err != nil {
				brarLog.Errorf("unable to delete channel "+
					"state: %v", err)
				return err
			}

			// Now that this channel is both breached _and_ closed,
			// we can skip adding it to the `channelsToWatch` since
			// we can begin the retribution process immediately.
			continue
		}

		// Finally, add this channel to breach arbiter's list of
		// channels to watch.
		channelsToWatch = append(channelsToWatch, channel)
	}

	// TODO(roasbeef): instead use closure height of channel
	_, currentHeight, err := b.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	// Additionally, we'll also want to watch any pending close or force
	// close transactions so we can properly mark them as resolved in the
	// database.
	if err := b.watchForPendingCloseConfs(currentHeight); err != nil {
		return err
	}

	// Spawn the exactRetribution tasks to monitor and resolve any breaches
	// that were loaded from the retribution store.
	for chanPoint, closeSummary := range closeSummaries {
		// Register for a notification when the breach transaction is
		// confirmed on chain.
		breachTXID := closeSummary.ClosingTXID
		confChan, err := b.cfg.Notifier.RegisterConfirmationsNtfn(
			&breachTXID, 1, uint32(currentHeight))
		if err != nil {
			brarLog.Errorf("unable to register for conf updates "+
				"for txid: %v, err: %v", breachTXID, err)
			return err
		}

		// Launch a new goroutine which to finalize the channel
		// retribution after the breach transaction confirms.
		retInfo := breachRetInfos[chanPoint]
		b.wg.Add(1)
		go b.exactRetribution(confChan, &retInfo)
	}

	// Start watching the remaining active channels!
	b.wg.Add(1)
	go b.contractObserver(channelsToWatch)

	return nil
}

// watchForPendingCloseConfs dispatches confirmation notification subscribers
// that mark any pending channels as fully closed when signaled.
func (b *breachArbiter) watchForPendingCloseConfs(currentHeight int32) error {
	pendingCloseChans, err := b.cfg.DB.FetchClosedChannels(true)
	if err != nil {
		brarLog.Errorf("unable to fetch closing channels: %v", err)
		return err
	}
	for _, pendingClose := range pendingCloseChans {
		// If this channel was force closed, and we have a non-zero
		// time-locked balance, then the utxoNursery is currently
		// watching over it.  As a result we don't need to watch over
		// it.
		if pendingClose.CloseType == channeldb.ForceClose &&
			pendingClose.TimeLockedBalance != 0 {
			continue
		}

		brarLog.Infof("Watching for the closure of ChannelPoint(%v)",
			pendingClose.ChanPoint)

		closeTXID := pendingClose.ClosingTXID
		confNtfn, err := b.cfg.Notifier.RegisterConfirmationsNtfn(
			&closeTXID, 1, uint32(currentHeight))
		if err != nil {
			return err
		}

		b.wg.Add(1)
		go func(chanPoint wire.OutPoint) {
			defer b.wg.Done()

			// In the case that the ChainNotifier is shutting down,
			// all subscriber notification channels will be closed,
			// generating a nil receive.
			select {
			case confInfo, ok := <-confNtfn.Confirmed:
				if !ok {
					return
				}

				brarLog.Infof("ChannelPoint(%v) is "+
					"fully closed, at height: %v",
					chanPoint, confInfo.BlockHeight)

				// TODO(roasbeef): need to store
				// UnilateralCloseSummary on disk so can
				// possibly sweep output here

				err := b.cfg.DB.MarkChanFullyClosed(&chanPoint)
				if err != nil {
					brarLog.Errorf("unable to mark channel"+
						" as closed: %v", err)
				}

			case <-b.quit:
				return
			}
		}(pendingClose.ChanPoint)
	}

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
func (b *breachArbiter) contractObserver(
	activeChannels []*lnwallet.LightningChannel) {

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

	// TODO(roasbeef): need to ensure currentHeight passed in doesn't
	// result in lost notification

out:
	for {
		select {
		case breachInfo := <-b.breachedContracts:
			_, currentHeight, err := b.cfg.ChainIO.GetBestBlock()
			if err != nil {
				brarLog.Errorf("unable to get best height: %v",
					err)
			}

			// A new channel contract has just been breached! We
			// first register for a notification to be dispatched
			// once the breach transaction (the revoked commitment
			// transaction) has been confirmed in the chain to
			// ensure we're not dealing with a moving target.
			breachTXID := &breachInfo.commitHash
			cfChan, err := b.cfg.Notifier.RegisterConfirmationsNtfn(
				breachTXID, 1, uint32(currentHeight))
			if err != nil {
				brarLog.Errorf("unable to register for conf "+
					"updates for txid: %v, err: %v",
					breachTXID, err)
				continue
			}

			brarLog.Warnf("A channel has been breached with "+
				"txid: %v. Waiting for confirmation, then "+
				"justice will be served!", breachTXID)

			// With the retribution state persisted, channel close
			// persisted, and notification registered, we launch a
			// new goroutine which will finalize the channel
			// retribution after the breach transaction has been
			// confirmed.
			b.wg.Add(1)
			go b.exactRetribution(cfChan, breachInfo)

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

			// TODO(roasbeef): add doneChan to signal to peer
			// continue * peer send over to us on
			// loadActiveChanenls, sync until we're aware so no
			// state transitions
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
func (b *breachArbiter) exactRetribution(
	confChan *chainntnfs.ConfirmationEvent,
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

	brarLog.Debugf("Broadcasting justice tx: %v",
		newLogClosure(func() string {
			return spew.Sdump(justiceTx)
		}))

	_, currentHeight, err := b.cfg.ChainIO.GetBestBlock()
	if err != nil {
		brarLog.Errorf("unable to get current height: %v", err)
		return
	}

	// Finally, broadcast the transaction, finalizing the channels'
	// retribution against the cheating counterparty.
	if err := b.cfg.PublishTransaction(justiceTx); err != nil {
		brarLog.Errorf("unable to broadcast "+
			"justice tx: %v", err)
		return
	}

	// As a conclusionary step, we register for a notification to be
	// dispatched once the justice tx is confirmed. After confirmation we
	// notify the caller that initiated the retribution workflow that the
	// deed has been done.
	justiceTXID := justiceTx.TxHash()
	confChan, err = b.cfg.Notifier.RegisterConfirmationsNtfn(
		&justiceTXID, 1, uint32(currentHeight))
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

		// Compute both the total value of funds being swept and the
		// amount of funds that were revoked from the counter party.
		var totalFunds, revokedFunds btcutil.Amount
		for _, input := range breachInfo.breachedOutputs {
			totalFunds += input.Amount()

			// If the output being revoked is the remote commitment
			// output or an offered HTLC output, it's amount
			// contributes to the value of funds being revoked from
			// the counter party.
			switch input.WitnessType() {
			case lnwallet.CommitmentRevoke:
				revokedFunds += input.Amount()
			case lnwallet.HtlcOfferedRevoke:
				revokedFunds += input.Amount()
			default:
			}
		}

		brarLog.Infof("Justice for ChannelPoint(%v) has "+
			"been served, %v revoked funds (%v total) "+
			"have been claimed", breachInfo.chanPoint,
			revokedFunds, totalFunds)

		// With the channel closed, mark it in the database as such.
		err := b.cfg.DB.MarkChanFullyClosed(&breachInfo.chanPoint)
		if err != nil {
			brarLog.Errorf("unable to mark chan as closed: %v", err)
		}

		// Justice has been carried out; we can safely delete the
		// retribution info from the database.
		err = b.cfg.Store.Remove(&breachInfo.chanPoint)
		if err != nil {
			brarLog.Errorf("unable to remove retribution "+
				"from the db: %v", err)
		}

		// TODO(roasbeef): add peer to blacklist?

		// TODO(roasbeef): close other active channels with offending
		// peer

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

	brarLog.Debugf("Breach observer for ChannelPoint(%v) started ",
		chanPoint)

	select {
	// A read from this channel indicates that the contract has been
	// settled cooperatively so we exit as our duties are no longer needed.
	case <-settleSignal:
		contract.CancelObserver()
		contract.Stop()
		return

	// The channel has been closed by a normal means: force closing with
	// the latest commitment transaction.
	case closeInfo := <-contract.UnilateralClose:
		// Launch a goroutine to cancel out this contract within the
		// breachArbiter's main goroutine.
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()

			select {
			case b.settledContracts <- chanPoint:
			case <-b.quit:
			}
		}()

		b.cfg.CloseLink(chanPoint, htlcswitch.CloseBreach)
		contract.CancelObserver()
		contract.Stop()

		// Next, we'll launch a goroutine to wait until the closing
		// transaction has been confirmed so we can mark the contract
		// as resolved in the database. This go routine is _not_ tracked
		// by the breach arbiter's wait group since the callback may not
		// be executed before shutdown, potentially leading to a
		// deadlocks as the arbiter may not be able to finish shutting
		// down.
		//
		// TODO(roasbeef): also notify utxoNursery, might've had
		// outbound HTLC's in flight
		go waitForChanToClose(uint32(closeInfo.SpendingHeight),
			b.cfg.Notifier, nil, chanPoint, closeInfo.SpenderTxHash,
			func() {
				// As we just detected a channel was closed via
				// a unilateral commitment broadcast by the
				// remote party, we'll need to sweep our main
				// commitment output, and any outstanding
				// outgoing HTLC we had as well.
				//
				// TODO(roasbeef): actually sweep HTLC's *
				// ensure reliable confirmation
				if closeInfo.SelfOutPoint != nil {
					sweepTx, err := b.craftCommitSweepTx(
						closeInfo,
					)
					if err != nil {
						brarLog.Errorf("unable to "+
							"generate sweep tx: %v",
							err)
						goto close
					}

					brarLog.Infof("Sweeping breached "+
						"outputs with: %v",
						spew.Sdump(sweepTx))

					err = b.cfg.PublishTransaction(sweepTx)
					if err != nil {
						brarLog.Errorf("unable to "+
							"broadcast tx: %v", err)
					}
				}

			close:
				brarLog.Infof("Force closed ChannelPoint(%v) "+
					"is fully closed, updating DB",
					chanPoint)

				err := b.cfg.DB.MarkChanFullyClosed(chanPoint)
				if err != nil {
					brarLog.Errorf("unable to mark chan "+
						"as closed: %v", err)
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
		b.cfg.CloseLink(chanPoint, htlcswitch.CloseBreach)

		// TODO(roasbeef): need to handle case of remote broadcast
		// mid-local initiated state-transition, possible
		// false-positive?

		// Obtain a snapshot of the final channel state, which can be
		// used to reclose a breached channel in the event of a failure.
		chanInfo := contract.StateSnapshot()

		// Using the breach information provided by the wallet and the
		// channel snapshot, construct the retribution information that
		// will be persisted to disk.
		retInfo := newRetributionInfo(chanPoint, breachInfo, chanInfo)

		// Persist the pending retribution state to disk.
		if err := b.cfg.Store.Add(retInfo); err != nil {
			brarLog.Errorf("unable to persist retribution info "+
				"to db: %v", err)
		}

		// TODO(conner): move responsibility of channel closure into
		// lnwallet. Have breach arbiter ACK after writing to disk, then
		// have wallet mark channel as closed. This allows the wallet to
		// attempt to retransmit the breach info if the either arbiter
		// or the wallet goes down before completing the hand off.

		// Now that the breach arbiter has persisted the information,
		// we can go ahead and mark the channel as closed in the
		// channeldb.  This step is done after persisting the
		// retribution information so that a failure between these steps
		// will cause an attempt to monitor the still-open channel.
		// However, since the retribution information was persisted
		// before, the arbiter will recognize that the channel should be
		// closed, and proceed to mark it as such after a restart, and
		// forgo monitoring it for breaches.

		// Construct the breached channel's close summary marking the
		// channel using the snapshot from before, and marking this as a
		// BreachClose.
		closeInfo := &channeldb.ChannelCloseSummary{
			ChanPoint:      *chanPoint,
			ChainHash:      breachInfo.ChainHash,
			ClosingTXID:    breachInfo.BreachTransaction.TxHash(),
			RemotePub:      &chanInfo.RemoteIdentity,
			Capacity:       chanInfo.Capacity,
			SettledBalance: chanInfo.LocalBalance.ToSatoshis(),
			CloseType:      channeldb.BreachClose,
			IsPending:      true,
		}

		// Next, persist the channel close to disk. Upon restart, the
		// arbiter will recognize that this channel has been breached
		// and marked close, and fast track its path to justice.
		if err := contract.DeleteState(closeInfo); err != nil {
			brarLog.Errorf("unable to delete channel state: %v",
				err)
		}

		// Finally, we send the retribution information into the
		// breachArbiter event loop to deal swift justice.
		select {
		case b.breachedContracts <- retInfo:
		case <-b.quit:
		}

	case <-b.quit:
		return
	}
}

// SpendableOutput an interface which can be used by the breach arbiter to
// construct a transaction spending from outputs we control.
type SpendableOutput interface {
	// Amount returns the number of satoshis contained within the output.
	Amount() btcutil.Amount

	// Outpoint returns the reference to the output being spent, used to
	// construct the corresponding transaction input.
	OutPoint() *wire.OutPoint

	// WitnessType returns an enum specifying the type of witness that must
	// be generated in order to spend this output.
	WitnessType() lnwallet.WitnessType

	// SignDesc returns a reference to a spendable output's sign descriptor,
	// which is used during signing to compute a valid witness that spends
	// this output.
	SignDesc() *lnwallet.SignDescriptor

	// BuildWitness returns a valid witness allowing this output to be
	// spent, the witness should be attached to the transaction at the
	// location determined by the given `txinIdx`.
	BuildWitness(signer lnwallet.Signer, txn *wire.MsgTx,
		hashCache *txscript.TxSigHashes,
		txinIdx int) ([][]byte, error)
}

// breachedOutput contains all the information needed to sweep a breached
// output. A breached output is an output that we are now entitled to due to a
// revoked commitment transaction being broadcast.
type breachedOutput struct {
	amt         btcutil.Amount
	outpoint    wire.OutPoint
	witnessType lnwallet.WitnessType
	signDesc    lnwallet.SignDescriptor

	witnessFunc lnwallet.WitnessGenerator
}

// makeBreachedOutput assembles a new breachedOutput that can be used by the
// breach arbiter to construct a justice or sweep transaction.
func makeBreachedOutput(outpoint *wire.OutPoint,
	witnessType lnwallet.WitnessType,
	signDescriptor *lnwallet.SignDescriptor) breachedOutput {

	amount := signDescriptor.Output.Value

	return breachedOutput{
		amt:         btcutil.Amount(amount),
		outpoint:    *outpoint,
		witnessType: witnessType,
		signDesc:    *signDescriptor,
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
func (bo *breachedOutput) WitnessType() lnwallet.WitnessType {
	return bo.witnessType
}

// SignDesc returns the breached output's SignDescriptor, which is used during
// signing to compute the witness.
func (bo *breachedOutput) SignDesc() *lnwallet.SignDescriptor {
	return &bo.signDesc
}

// BuildWitness computes a valid witness that allows us to spend from the
// breached output. It does so by first generating and memoizing the witness
// generation function, which parameterized primarily by the witness type and
// sign descriptor. The method then returns the witness computed by invoking
// this function on the first and subsequent calls.
func (bo *breachedOutput) BuildWitness(signer lnwallet.Signer,
	txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes,
	txinIdx int) ([][]byte, error) {

	// First, we ensure that the witness generation function has
	// been initialized for this breached output.
	if bo.witnessFunc == nil {
		bo.witnessFunc = bo.witnessType.GenWitnessFunc(
			signer, bo.SignDesc())
	}

	// Now that we have ensured that the witness generation function has
	// been initialized, we can proceed to execute it and generate the
	// witness for this particular breached output.
	return bo.witnessFunc(txn, hashCache, txinIdx)
}

// Add compile-time constraint ensuring breachedOutput implements
// SpendableOutput.
var _ SpendableOutput = (*breachedOutput)(nil)

// retributionInfo encapsulates all the data needed to sweep all the contested
// funds within a channel whose contract has been breached by the prior
// counterparty. This struct is used to create the justice transaction which
// spends all outputs of the commitment transaction into an output controlled
// by the wallet.
type retributionInfo struct {
	commitHash chainhash.Hash
	chanPoint  wire.OutPoint
	chainHash  chainhash.Hash

	// TODO(conner): remove the following group of fields after decoupling
	// the breach arbiter from the wallet.

	// Fields copied from channel snapshot when a breach is detected. This
	// is necessary for deterministically constructing the channel close
	// summary in the event that the breach arbiter crashes before closing
	// the channel.
	remoteIdentity *btcec.PublicKey
	capacity       btcutil.Amount
	settledBalance btcutil.Amount

	breachedOutputs []breachedOutput
}

// newRetributionInfo constructs a retributionInfo containing all the
// information required by the breach arbiter to recover funds from breached
// channels.  The information is primarily populated using the BreachRetribution
// delivered by the wallet when it detects a channel breach.
func newRetributionInfo(chanPoint *wire.OutPoint,
	breachInfo *lnwallet.BreachRetribution,
	chanInfo *channeldb.ChannelSnapshot) *retributionInfo {

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
			lnwallet.CommitmentNoDelay,
			breachInfo.LocalOutputSignDesc)

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
			lnwallet.CommitmentRevoke,
			breachInfo.RemoteOutputSignDesc)

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
		var htlcWitnessType lnwallet.WitnessType
		if breachedHtlc.IsIncoming {
			htlcWitnessType = lnwallet.HtlcAcceptedRevoke
		} else {
			htlcWitnessType = lnwallet.HtlcOfferedRevoke
		}

		htlcOutput := makeBreachedOutput(
			&breachInfo.HtlcRetributions[i].OutPoint,
			htlcWitnessType,
			&breachInfo.HtlcRetributions[i].SignDesc)

		breachedOutputs = append(breachedOutputs, htlcOutput)
	}

	// TODO(conner): remove dependency on channel snapshot after decoupling
	// channel closure from the breach arbiter.

	return &retributionInfo{
		commitHash:      breachInfo.BreachTransaction.TxHash(),
		chainHash:       chanInfo.ChainHash,
		chanPoint:       *chanPoint,
		remoteIdentity:  &chanInfo.RemoteIdentity,
		capacity:        chanInfo.Capacity,
		settledBalance:  chanInfo.LocalBalance.ToSatoshis(),
		breachedOutputs: breachedOutputs,
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
		spendableOutputs []SpendableOutput
		weightEstimate   lnwallet.TxWeightEstimator
	)

	// Allocate enough space to potentially hold each of the breached
	// outputs in the retribution info.
	spendableOutputs = make([]SpendableOutput, 0, len(r.breachedOutputs))

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
		input := &r.breachedOutputs[i]

		// First, select the appropriate estimated witness weight for
		// the give witness type of this breached output. If the witness
		// type is unrecognized, we will omit it from the transaction.
		var witnessWeight int
		switch input.WitnessType() {
		case lnwallet.CommitmentNoDelay:
			witnessWeight = lnwallet.ToLocalPenaltyWitnessSize

		case lnwallet.CommitmentRevoke:
			witnessWeight = lnwallet.P2WKHWitnessSize

		case lnwallet.HtlcOfferedRevoke:
			witnessWeight = lnwallet.OfferedHtlcPenaltyWitnessSize

		case lnwallet.HtlcAcceptedRevoke:
			witnessWeight = lnwallet.AcceptedHtlcPenaltyWitnessSize

		default:
			brarLog.Warnf("breached output in retribution info "+
				"contains unexpected witness type: %v",
				input.WitnessType())
			continue
		}
		weightEstimate.AddWitnessInput(witnessWeight)

		// Finally, append this input to our list of spendable outputs.
		spendableOutputs = append(spendableOutputs, input)
	}

	txWeight := uint64(weightEstimate.Weight())
	return b.sweepSpendableOutputsTxn(txWeight, spendableOutputs...)
}

// craftCommitmentSweepTx creates a transaction to sweep the non-delayed output
// within the commitment transaction that pays to us. We must manually sweep
// this output as it uses a tweaked public key in its pkScript, so the wallet
// won't immediacy be aware of it.
//
// TODO(roasbeef): alternative options
//  * leave the output in the chain, use as input to future funding tx
//  * leave output in the chain, extend wallet to add knowledge of how to claim
func (b *breachArbiter) craftCommitSweepTx(
	closeInfo *lnwallet.UnilateralCloseSummary) (*wire.MsgTx, error) {

	selfOutput := makeBreachedOutput(
		closeInfo.SelfOutPoint,
		lnwallet.CommitmentNoDelay,
		closeInfo.SelfOutputSignDesc,
	)

	// Compute the transaction weight of the commit sweep transaction, which
	// includes a single input and output.
	var weightEstimate lnwallet.TxWeightEstimator
	weightEstimate.AddP2WKHOutput()

	// Add to_local p2wpkh witness and tx input.
	weightEstimate.AddP2WKHInput()

	txWeight := uint64(weightEstimate.Weight())
	return b.sweepSpendableOutputsTxn(txWeight, &selfOutput)
}

// sweepSpendableOutputsTxn creates a signed transaction from a sequence of
// spendable outputs by sweeping the funds into a single p2wkh output.
func (b *breachArbiter) sweepSpendableOutputsTxn(txWeight uint64,
	inputs ...SpendableOutput) (*wire.MsgTx, error) {

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
		totalAmt += input.Amount()
	}

	// We'll actually attempt to target inclusion within the next two
	// blocks as we'd like to sweep these funds back into our wallet ASAP.
	feePerWeight, err := b.cfg.Estimator.EstimateFeePerWeight(2)
	if err != nil {
		return nil, err
	}
	txFee := btcutil.Amount(txWeight * uint64(feePerWeight))

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
	addWitness := func(idx int, so SpendableOutput) error {
		// First, we construct a valid witness for this outpoint and
		// transaction using the SpendableOutput's witness generation
		// function.
		witness, err := so.BuildWitness(b.cfg.Signer, txn, hashCache,
			idx)
		if err != nil {
			return err
		}

		// Then, we add the witness to the transaction at the
		// appropriate txin index.
		txn.TxIn[idx].Witness = witness

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
	// entires found under the same key, and an error should be raised if
	// the addition fails.
	Add(retInfo *retributionInfo) error

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
	return rs.db.Update(func(tx *bolt.Tx) error {
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

// Remove removes a retribution state from the retributionStore database.
func (rs *retributionStore) Remove(key *wire.OutPoint) error {
	return rs.db.Update(func(tx *bolt.Tx) error {
		retBucket := tx.Bucket(retributionBucket)

		// We return an error if the bucket is not already created,
		// since normal operation of the breach arbiter should never try
		// to remove a finalized retribution state that is not already
		// stored in the db.
		if retBucket == nil {
			return errors.New("unable to remove retribution " +
				"because the db bucket doesn't exist.")
		}

		var outBuf bytes.Buffer
		if err := writeOutpoint(&outBuf, key); err != nil {
			return err
		}

		return retBucket.Delete(outBuf.Bytes())
	})
}

// ForAll iterates through all stored retributions and executes the passed
// callback function on each retribution.
func (rs *retributionStore) ForAll(cb func(*retributionInfo) error) error {
	return rs.db.View(func(tx *bolt.Tx) error {
		// If the bucket does not exist, then there are no pending
		// retributions.
		retBucket := tx.Bucket(retributionBucket)
		if retBucket == nil {
			return nil
		}

		// Otherwise, we fetch each serialized retribution info,
		// deserialize it, and execute the passed in callback function
		// on it.
		return retBucket.ForEach(func(outBytes, retBytes []byte) error {
			ret := &retributionInfo{}
			if err := ret.Decode(
				bytes.NewBuffer(retBytes),
			); err != nil {
				return err
			}

			return cb(ret)
		})
	})
}

// Encode serializes the retribution into the passed byte stream.
func (ret *retributionInfo) Encode(w io.Writer) error {
	var scratch [8]byte

	if _, err := w.Write(ret.commitHash[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, &ret.chanPoint); err != nil {
		return err
	}

	if _, err := w.Write(ret.chainHash[:]); err != nil {
		return err
	}

	if _, err := w.Write(
		ret.remoteIdentity.SerializeCompressed()); err != nil {
		return err
	}

	binary.BigEndian.PutUint64(scratch[:8], uint64(ret.capacity))
	if _, err := w.Write(scratch[:8]); err != nil {
		return err
	}

	binary.BigEndian.PutUint64(scratch[:8], uint64(ret.settledBalance))
	if _, err := w.Write(scratch[:8]); err != nil {
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
	var scratch [33]byte

	if _, err := io.ReadFull(r, scratch[:32]); err != nil {
		return err
	}
	hash, err := chainhash.NewHash(scratch[:32])
	if err != nil {
		return err
	}
	ret.commitHash = *hash

	if err := readOutpoint(r, &ret.chanPoint); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, scratch[:32]); err != nil {
		return err
	}
	chainHash, err := chainhash.NewHash(scratch[:32])
	if err != nil {
		return err
	}
	ret.chainHash = *chainHash

	if _, err = io.ReadFull(r, scratch[:33]); err != nil {
		return err
	}
	remoteIdentity, err := btcec.ParsePubKey(scratch[:33], btcec.S256())
	if err != nil {
		return err
	}
	ret.remoteIdentity = remoteIdentity

	if _, err := io.ReadFull(r, scratch[:8]); err != nil {
		return err
	}
	ret.capacity = btcutil.Amount(binary.BigEndian.Uint64(scratch[:8]))

	if _, err := io.ReadFull(r, scratch[:8]); err != nil {
		return err
	}
	ret.settledBalance = btcutil.Amount(
		binary.BigEndian.Uint64(scratch[:8]))

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

	if err := lnwallet.WriteSignDescriptor(w, &bo.signDesc); err != nil {
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

	if err := lnwallet.ReadSignDescriptor(r, &bo.signDesc); err != nil {
		return err
	}

	if _, err := io.ReadFull(r, scratch[:2]); err != nil {
		return err
	}
	bo.witnessType = lnwallet.WitnessType(
		binary.BigEndian.Uint16(scratch[:2]))

	return nil
}
