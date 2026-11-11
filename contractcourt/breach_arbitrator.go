package contractcourt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// justiceTxConfTarget is the number of blocks we'll use as a
	// confirmation target when creating the justice transaction. We'll
	// choose an aggressive target, since we want to be sure it confirms
	// quickly.
	justiceTxConfTarget = 2

	// blocksPassedSplitPublish is the number of blocks without
	// confirmation of the justice tx we'll wait before starting to publish
	// smaller variants of the justice tx. We do this to mitigate an attack
	// the channel peer can do by pinning the HTLC outputs of the
	// commitment with low-fee HTLC transactions.
	blocksPassedSplitPublish = 4
)

var (
	// retributionBucket stores retribution state on disk between detecting
	// a contract breach, broadcasting a justice transaction that sweeps the
	// channel, and finally witnessing the justice transaction confirm on
	// the blockchain. It is critical that such state is persisted on disk,
	// so that if our node restarts at any point during the retribution
	// procedure, we can recover and continue from the persisted state.
	retributionBucket = []byte("retribution")

	// taprootRetributionBucket stores the tarpoot specific retribution
	// information. This includes things like the control blocks for both
	// commitment outputs, and the taptweak needed to sweep each HTLC (one
	// for the first and one for the second level).
	taprootRetributionBucket = []byte("tap-retribution")

	// errBrarShuttingDown is an error returned if the BreachArbitrator has
	// been signalled to exit.
	errBrarShuttingDown = errors.New("BreachArbitrator shutting down")

	// errSecondLevelOutputNotFound is returned when the second-level
	// output of a breached aux channel HTLC cannot be located in the
	// transaction that took the HTLC to the second level.
	errSecondLevelOutputNotFound = errors.New("no unclaimed output " +
		"matching the expected second-level taproot pkScript")
)

// ContractBreachEvent is an event the BreachArbitrator will receive in case a
// contract breach is observed on-chain. It contains the necessary information
// to handle the breach, and a ProcessACK closure we will use to ACK the event
// when we have safely stored all the necessary information.
type ContractBreachEvent struct {
	// ChanPoint is the channel point of the breached channel.
	ChanPoint wire.OutPoint

	// ProcessACK is an closure that should be called with a nil error iff
	// the breach retribution info is safely stored in the retribution
	// store. In case storing the information to the store fails, a non-nil
	// error should be used. When this closure returns, it means that the
	// contract court has marked the channel pending close in the DB, and
	// it is safe for the BreachArbitrator to carry on its duty.
	ProcessACK func(error)

	// BreachRetribution is the information needed to act on this contract
	// breach.
	BreachRetribution *lnwallet.BreachRetribution
}

// ChannelCloseType is an enum which signals the type of channel closure the
// peer should execute.
type ChannelCloseType uint8

const (
	// CloseRegular indicates a regular cooperative channel closure
	// should be attempted.
	CloseRegular ChannelCloseType = iota

	// CloseBreach indicates that a channel breach has been detected, and
	// the link should immediately be marked as unavailable.
	CloseBreach
)

// RetributionStorer provides an interface for managing a persistent map from
// wire.OutPoint -> retributionInfo. Upon learning of a breach, a
// BreachArbitrator should record the retributionInfo for the breached channel,
// which serves a checkpoint in the event that retribution needs to be resumed
// after failure. A RetributionStore provides an interface for managing the
// persisted set, as well as mapping user defined functions over the entire
// on-disk contents.
//
// Calls to RetributionStore may occur concurrently. A concrete instance of
// RetributionStore should use appropriate synchronization primitives, or
// be otherwise safe for concurrent access.
type RetributionStorer interface {
	// Add persists the retributionInfo to disk, using the information's
	// chanPoint as the key. This method should overwrite any existing
	// entries found under the same key, and an error should be raised if
	// the addition fails.
	Add(retInfo *retributionInfo) error

	// IsBreached queries the retribution store to see if the breach arbiter
	// is aware of any breaches for the provided channel point.
	IsBreached(chanPoint *wire.OutPoint) (bool, error)

	// Remove deletes the retributionInfo from disk, if any exists, under
	// the given key. An error should be re raised if the removal fails.
	Remove(key *wire.OutPoint) error

	// ForAll iterates over the existing on-disk contents and applies a
	// chosen, read-only callback to each. This method should ensure that it
	// immediately propagate any errors generated by the callback.
	ForAll(cb func(*retributionInfo) error, reset func()) error

	// RecordJusticeTxs persists the metadata of justice transactions
	// created for the breach of the given channel, keyed by txid, so the
	// aux sweeper can still be told about the one that confirms after a
	// restart. It is only called for channels that carry aux resolution
	// data.
	RecordJusticeTxs(chanPoint *wire.OutPoint,
		justiceTxs map[chainhash.Hash]*justiceTxCtx) error
}

// BreachConfig bundles the required subsystems used by the breach arbiter. An
// instance of BreachConfig is passed to NewBreachArbitrator during
// instantiation.
type BreachConfig struct {
	// CloseLink allows the breach arbiter to shutdown any channel links for
	// which it detects a breach, ensuring now further activity will
	// continue across the link. The method accepts link's channel point and
	// a close type to be included in the channel close summary.
	CloseLink func(*wire.OutPoint, ChannelCloseType)

	// DB provides access to the user's closed channels, allowing the breach
	// arbiter to determine how it should respond to channel closure.
	DB chanstate.ClosedChannelStore

	// Estimator is used by the breach arbiter to determine an appropriate
	// fee level when generating, signing, and broadcasting sweep
	// transactions.
	Estimator chainfee.Estimator

	// GenSweepScript generates the receiving scripts for swept outputs.
	GenSweepScript func() fn.Result[lnwallet.AddrWithKey]

	// Notifier provides a publish/subscribe interface for event driven
	// notifications regarding the confirmation of txids.
	Notifier chainntnfs.ChainNotifier

	// PublishTransaction facilitates the process of broadcasting a
	// transaction to the network.
	PublishTransaction func(*wire.MsgTx, string) error

	// ContractBreaches is a channel where the BreachArbitrator will receive
	// notifications in the event of a contract breach being observed. A
	// ContractBreachEvent must be ACKed by the BreachArbitrator, such that
	// the sending subsystem knows that the event is properly handed off.
	ContractBreaches <-chan *ContractBreachEvent

	// Signer is used by the breach arbiter to generate sweep transactions,
	// which move coins from previously open channels back to the user's
	// wallet.
	Signer input.Signer

	// Store is a persistent resource that maintains information regarding
	// breached channels. This is used in conjunction with DB to recover
	// from crashes, restarts, or other failures.
	Store RetributionStorer

	// AuxSweeper is an optional interface that can be used to modify the
	// way sweep transaction are generated.
	AuxSweeper fn.Option[sweep.AuxSweeper]

	// AuxResolver is an optional interface for resolving auxiliary
	// contract data. Used to generate resolution blobs for second-level
	// HTLC revocations at morph time.
	AuxResolver fn.Option[lnwallet.AuxContractResolver]
}

// BreachArbitrator is a special subsystem which is responsible for watching and
// acting on the detection of any attempted uncooperative channel breaches by
// channel counterparties. This file essentially acts as deterrence code for
// those attempting to launch attacks against the daemon. In practice it's
// expected that the logic in this file never gets executed, but it is
// important to have it in place just in case we encounter cheating channel
// counterparties.
// TODO(roasbeef): closures in config for subsystem pointers to decouple?
type BreachArbitrator struct {
	started sync.Once
	stopped sync.Once

	cfg *BreachConfig

	subscriptions map[wire.OutPoint]chan struct{}

	quit chan struct{}
	wg   sync.WaitGroup
	sync.Mutex
}

// NewBreachArbitrator creates a new instance of a BreachArbitrator initialized
// with its dependent objects.
func NewBreachArbitrator(cfg *BreachConfig) *BreachArbitrator {
	return &BreachArbitrator{
		cfg:           cfg,
		subscriptions: make(map[wire.OutPoint]chan struct{}),
		quit:          make(chan struct{}),
	}
}

// Start is an idempotent method that officially starts the BreachArbitrator
// along with all other goroutines it needs to perform its functions.
func (b *BreachArbitrator) Start() error {
	var err error
	b.started.Do(func() {
		brarLog.Info("Breach arbiter starting")
		err = b.start()
	})
	return err
}

func (b *BreachArbitrator) start() error {
	// Load all retributions currently persisted in the retribution store.
	var breachRetInfos map[wire.OutPoint]retributionInfo
	if err := b.cfg.Store.ForAll(func(ret *retributionInfo) error {
		breachRetInfos[ret.chanPoint] = *ret
		return nil
	}, func() {
		breachRetInfos = make(map[wire.OutPoint]retributionInfo)
	}); err != nil {
		brarLog.Errorf("Unable to create retribution info: %v", err)
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

	brarLog.Debugf("Found %v closing channels, %v retribution records",
		len(closedChans), len(breachRetInfos))

	// Using the set of non-pending, closed channels, reconcile any
	// discrepancies between the channeldb and the retribution store by
	// removing any retribution information for which we have already
	// finished our responsibilities. If the removal is successful, we also
	// remove the entry from our in-memory map, to avoid any further action
	// for this channel.
	// TODO(halseth): no need continue on IsPending once closed channels
	// actually means close transaction is confirmed.
	for _, chanSummary := range closedChans {
		brarLog.Debugf("Working on close channel: %v, is_pending: %v",
			chanSummary.ChanPoint, chanSummary.IsPending)

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

			brarLog.Debugf("Skipped closed channel: %v",
				chanSummary.ChanPoint)
		}
	}

	// Spawn the exactRetribution tasks to monitor and resolve any breaches
	// that were loaded from the retribution store.
	for chanPoint := range breachRetInfos {
		retInfo := breachRetInfos[chanPoint]

		brarLog.Debugf("Handling breach handoff on startup "+
			"for ChannelPoint(%v)", chanPoint)

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

// Stop is an idempotent method that signals the BreachArbitrator to execute a
// graceful shutdown. This function will block until all goroutines spawned by
// the BreachArbitrator have gracefully exited.
func (b *BreachArbitrator) Stop() error {
	b.stopped.Do(func() {
		brarLog.Infof("Breach arbiter shutting down...")
		defer brarLog.Debug("Breach arbiter shutdown complete")

		close(b.quit)
		b.wg.Wait()
	})
	return nil
}

// IsBreached queries the breach arbiter's retribution store to see if it is
// aware of any channel breaches for a particular channel point.
func (b *BreachArbitrator) IsBreached(chanPoint *wire.OutPoint) (bool, error) {
	return b.cfg.Store.IsBreached(chanPoint)
}

// SubscribeBreachComplete is used by outside subsystems to be notified of a
// successful breach resolution.
func (b *BreachArbitrator) SubscribeBreachComplete(chanPoint *wire.OutPoint,
	c chan struct{}) (bool, error) {

	breached, err := b.cfg.Store.IsBreached(chanPoint)
	if err != nil {
		// If an error occurs, no subscription will be registered.
		return false, err
	}

	if !breached {
		// If chanPoint no longer exists in the Store, then the breach
		// was cleaned up successfully. Any subscription that occurs
		// happens after the breach information was persisted to the
		// underlying store.
		return true, nil
	}

	// Otherwise since the channel point is not resolved, add a
	// subscription. There can only be one subscription per channel point.
	b.Lock()
	defer b.Unlock()
	b.subscriptions[*chanPoint] = c

	return false, nil
}

// notifyBreachComplete is used by the BreachArbitrator to notify outside
// subsystems that the breach resolution process is complete.
func (b *BreachArbitrator) notifyBreachComplete(chanPoint *wire.OutPoint) {
	b.Lock()
	defer b.Unlock()
	if c, ok := b.subscriptions[*chanPoint]; ok {
		close(c)
	}

	// Remove the subscription.
	delete(b.subscriptions, *chanPoint)
}

// contractObserver is the primary goroutine for the BreachArbitrator. This
// goroutine is responsible for handling breach events coming from the
// contractcourt on the ContractBreaches channel. If a channel breach is
// detected, then the contractObserver will execute the retribution logic
// required to sweep ALL outputs from a contested channel into the daemon's
// wallet.
//
// NOTE: This MUST be run as a goroutine.
func (b *BreachArbitrator) contractObserver() {
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

// spend is used to wrap the index of the retributionInfo output that gets
// spent together with the spend details.
type spend struct {
	index  int
	detail *chainntnfs.SpendDetail
}

// waitForSpendEvent waits for any of the breached outputs to get spent, and
// returns the spend details for those outputs. The spendNtfns map is a cache
// used to store registered spend subscriptions, in case we must call this
// method multiple times.
func (b *BreachArbitrator) waitForSpendEvent(breachInfo *retributionInfo,
	spendNtfns map[wire.OutPoint]*chainntnfs.SpendEvent) ([]spend, error) {

	inputs := breachInfo.breachedOutputs

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
					return nil, errBrarShuttingDown
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

		// Gather all detected spends and return them.
		var spends []spend
		for s := range allSpends {
			breachedOutput := &inputs[s.index]
			delete(spendNtfns, breachedOutput.outpoint)

			spends = append(spends, s)
		}

		return spends, nil

	case <-b.quit:
		return nil, errBrarShuttingDown
	}
}

// findSecondLevelOutputIndex searches the spending (second-level) transaction's
// outputs for the one that matches the expected second-level HTLC output
// script. This is necessary because SpenderInputIndex (which identifies the
// input that spent our output) does NOT necessarily correspond to the output
// index: the second-level tx may contain additional inputs (e.g. wallet UTXOs
// for fees) that shift the input indices.
//
// Several HTLCs of one commitment can share a second-level script, as it only
// depends on keys, the CSV delay and the aux leaf. The script alone then does
// not tell their outputs apart, so outputs that already belong to another
// breached output (as reported by claimed) are skipped, and among the rest
// the output at the index of the spending input is preferred, which is where
// a second-level tx that pairs inputs with outputs puts it.
func findSecondLevelOutputIndex(bo *breachedOutput, spendingTx *wire.MsgTx,
	spenderInputIndex uint32,
	claimed func(wire.OutPoint) bool) (uint32, error) {

	isTaproot := txscript.IsPayToTaproot(bo.signDesc.Output.PkScript)
	if !isTaproot || bo.signDesc.DoubleTweak == nil {
		return 0, fmt.Errorf("cannot derive expected pkScript: " +
			"non-taproot or missing DoubleTweak")
	}

	// For taproot outputs we derive the expected pkScript from the
	// revocation key and the second-level tap tweak.
	//
	// Derive the revocation public key from the revocation base point
	// and the commitment secret (DoubleTweak).
	commitPoint := bo.signDesc.DoubleTweak.PubKey()
	revokeKey := input.DeriveRevocationPubkey(
		bo.signDesc.KeyDesc.PubKey, commitPoint,
	)

	// Compute the expected taproot output key using the revocation key
	// as internal key and the second-level tap tweak.
	expectedOutputKey := txscript.ComputeTaprootOutputKey(
		revokeKey, bo.secondLevelTapTweak[:],
	)
	expectedPkScript, err := input.PayToTaprootScript(expectedOutputKey)
	if err != nil {
		return 0, fmt.Errorf("unable to compute expected pkScript: %w",
			err)
	}

	// An output is a candidate if it carries the expected script and no
	// other breached output has been assigned to it yet.
	txHash := spendingTx.TxHash()
	isCandidate := func(idx uint32) bool {
		if int(idx) >= len(spendingTx.TxOut) {
			return false
		}

		txOut := spendingTx.TxOut[idx]
		if !bytes.Equal(txOut.PkScript, expectedPkScript) {
			return false
		}

		return !claimed(wire.OutPoint{Hash: txHash, Index: idx})
	}

	if isCandidate(spenderInputIndex) {
		return spenderInputIndex, nil
	}
	for i := range spendingTx.TxOut {
		if isCandidate(uint32(i)) {
			return uint32(i), nil
		}
	}

	// Log details to help diagnose why no match was found.
	brarLog.Warnf("Expected second-level pkScript %x (revokeKey=%x, "+
		"tapTweak=%x) not found among the unclaimed outputs of the "+
		"%d outputs of tx %s", expectedPkScript,
		revokeKey.SerializeCompressed(), bo.secondLevelTapTweak[:],
		len(spendingTx.TxOut), txHash)
	for i, txOut := range spendingTx.TxOut {
		brarLog.Warnf("  output %d: pkScript=%x value=%d",
			i, txOut.PkScript, txOut.Value)
	}

	return 0, fmt.Errorf("%w in tx %s", errSecondLevelOutputNotFound,
		txHash)
}

// convertToSecondLevelRevoke takes a breached output, and a transaction that
// spends it to the second level, and mutates the breach output into one that
// is able to properly sweep that second level output. We'll use this function
// when we go to sweep a breached commitment transaction, but the cheating
// party has already attempted to take it to the second level.
//
// Everything is worked out first and only then committed to the breached
// output, so an error leaves the output exactly as it was. The only error is
// errSecondLevelOutputNotFound, returned when the second-level output of an
// aux channel HTLC cannot be located in the spending transaction.
func convertToSecondLevelRevoke(bo *breachedOutput, breachInfo *retributionInfo,
	spendDetails *chainntnfs.SpendDetail,
	auxResolver fn.Option[lnwallet.AuxContractResolver]) error {

	// In this case, we'll modify the witness type of this output to
	// actually prepare for a second level revoke.
	witnessType := input.HtlcSecondLevelRevoke
	if txscript.IsPayToTaproot(bo.signDesc.Output.PkScript) {
		witnessType = input.TaprootHtlcSecondLevelRevoke
	}

	// Find the correct output index in the spending (second-level) tx.
	//
	// On aux (taproot-asset) channels the second-level tx may bundle
	// multiple HTLCs and/or wallet UTXOs for fees, so input index no
	// longer matches output index. We script-match our expected
	// second-level pkScript to locate the right output.
	//
	// On non-aux channels (regular legacy/anchor/taproot, including
	// non-custom simple taproot) we keep the legacy behavior of using
	// SpenderInputIndex directly, so we don't regress sweep behavior on
	// normal channels.
	//
	// resolveReq is a reliable aux-channel discriminator here: it is
	// populated in createHtlcRetribution only for channels with a
	// tapscript root (aux/custom channels), and restored from the
	// retribution store after a restart, so a nil resolveReq
	// unambiguously means "not a custom channel" and takes the legacy
	// path below.
	spendingTx := spendDetails.SpendingTx
	isAuxChannel := bo.resolveReq != nil

	outputIndex := spendDetails.SpenderInputIndex
	if isAuxChannel {
		idx, err := findSecondLevelOutputIndex(
			bo, spendingTx, spendDetails.SpenderInputIndex,
			breachInfo.claimsOutpoint,
		)
		if err != nil {
			return err
		}
		outputIndex = idx
	}

	// Re-resolve the aux contract blob for the second-level witness
	// type. The blob we hold was resolved for the output on the
	// commitment, which is spent now, so it must not be carried over to
	// the second-level output. If the resolver fails, the output is
	// still converted and swept for its BTC value, just without a blob:
	// one HTLC the resolver cannot handle must not hold up the justice
	// transactions of all the other outputs.
	resolutionBlob := bo.resolutionBlob
	if isAuxChannel {
		blob, err := reresolveAuxBlobForSecondLevel(
			bo, spendingTx, spendDetails, auxResolver,
		)
		if err != nil {
			brarLog.Errorf("Unable to re-resolve second-level "+
				"HTLC blob for %v of ChannelPoint(%v), "+
				"sweeping it without aux resolution: %v",
				bo.outpoint, breachInfo.chanPoint, err)
		}
		resolutionBlob = blob
	}

	// With everything worked out, commit the conversion.
	oldOp := bo.outpoint
	bo.witnessType = witnessType
	bo.outpoint = wire.OutPoint{
		Hash:  spendingTx.TxHash(),
		Index: outputIndex,
	}

	// Next, we need to update the amount so we can do fee estimation
	// properly, and also so we can generate a valid signature as we need
	// to know the new input value (the second level transactions shaves
	// off some funds to fees).
	newAmt := spendingTx.TxOut[outputIndex].Value
	bo.amt = btcutil.Amount(newAmt)
	bo.signDesc.Output.Value = newAmt
	bo.signDesc.Output.PkScript = spendingTx.TxOut[outputIndex].PkScript

	// For taproot outputs, the taptweak also needs to be swapped out. We
	// do this unconditionally as this field isn't used at all for segwit
	// v0 outputs. The tweak is copied out of the breached output, as a
	// slice over the array embedded in the struct keeps pointing at this
	// slot of the breached outputs slice. Once updateBreachInfo compacts
	// that slice, another output shifts into the slot and we would sign
	// with its tweak.
	bo.signDesc.TapTweak = bytes.Clone(bo.secondLevelTapTweak[:])

	// Finally, we'll need to adjust the witness program in the
	// SignDescriptor.
	bo.signDesc.WitnessScript = bo.secondLevelWitnessScript

	bo.resolutionBlob = resolutionBlob

	brarLog.Warnf("HTLC(%v) for ChannelPoint(%v) has been spent to the "+
		"second-level, adjusting -> %v", oldOp, breachInfo.chanPoint,
		bo.outpoint)

	return nil
}

// reresolveAuxBlobForSecondLevel re-runs the aux contract resolution against
// the now-known second-level transaction and returns the resolution blob that
// targets the second-level witness path. The breached output is not modified.
// On error no blob is returned: the one resolved for the commitment output
// does not apply to the second-level output.
func reresolveAuxBlobForSecondLevel(bo *breachedOutput,
	spendingTx *wire.MsgTx, spendDetails *chainntnfs.SpendDetail,
	auxResolver fn.Option[lnwallet.AuxContractResolver]) (
	fn.Option[tlv.Blob], error) {

	none := fn.None[tlv.Blob]()

	if bo.resolveReq == nil || bo.resolveReq.KeyRing == nil {
		return none, errors.New("no resolve request template")
	}

	secondLevelReq := *bo.resolveReq
	if bo.resolveReq.CommitTx != nil {
		secondLevelReq.CommitTx = bo.resolveReq.CommitTx.Copy()
	}
	secondLevelReq.Type = input.TaprootHtlcSecondLevelRevoke
	secondLevelReq.SecondLevel = fn.Some(lnwallet.SecondLevelInfo{
		Tx:          spendingTx,
		BlockHeight: uint32(spendDetails.SpendingHeight),
	})

	brarLog.Infof("Re-resolving HTLC blob for second-level revoke, "+
		"htlcID=%v, incoming=%v, htlcAmt=%v", secondLevelReq.HtlcID,
		secondLevelReq.HtlcIncoming, secondLevelReq.HtlcAmt)

	resolveBlob := fn.MapOptionZ(
		auxResolver,
		func(a lnwallet.AuxContractResolver) fn.Result[tlv.Blob] {
			return a.ResolveContract(secondLevelReq)
		},
	)
	if err := resolveBlob.Err(); err != nil {
		return none, err
	}

	return resolveBlob.OkToSome(), nil
}

// updateBreachInfo mutates the passed breachInfo by removing or converting any
// outputs among the spends. It also counts the total and revoked funds swept
// by our justice spends.
func updateBreachInfo(breachInfo *retributionInfo, spends []spend,
	auxResolver fn.Option[lnwallet.AuxContractResolver]) (
	btcutil.Amount, btcutil.Amount) {

	inputs := breachInfo.breachedOutputs
	doneOutputs := make(map[int]struct{})

	var totalFunds, revokedFunds btcutil.Amount
	for _, s := range spends {
		breachedOutput := &inputs[s.index]
		txIn := s.detail.SpendingTx.TxIn[s.detail.SpenderInputIndex]

		switch breachedOutput.witnessType {
		case input.TaprootHtlcAcceptedRevoke:
			fallthrough
		case input.TaprootHtlcOfferedRevoke:
			fallthrough
		case input.HtlcAcceptedRevoke:
			fallthrough
		case input.HtlcOfferedRevoke:
			// If the HTLC output was spent using the revocation
			// key, it is our own spend, and we can forget the
			// output. Otherwise it has been taken to the second
			// level.
			signDesc := &breachedOutput.signDesc
			ok, err := input.IsHtlcSpendRevoke(txIn, signDesc)
			if err != nil {
				brarLog.Errorf("Unable to determine if "+
					"revoke spend: %v", err)
				break
			}

			if ok {
				brarLog.Debugf("HTLC spend was our own " +
					"revocation spend")
				break
			}

			brarLog.Infof("Spend on second-level "+
				"%s(%v) for ChannelPoint(%v) "+
				"transitions to second-level output",
				breachedOutput.witnessType,
				breachedOutput.outpoint, breachInfo.chanPoint)

			// In this case we'll morph our initial revoke
			// spend to instead point to the second level
			// output, and update the sign descriptor in the
			// process.
			err = convertToSecondLevelRevoke(
				breachedOutput, breachInfo, s.detail,
				auxResolver,
			)
			if err == nil {
				continue
			}

			// The second-level output could not be located, so
			// there is nothing we could sweep for this HTLC
			// anymore. The output is dropped so the remaining
			// ones can proceed, but its value is not reported as
			// claimed: it was not.
			brarLog.Errorf("BUG: abandoning %s(%v) of "+
				"ChannelPoint(%v), spent to the second level "+
				"by %v: %v", breachedOutput.witnessType,
				breachedOutput.outpoint, breachInfo.chanPoint,
				s.detail.SpenderTxHash, err)

			doneOutputs[s.index] = struct{}{}

			continue
		}

		// Now that we have determined the spend is done by us, we
		// count the total and revoked funds swept depending on the
		// input type.
		switch breachedOutput.witnessType {
		// If the output being revoked is the remote commitment output
		// or an offered HTLC output, its amount contributes to the
		// value of funds being revoked from the counter party.
		case input.CommitmentRevoke, input.TaprootCommitmentRevoke,
			input.TaprootCommitmentRevokeFinal,
			input.HtlcSecondLevelRevoke,
			input.TaprootHtlcSecondLevelRevoke,
			input.TaprootHtlcOfferedRevoke, input.HtlcOfferedRevoke:

			revokedFunds += breachedOutput.Amount()
		}

		totalFunds += breachedOutput.Amount()
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
	return totalFunds, revokedFunds
}

// confirmedJusticeTx is a justice transaction of ours that confirmed and
// that the aux sweeper has not been told about yet.
type confirmedJusticeTx struct {
	// tx is the confirmed transaction.
	tx *wire.MsgTx

	// height is the height the transaction confirmed at.
	height uint32

	// ctx is the metadata recorded when the transaction was created.
	ctx *justiceTxCtx

	// inputs holds a copy of every breached output the transaction
	// spends, taken as the spends are detected, keyed by outpoint. The
	// aux sweeper is only notified once all inputs of the transaction
	// are collected.
	inputs map[wire.OutPoint]*breachedOutput
}

// complete returns true once every input of the transaction is collected.
func (c *confirmedJusticeTx) complete() bool {
	return len(c.inputs) == len(c.tx.TxIn)
}

// justiceTxTracker keeps track of the justice transactions created for a
// breach and of which confirmed ones the aux sweeper still has to be told
// about. The aux sweeper builds its proofs from a justice transaction only
// once it confirmed, so each confirmed justice transaction must be reported
// to it exactly once, with all of its inputs, and reporting must survive both
// a failure of the aux sweeper and a restart.
type justiceTxTracker struct {
	// history holds the metadata of every justice tx created for the
	// breach, keyed by txid. The variants are rebuilt every time an
	// output is spent or morphed, and the transaction that ends up
	// confirming may stem from any earlier rebuild, or from before a
	// restart.
	history map[chainhash.Hash]*justiceTxCtx

	// notified is the set of confirmed justice txs the aux sweeper was
	// successfully told about.
	notified map[chainhash.Hash]struct{}

	// pending holds the confirmed justice txs the aux sweeper has not
	// been told about yet, either because not all of their inputs have
	// been collected or because telling it failed and is to be retried.
	pending map[chainhash.Hash]*confirmedJusticeTx
}

// newJusticeTxTracker creates a tracker seeded with the justice txs that
// were recorded for the breach before a restart, if any.
func newJusticeTxTracker(
	recorded map[chainhash.Hash]*justiceTxCtx) *justiceTxTracker {

	history := make(map[chainhash.Hash]*justiceTxCtx, len(recorded))
	for txid, jtx := range recorded {
		history[txid] = jtx
	}

	return &justiceTxTracker{
		history:  history,
		notified: make(map[chainhash.Hash]struct{}),
		pending:  make(map[chainhash.Hash]*confirmedJusticeTx),
	}
}

// record adds all non-nil justice tx variants to the history, keyed by their
// txid, and returns the ones that were not known before.
func (j *justiceTxTracker) record(
	variants *justiceTxVariants) map[chainhash.Hash]*justiceTxCtx {

	added := make(map[chainhash.Hash]*justiceTxCtx)
	record := func(jtx *justiceTxCtx) {
		if jtx == nil || jtx.justiceTx == nil {
			return
		}

		hash := jtx.justiceTx.TxHash()
		if _, ok := j.history[hash]; ok {
			return
		}

		// The history is only used to recognize a confirmed spend as
		// one of our justice txs and to tell the aux sweeper about
		// it. Store an immutable snapshot of the metadata that takes,
		// so later rebuilds can't change what is associated with an
		// older txid. The inputs are collected afresh from the spends
		// that report the transaction, and the transaction itself is
		// taken from there too.
		snapshot := *jtx
		snapshot.inputs = nil
		snapshot.justiceTx = nil

		j.history[hash] = &snapshot
		added[hash] = &snapshot
	}

	record(variants.spendAll)
	record(variants.spendCommitOuts)
	record(variants.spendHTLCs)
	for _, tx := range variants.spendSecondLevelHTLCs {
		record(tx)
	}

	return added
}

// collect inspects a batch of spends for confirmed justice txs and collects
// the breached outputs they spend. It must be called before the spends are
// applied to the breached outputs, as that removes the outputs a justice tx
// spent.
//
// A justice tx usually spends several breached outputs, and the spend of
// each is reported on its own, possibly in different batches. After a
// restart the spends are replayed from the breach on, so a justice tx can be
// reported through one of its inputs before another input of it, an HTLC
// that was taken to the second level, has been morphed back into the output
// the justice tx spent. The inputs are therefore accumulated across batches.
func (j *justiceTxTracker) collect(spends []spend,
	breachedOutputs []breachedOutput) {

	for _, s := range spends {
		txid := *s.detail.SpenderTxHash
		if _, ok := j.notified[txid]; ok {
			continue
		}

		jtx, ok := j.history[txid]
		if !ok {
			brarLog.Debugf("Spend txid=%v is not one of our "+
				"justice txs", txid)

			continue
		}

		confirmed, ok := j.pending[txid]
		if !ok {
			confirmed = &confirmedJusticeTx{
				tx:     s.detail.SpendingTx,
				height: uint32(s.detail.SpendingHeight),
				ctx:    jtx,
				inputs: make(
					map[wire.OutPoint]*breachedOutput,
				),
			}
			j.pending[txid] = confirmed
		}

		// Collect every breached output the transaction spends that
		// we currently know at the outpoint it was spent from. The
		// output is copied: it is about to be removed from the set,
		// or mutated by later morphing.
		for _, txIn := range confirmed.tx.TxIn {
			op := txIn.PreviousOutPoint
			if _, ok := confirmed.inputs[op]; ok {
				continue
			}

			for i := range breachedOutputs {
				if breachedOutputs[i].outpoint != op {
					continue
				}

				bo := breachedOutputs[i]
				confirmed.inputs[op] = &bo

				break
			}
		}
	}
}

// hasPending returns true if the aux sweeper still has to be told about a
// confirmed justice tx.
func (j *justiceTxTracker) hasPending() bool {
	return len(j.pending) > 0
}

// notifyConfirmedJusticeTxs tells the aux sweeper about every confirmed
// justice tx whose inputs are all collected, so it can generate the aux-level
// proofs. A transaction the aux sweeper fails to take stays pending and is
// retried on the next call.
func (b *BreachArbitrator) notifyConfirmedJusticeTxs(j *justiceTxTracker) {
	for txid, confirmed := range j.pending {
		if !confirmed.complete() {
			brarLog.Debugf("Justice tx %v confirmed, collected "+
				"%d of its %d inputs so far", txid,
				len(confirmed.inputs), len(confirmed.tx.TxIn))

			continue
		}

		// The inputs are handed over in the order the transaction
		// spends them.
		hasSecondLevel := confirmed.ctx.hasSecondLevel
		inputs := make([]input.Input, 0, len(confirmed.tx.TxIn))
		for _, txIn := range confirmed.tx.TxIn {
			bo := confirmed.inputs[txIn.PreviousOutPoint]
			inputs = append(inputs, bo)

			switch bo.WitnessType() {
			case input.HtlcSecondLevelRevoke,
				input.TaprootHtlcSecondLevelRevoke:

				hasSecondLevel = true
			}
		}

		brarLog.Debugf("Notifying aux sweeper of confirmed justice "+
			"tx %v (numInputs=%d, hasSecondLevel=%v)", txid,
			len(inputs), hasSecondLevel)

		bumpReq := sweep.BumpRequest{
			Inputs:          inputs,
			DeliveryAddress: confirmed.ctx.sweepAddr,
			ExtraTxOut:      confirmed.ctx.extraTxOut,
		}

		err := fn.MapOptionZ(
			b.cfg.AuxSweeper,
			func(aux sweep.AuxSweeper) error {
				// The tx is already confirmed, so skip
				// broadcast.
				return aux.NotifyBroadcast(
					&bumpReq, confirmed.tx,
					confirmed.ctx.fee, nil,
					sweep.AuxNotifyOpts{
						SkipBroadcast: true,
						ConfirmHeight: confirmed.height,
						//nolint:ll
						LookupInputProofs: hasSecondLevel,
					},
				)
			},
		)
		if err != nil {
			brarLog.Errorf("Failed to notify aux sweeper of "+
				"confirmed justice tx %v, will retry: %v",
				txid, err)

			continue
		}

		j.notified[txid] = struct{}{}
		delete(j.pending, txid)
	}
}

// exactRetribution is a goroutine which is executed once a contract breach has
// been detected by a breachObserver. This function is responsible for
// punishing a counterparty for violating the channel contract by sweeping ALL
// the lingering funds within the channel into the daemon's wallet.
//
// NOTE: This MUST be run as a goroutine.
//
//nolint:funlen
func (b *BreachArbitrator) exactRetribution(
	confChan *chainntnfs.ConfirmationEvent, breachInfo *retributionInfo) {

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

	// We may have to wait for some of the HTLC outputs to be spent to the
	// second level before broadcasting the justice tx. We'll store the
	// SpendEvents between each attempt to not re-register unnecessarily.
	spendNtfns := make(map[wire.OutPoint]*chainntnfs.SpendEvent)

	// Track all justice txs created for this breach by their txid, along
	// with the confirmed ones the aux sweeper still has to be told about.
	// This is needed because justiceTxs is rebuilt after each spend
	// detection, and the tx that actually confirmed may have been from
	// an earlier rebuild cycle, or from before a restart. Without this
	// history, we can't match the confirmed tx to call NotifyBroadcast on
	// the aux sweeper.
	//
	// Only aux (custom) channels have anything to report to the aux
	// sweeper. For every other channel the tracker stays empty and
	// nothing is persisted.
	tracker := newJusticeTxTracker(breachInfo.justiceTxs)
	trackJusticeTxs := b.cfg.AuxSweeper.IsSome() && breachInfo.hasAuxData()

	// Compute both the total value of funds being swept and the
	// amount of funds that were revoked from the counter party.
	var totalFunds, revokedFunds btcutil.Amount

	// allSwept is set once no breached output is left to sweep. The
	// breach is only cleaned up after the aux sweeper has also been told
	// about every confirmed justice tx: the retribution store is what
	// lets us pick that up again after a restart.
	var allSwept bool

justiceTxBroadcast:
	// With the breach transaction confirmed, we now create the
	// justice tx which will claim ALL the funds within the
	// channel.
	justiceTxs, err := b.createJusticeTx(breachInfo.breachedOutputs)
	if err != nil {
		brarLog.Errorf("Unable to create justice tx: %v", err)
		return
	}

	// Record the new variants before any of them is published, so the
	// one that confirms is recognized even if we restart in between.
	if trackJusticeTxs {
		err = b.cfg.Store.RecordJusticeTxs(
			&breachInfo.chanPoint, tracker.record(justiceTxs),
		)
		if err != nil {
			brarLog.Errorf("Unable to persist justice txs of "+
				"ChannelPoint(%v): %v", breachInfo.chanPoint,
				err)
		}
	}
	finalTx := justiceTxs.spendAll

	// We'll now attempt to broadcast the transaction which finalized the
	// channel's retribution against the cheating counter party.
	label := labels.MakeLabel(labels.LabelTypeJusticeTransaction, nil)
	err = b.cfg.PublishTransaction(finalTx.justiceTx, label)
	if err != nil {
		brarLog.Errorf("Unable to broadcast initial spendAll "+
			"justice tx: %v", err)
	}

	// Regardless of publication succeeded or not, we now wait for any of
	// the inputs to be spent. If any input got spent by the remote, we
	// must recreate our justice transaction.
	var (
		spendChan = make(chan []spend, 1)
		errChan   = make(chan error, 1)
		wg        sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()

		spends, err := b.waitForSpendEvent(breachInfo, spendNtfns)
		if err != nil {
			errChan <- err
			return
		}
		spendChan <- spends
	}()

	// We'll also register for block notifications, such that in case our
	// justice tx doesn't confirm within a reasonable timeframe, we can
	// start to more aggressively sweep the time sensitive outputs.
	newBlockChan, err := b.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		brarLog.Errorf("Unable to register for block notifications: %v",
			err)
		return
	}
	defer newBlockChan.Cancel()

Loop:
	for {
		select {
		case spends := <-spendChan:
			// Check if any of the spends represent a confirmed
			// justice transaction, and if so, collect the outputs
			// it spends while they are still part of the breach
			// info.
			if trackJusticeTxs {
				tracker.collect(
					spends, breachInfo.breachedOutputs,
				)
			}

			// Update the breach info with the new spends.
			t, r := updateBreachInfo(
				breachInfo, spends, b.cfg.AuxResolver,
			)

			// Notify the aux sweeper of the confirmed justice
			// transactions whose inputs are all accounted for.
			b.notifyConfirmedJusticeTxs(tracker)

			totalFunds += t
			revokedFunds += r

			brarLog.Infof("%v spends from breach tx for "+
				"ChannelPoint(%v) has been detected, %v "+
				"revoked funds (%v total) have been claimed",
				len(spends), breachInfo.chanPoint,
				revokedFunds, totalFunds)

			if len(breachInfo.breachedOutputs) == 0 {
				brarLog.Infof("Justice for ChannelPoint(%v) "+
					"has been served, %v revoked funds "+
					"(%v total) have been claimed. No "+
					"more outputs to sweep, marking fully "+
					"resolved", breachInfo.chanPoint,
					revokedFunds, totalFunds)

				// If the aux sweeper still has to be told
				// about a confirmed justice tx, keep the
				// breach around and retry on the next block.
				allSwept = true
				if tracker.hasPending() {
					brarLog.Warnf("ChannelPoint(%v) fully "+
						"swept, waiting to notify the "+
						"aux sweeper of %d confirmed "+
						"justice tx(s)",
						breachInfo.chanPoint,
						len(tracker.pending))

					continue Loop
				}

				err = b.cleanupBreach(&breachInfo.chanPoint)
				if err != nil {
					brarLog.Errorf("Failed to cleanup "+
						"breached ChannelPoint(%v): %v",
						breachInfo.chanPoint, err)
				}

				// TODO(roasbeef): add peer to blacklist?

				// TODO(roasbeef): close other active channels
				// with offending peer
				break Loop
			}

			brarLog.Infof("Attempting another justice tx "+
				"with %d inputs",
				len(breachInfo.breachedOutputs))

			wg.Wait()
			goto justiceTxBroadcast

		// On every new block, we check whether we should republish the
		// transactions.
		case epoch, ok := <-newBlockChan.Epochs:
			if !ok {
				return
			}

			// Retry telling the aux sweeper about confirmed
			// justice txs it failed to take earlier.
			b.notifyConfirmedJusticeTxs(tracker)

			// Once everything is swept, the only thing left to
			// wait for is the aux sweeper.
			if allSwept {
				if tracker.hasPending() {
					continue Loop
				}

				err = b.cleanupBreach(&breachInfo.chanPoint)
				if err != nil {
					brarLog.Errorf("Failed to cleanup "+
						"breached ChannelPoint(%v): %v",
						breachInfo.chanPoint, err)
				}

				break Loop
			}

			// If less than four blocks have passed since the
			// breach confirmed, we'll continue waiting. It was
			// published with a 2-block fee estimate, so it's not
			// unexpected that four blocks without confirmation can
			// pass.
			splitHeight := breachInfo.breachHeight +
				blocksPassedSplitPublish
			if uint32(epoch.Height) < splitHeight {
				continue Loop
			}

			brarLog.Warnf("Block height %v arrived without "+
				"justice tx confirming (breached at "+
				"height %v), splitting justice tx.",
				epoch.Height, breachInfo.breachHeight)

			// Otherwise we'll attempt to publish the two separate
			// justice transactions that sweeps the commitment
			// outputs and the HTLC outputs separately. This is to
			// mitigate the case where our "spend all" justice TX
			// doesn't propagate because the HTLC outputs have been
			// pinned by low fee HTLC txs.
			label := labels.MakeLabel(
				labels.LabelTypeJusticeTransaction, nil,
			)
			if justiceTxs.spendCommitOuts != nil {
				tx := justiceTxs.spendCommitOuts

				brarLog.Debugf("Broadcasting justice tx "+
					"spending commitment outs: %v",
					lnutils.SpewLogClosure(tx))

				err = b.cfg.PublishTransaction(
					tx.justiceTx, label,
				)
				if err != nil {
					brarLog.Warnf("Unable to broadcast "+
						"commit out spending justice "+
						"tx: %v", err)
				}
			}

			if justiceTxs.spendHTLCs != nil {
				tx := justiceTxs.spendHTLCs

				brarLog.Debugf("Broadcasting justice tx "+
					"spending HTLC outs: %v",
					lnutils.SpewLogClosure(tx))

				err = b.cfg.PublishTransaction(
					tx.justiceTx, label,
				)
				if err != nil {
					brarLog.Warnf("Unable to broadcast "+
						"HTLC out spending justice "+
						"tx: %v", err)
				}
			}

			for _, tx := range justiceTxs.spendSecondLevelHTLCs {

				brarLog.Debugf("Broadcasting justice tx "+
					"spending second-level HTLC output: %v",
					lnutils.SpewLogClosure(tx))

				err = b.cfg.PublishTransaction(
					tx.justiceTx, label,
				)
				if err != nil {
					brarLog.Warnf("Unable to broadcast "+
						"second-level HTLC out "+
						"spending justice tx: %v", err)
				}
			}

		case err := <-errChan:
			if err != errBrarShuttingDown {
				brarLog.Errorf("error waiting for "+
					"spend event: %v", err)
			}
			break Loop

		case <-b.quit:
			break Loop
		}
	}

	// Wait for our go routine to exit.
	wg.Wait()
}

// cleanupBreach marks the given channel point as fully resolved and removes the
// retribution for that the channel from the retribution store.
func (b *BreachArbitrator) cleanupBreach(chanPoint *wire.OutPoint) error {
	// With the channel closed, mark it in the database as such.
	err := b.cfg.DB.MarkChanFullyClosed(chanPoint)
	if err != nil {
		return fmt.Errorf("unable to mark chan as closed: %w", err)
	}

	// Justice has been carried out; we can safely delete the retribution
	// info from the database.
	err = b.cfg.Store.Remove(chanPoint)
	if err != nil {
		return fmt.Errorf("unable to remove retribution from db: %w",
			err)
	}

	// This is after the Remove call so that the chan passed in via
	// SubscribeBreachComplete is always notified, no matter when it is
	// called. Otherwise, if notifyBreachComplete was before Remove, a
	// very rare edge case could occur in which SubscribeBreachComplete
	// is called after notifyBreachComplete and before Remove, meaning the
	// caller would never be notified.
	b.notifyBreachComplete(chanPoint)

	return nil
}

// handleBreachHandoff handles a new breach event, by writing it to disk, then
// notifies the BreachArbitrator contract observer goroutine that a channel's
// contract has been breached by the prior counterparty. Once notified the
// BreachArbitrator will attempt to sweep ALL funds within the channel using the
// information provided within the BreachRetribution generated due to the
// breach of channel contract. The funds will be swept only after the breaching
// transaction receives a necessary number of confirmations.
//
// NOTE: This MUST be run as a goroutine.
func (b *BreachArbitrator) handleBreachHandoff(
	breachEvent *ContractBreachEvent) {

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
	b.cfg.CloseLink(&chanPoint, CloseBreach)

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

		// Notify about the failed lookup and return.
		breachEvent.ProcessACK(err)
		return
	}

	// If this channel is already marked as breached in the retribution
	// store, we already have handled the handoff for this breach. In this
	// case we can safely ACK the handoff, and return.
	if breached {
		b.Unlock()
		breachEvent.ProcessACK(nil)
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
	breachEvent.ProcessACK(err)

	// Bail if we failed to persist retribution info.
	if err != nil {
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
	witnessType input.StandardWitnessType
	signDesc    input.SignDescriptor
	confHeight  uint32

	secondLevelWitnessScript []byte
	secondLevelTapTweak      [32]byte

	// resolveReq is a template ResolutionReq populated at breach
	// detection time. It is used when an HTLC is taken to the second
	// level and we need to re-resolve the contract with the updated
	// witness type and the actual second-level spending tx.
	resolveReq *lnwallet.ResolutionReq

	witnessFunc input.WitnessGenerator

	resolutionBlob fn.Option[tlv.Blob]

	// TODO(roasbeef): function opt and hook into brar
}

// makeBreachedOutput assembles a new breachedOutput that can be used by the
// breach arbiter to construct a justice or sweep transaction.
func makeBreachedOutput(outpoint *wire.OutPoint,
	witnessType input.StandardWitnessType, secondLevelScript []byte,
	signDescriptor *input.SignDescriptor, confHeight uint32,
	resolutionBlob fn.Option[tlv.Blob]) breachedOutput {

	amount := signDescriptor.Output.Value

	return breachedOutput{
		amt:                      btcutil.Amount(amount),
		outpoint:                 *outpoint,
		secondLevelWitnessScript: secondLevelScript,
		witnessType:              witnessType,
		signDesc:                 *signDescriptor,
		confHeight:               confHeight,
		resolutionBlob:           resolutionBlob,
	}
}

// Amount returns the number of satoshis contained in the breached output.
func (bo *breachedOutput) Amount() btcutil.Amount {
	return bo.amt
}

// OutPoint returns the breached output's identifier that is to be included as a
// transaction input.
func (bo *breachedOutput) OutPoint() wire.OutPoint {
	return bo.outpoint
}

// RequiredTxOut returns a non-nil TxOut if input commits to a certain
// transaction output. This is used in the SINGLE|ANYONECANPAY case to make
// sure any presigned input is still valid by including the output.
func (bo *breachedOutput) RequiredTxOut() *wire.TxOut {
	return nil
}

// RequiredLockTime returns whether this input commits to a tx locktime that
// must be used in the transaction including it.
func (bo *breachedOutput) RequiredLockTime() (uint32, bool) {
	return 0, false
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

// Preimage returns the preimage that was used to create the breached output.
func (bo *breachedOutput) Preimage() fn.Option[lntypes.Preimage] {
	return fn.None[lntypes.Preimage]()
}

// CraftInputScript computes a valid witness that allows us to spend from the
// breached output. It does so by first generating and memoizing the witness
// generation function, which parameterized primarily by the witness type and
// sign descriptor. The method then returns the witness computed by invoking
// this function on the first and subsequent calls.
func (bo *breachedOutput) CraftInputScript(signer input.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes,
	prevOutputFetcher txscript.PrevOutputFetcher,
	txinIdx int) (*input.Script, error) {

	// First, we ensure that the witness generation function has been
	// initialized for this breached output.
	signDesc := bo.SignDesc()
	signDesc.PrevOutputFetcher = prevOutputFetcher
	bo.witnessFunc = bo.witnessType.WitnessGenerator(signer, signDesc)

	// Now that we have ensured that the witness generation function has
	// been initialized, we can proceed to execute it and generate the
	// witness for this particular breached output.
	return bo.witnessFunc(txn, hashCache, txinIdx)
}

// BlocksToMaturity returns the relative timelock, as a number of blocks, that
// must be built on top of the confirmation height before the output can be
// spent.
func (bo *breachedOutput) BlocksToMaturity() uint32 {
	// If the output is a to_remote output we can claim, and it's of the
	// confirmed type (or is a taproot channel that always has the CSV 1),
	// we must wait one block before claiming it.
	switch bo.witnessType {
	case input.CommitmentToRemoteConfirmed,
		input.TaprootRemoteCommitSpend,
		input.TaprootRemoteCommitSpendFinal:

		return 1
	}

	// All other breached outputs have no CSV delay.
	return 0
}

// HeightHint returns the minimum height at which a confirmed spending tx can
// occur.
func (bo *breachedOutput) HeightHint() uint32 {
	return bo.confHeight
}

// UnconfParent returns information about a possibly unconfirmed parent tx.
func (bo *breachedOutput) UnconfParent() *input.TxInfo {
	return nil
}

// ResolutionBlob returns a special opaque blob to be used to sweep/resolve this
// input.
func (bo *breachedOutput) ResolutionBlob() fn.Option[tlv.Blob] {
	return bo.resolutionBlob
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

	// justiceTxs holds the metadata of the justice transactions created
	// for this breach before a restart, keyed by txid. It is only set
	// when the retribution of an aux channel is loaded from the store.
	justiceTxs map[chainhash.Hash]*justiceTxCtx
}

// hasAuxData returns true if any breached output carries aux resolution
// data, which is only ever the case for aux (custom) channels.
func (ret *retributionInfo) hasAuxData() bool {
	for i := range ret.breachedOutputs {
		bo := &ret.breachedOutputs[i]
		if bo.resolveReq != nil || bo.resolutionBlob.IsSome() {
			return true
		}
	}

	return false
}

// claimsOutpoint returns true if one of the breached outputs currently sits
// at the given outpoint.
func (ret *retributionInfo) claimsOutpoint(op wire.OutPoint) bool {
	for i := range ret.breachedOutputs {
		if ret.breachedOutputs[i].outpoint == op {
			return true
		}
	}

	return false
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
	// where the local, remote, and all HTLCs are not dust outputs. HTLC
	// outputs provided by the wallet are expected to be non-dust, though
	// the commitment outputs are conditionally added depending on the
	// nil-ness of their sign descriptors.
	breachedOutputs := make([]breachedOutput, 0, nHtlcs+2)

	isTaproot := func() bool {
		if breachInfo.LocalOutputSignDesc != nil {
			return txscript.IsPayToTaproot(
				breachInfo.LocalOutputSignDesc.Output.PkScript,
			)
		}

		return txscript.IsPayToTaproot(
			breachInfo.RemoteOutputSignDesc.Output.PkScript,
		)
	}()

	// First, record the breach information for the local channel point if
	// it is not considered dust, which is signaled by a non-nil sign
	// descriptor. Here we use CommitmentNoDelay (or
	// CommitmentNoDelayTweakless for newer commitments) since this output
	// belongs to us and has no time-based constraints on spending. For
	// taproot channels, this is a normal spend from our output on the
	// commitment of the remote party.
	if breachInfo.LocalOutputSignDesc != nil {
		var witnessType input.StandardWitnessType
		switch {
		// Check the final channel type before the generic taproot case,
		// since the pkScript check below is true for both variants.
		case breachInfo.ChanType.IsTaprootFinal():
			witnessType = input.TaprootRemoteCommitSpendFinal

		case isTaproot:
			witnessType = input.TaprootRemoteCommitSpend

		case !isTaproot &&
			breachInfo.LocalOutputSignDesc.SingleTweak == nil:

			witnessType = input.CommitSpendNoDelayTweakless

		case !isTaproot:
			witnessType = input.CommitmentNoDelay
		}

		// If the local delay is non-zero, it means this output is of
		// the confirmed to_remote type.
		if !isTaproot && breachInfo.LocalDelay != 0 {
			witnessType = input.CommitmentToRemoteConfirmed
		}

		localOutput := makeBreachedOutput(
			&breachInfo.LocalOutpoint,
			witnessType,
			// No second level script as this is a commitment
			// output.
			nil,
			breachInfo.LocalOutputSignDesc,
			breachInfo.BreachHeight,
			breachInfo.LocalResolutionBlob,
		)

		breachedOutputs = append(breachedOutputs, localOutput)
	}

	// Second, record the same information regarding the remote outpoint,
	// again if it is not dust, which belongs to the party who tried to
	// steal our money! Here we set witnessType of the breachedOutput to
	// CommitmentRevoke, since we will be using a revoke key, withdrawing
	// the funds from the commitment transaction immediately.
	if breachInfo.RemoteOutputSignDesc != nil {
		var witType input.StandardWitnessType
		switch {
		case breachInfo.ChanType.IsTaprootFinal():
			witType = input.TaprootCommitmentRevokeFinal

		case isTaproot:
			witType = input.TaprootCommitmentRevoke

		default:
			witType = input.CommitmentRevoke
		}

		remoteOutput := makeBreachedOutput(
			&breachInfo.RemoteOutpoint,
			witType,
			// No second level script as this is a commitment
			// output.
			nil,
			breachInfo.RemoteOutputSignDesc,
			breachInfo.BreachHeight,
			breachInfo.RemoteResolutionBlob,
		)

		breachedOutputs = append(breachedOutputs, remoteOutput)
	}

	// Lastly, for each of the breached HTLC outputs, record each as a
	// breached output with the appropriate witness type based on its
	// directionality.
	for i, breachedHtlc := range breachInfo.HtlcRetributions {
		// Defensively skip blank entries. A nil sign descriptor
		// output is an invariant violation, so we log it loudly
		// instead of silently continuing.
		if breachedHtlc.SignDesc.Output == nil {
			brarLog.Warnf("Skipping blank HTLC retribution for "+
				"ChannelPoint(%v), index=%d", chanPoint, i)

			continue
		}

		// Using the breachedHtlc's incoming flag, determine the
		// appropriate witness type that needs to be generated in order
		// to sweep the HTLC output.
		var htlcWitnessType input.StandardWitnessType
		switch {
		case isTaproot && breachedHtlc.IsIncoming:
			htlcWitnessType = input.TaprootHtlcAcceptedRevoke

		case isTaproot && !breachedHtlc.IsIncoming:
			htlcWitnessType = input.TaprootHtlcOfferedRevoke

		case !isTaproot && breachedHtlc.IsIncoming:
			htlcWitnessType = input.HtlcAcceptedRevoke

		case !isTaproot && !breachedHtlc.IsIncoming:
			htlcWitnessType = input.HtlcOfferedRevoke
		}

		htlcOutput := makeBreachedOutput(
			&breachInfo.HtlcRetributions[i].OutPoint,
			htlcWitnessType,
			breachInfo.HtlcRetributions[i].SecondLevelWitnessScript,
			&breachInfo.HtlcRetributions[i].SignDesc,
			breachInfo.BreachHeight,
			breachInfo.HtlcRetributions[i].ResolutionBlob,
		)

		// For taproot outputs, we also need to hold onto the second
		// level tap tweak and resolution request template.
		//nolint:ll
		htlcOutput.secondLevelTapTweak = breachedHtlc.SecondLevelTapTweak
		htlcOutput.resolveReq = breachedHtlc.ResolveReq

		breachedOutputs = append(breachedOutputs, htlcOutput)
	}

	return &retributionInfo{
		commitHash:      breachInfo.BreachTxHash,
		chainHash:       breachInfo.ChainHash,
		chanPoint:       *chanPoint,
		breachedOutputs: breachedOutputs,
		breachHeight:    breachInfo.BreachHeight,
	}
}

// justiceTxVariants is a struct that holds transactions which exacts "justice"
// by sweeping ALL the funds within the channel which we are now entitled to
// due to a breach of the channel's contract by the counterparty. There are
// four variants of justice transactions:
//
// 1. The "normal" justice tx that spends all breached outputs.
// 2. A tx that spends only the breached to_local output and to_remote output
// (can be nil if none of these exist).
// 3. A tx that spends all the breached commitment level HTLC outputs (can be
// nil if none of these exist or if all have been taken to the second level).
// 4. A set of txs that spend all the second-level HTLC outputs (can be empty if
// no HTLC second-level txs have been confirmed).
//
// The reason we create these three variants, is that in certain cases (like
// with the anchor output HTLC malleability), the channel counter party can pin
// the HTLC outputs with low fee children, hindering our normal justice tx that
// attempts to spend these outputs from propagating. In this case we want to
// spend the to_local output and commitment level HTLC outputs separately,
// before the CSV locks expire.
type justiceTxVariants struct {
	spendAll              *justiceTxCtx
	spendCommitOuts       *justiceTxCtx
	spendHTLCs            *justiceTxCtx
	spendSecondLevelHTLCs []*justiceTxCtx
}

// createJusticeTx creates transactions which exacts "justice" by sweeping ALL
// the funds within the channel which we are now entitled to due to a breach of
// the channel's contract by the counterparty. This function returns a *fully*
// signed transaction with the witness for each input fully in place.
func (b *BreachArbitrator) createJusticeTx(
	breachedOutputs []breachedOutput) (*justiceTxVariants, error) {

	var (
		allInputs         []input.Input
		commitInputs      []input.Input
		htlcInputs        []input.Input
		secondLevelInputs []input.Input
	)

	for i := range breachedOutputs {
		// Grab locally scoped reference to breached output.
		inp := &breachedOutputs[i]
		allInputs = append(allInputs, inp)

		// Check if the input is from a commitment output, a commitment
		// level HTLC output or a second level HTLC output.
		switch inp.WitnessType() {
		case input.HtlcAcceptedRevoke, input.HtlcOfferedRevoke,
			input.TaprootHtlcAcceptedRevoke,
			input.TaprootHtlcOfferedRevoke:

			htlcInputs = append(htlcInputs, inp)

		case input.HtlcSecondLevelRevoke,
			input.TaprootHtlcSecondLevelRevoke:

			secondLevelInputs = append(secondLevelInputs, inp)

		default:
			commitInputs = append(commitInputs, inp)
		}
	}

	brarLog.Infof("createJusticeTx: %d total inputs (%d commit, "+
		"%d htlc, %d second-level)", len(allInputs),
		len(commitInputs), len(htlcInputs), len(secondLevelInputs))

	var (
		txs = &justiceTxVariants{}
		err error
	)

	// For each group of inputs, create a tx that spends them.
	txs.spendAll, err = b.createSweepTx(allInputs...)
	if err != nil {
		return nil, err
	}
	brarLog.Infof("createJusticeTx: spendAll created successfully "+
		"(%d inputs, txid=%v)", len(allInputs),
		txs.spendAll.justiceTx.TxHash())

	txs.spendCommitOuts, err = b.createSweepTx(commitInputs...)
	if err != nil {
		brarLog.Errorf("could not create sweep tx for commitment "+
			"outputs: %v", err)
	} else if txs.spendCommitOuts != nil {
		brarLog.Infof("createJusticeTx: spendCommitOuts created "+
			"successfully (%d inputs)", len(commitInputs))
	}

	txs.spendHTLCs, err = b.createSweepTx(htlcInputs...)
	if err != nil {
		brarLog.Errorf("could not create sweep tx for HTLC outputs: %v",
			err)
	} else if txs.spendHTLCs != nil {
		brarLog.Infof("createJusticeTx: spendHTLCs created "+
			"successfully (%d inputs)", len(htlcInputs))
	}

	// TODO(roasbeef): only register one of them?

	secondLevelSweeps := make([]*justiceTxCtx, 0, len(secondLevelInputs))
	for i, input := range secondLevelInputs {
		sweepTx, err := b.createSweepTx(input)
		if err != nil {
			brarLog.Errorf("could not create sweep tx for "+
				"second-level HTLC output %d: %v", i, err)

			continue
		}

		brarLog.Infof("createJusticeTx: individual second-level "+
			"sweep %d created successfully", i)
		secondLevelSweeps = append(secondLevelSweeps, sweepTx)
	}
	txs.spendSecondLevelHTLCs = secondLevelSweeps

	brarLog.Infof("createJusticeTx: variants summary: spendAll=%v, "+
		"spendCommitOuts=%v, spendHTLCs=%v, "+
		"spendSecondLevelHTLCs=%d",
		txs.spendAll != nil, txs.spendCommitOuts != nil,
		txs.spendHTLCs != nil, len(txs.spendSecondLevelHTLCs))

	return txs, nil
}

// justiceTxCtx contains the justice transaction along with other related meta
// data.
type justiceTxCtx struct {
	justiceTx *wire.MsgTx

	sweepAddr lnwallet.AddrWithKey

	extraTxOut fn.Option[sweep.SweepOutput]

	fee btcutil.Amount

	// hasSecondLevel records whether this concrete justice tx spends any
	// second-level revoke inputs. We keep it on the tx context because the
	// live breachedOutputs slice may later be rebuilt into a view that no
	// longer faithfully reflects the confirmed tx we need to notify about.
	hasSecondLevel bool

	inputs []input.Input
}

// createSweepTx creates a tx that sweeps the passed inputs back to our wallet.
func (b *BreachArbitrator) createSweepTx(
	inputs ...input.Input) (*justiceTxCtx, error) {

	if len(inputs) == 0 {
		return nil, nil
	}

	// We will assemble the breached outputs into a slice of spendable
	// outputs, while simultaneously computing the estimated weight of the
	// transaction.
	var (
		spendableOutputs []input.Input
		weightEstimate   input.TxWeightEstimator
	)

	// Allocate enough space to potentially hold each of the breached
	// outputs in the retribution info.
	spendableOutputs = make([]input.Input, 0, len(inputs))

	// The justice transaction we construct will be a segwit transaction
	// that pays to a p2tr output. Components such as the version,
	// nLockTime, and output are already included in the TxWeightEstimator.
	weightEstimate.AddP2TROutput()

	// If any of our inputs has a resolution blob, then we'll add another
	// P2TR _output_, since we'll want to separate the custom channel
	// outputs from the regular, BTC only outputs. So we only need one such
	// output, which'll carry the custom channel "valuables" from both the
	// breached commitment and HTLC outputs.
	hasBlobs := fn.Any(inputs, func(i input.Input) bool {
		return i.ResolutionBlob().IsSome()
	})
	if hasBlobs {
		weightEstimate.AddP2TROutput()
	}

	// Next, we iterate over the breached outputs contained in the
	// retribution info.  For each, we switch over the witness type such
	// that we contribute the appropriate weight for each input and
	// witness, finally adding to our list of spendable outputs.
	for i := range inputs {
		// Grab locally scoped reference to breached output.
		inp := inputs[i]

		// First, determine the appropriate estimated witness weight
		// for the give witness type of this breached output. If the
		// witness weight cannot be estimated, we will omit it from the
		// transaction.
		witnessWeight, _, err := inp.WitnessType().SizeUpperBound()
		if err != nil {
			brarLog.Warnf("could not determine witness weight "+
				"for breached output in retribution info: %v",
				err)
			continue
		}
		weightEstimate.AddWitnessInput(witnessWeight)

		// Finally, append this input to our list of spendable outputs.
		spendableOutputs = append(spendableOutputs, inp)
	}

	txWeight := weightEstimate.Weight()

	return b.sweepSpendableOutputsTxn(txWeight, spendableOutputs...)
}

// sweepSpendableOutputsTxn creates a signed transaction from a sequence of
// spendable outputs by sweeping the funds into a single p2wkh output.
func (b *BreachArbitrator) sweepSpendableOutputsTxn(txWeight lntypes.WeightUnit,
	inputs ...input.Input) (*justiceTxCtx, error) {

	// First, we obtain a new public key script from the wallet which we'll
	// sweep the funds to.
	// TODO(roasbeef): possibly create many outputs to minimize change in
	// the future?
	pkScript, err := b.cfg.GenSweepScript().Unpack()
	if err != nil {
		return nil, err
	}

	// Compute the total amount contained in the inputs.
	var totalAmt btcutil.Amount
	for _, inp := range inputs {
		totalAmt += btcutil.Amount(inp.SignDesc().Output.Value)
	}

	// We'll actually attempt to target inclusion within the next two
	// blocks as we'd like to sweep these funds back into our wallet ASAP.
	feePerKw, err := b.cfg.Estimator.EstimateFeePerKW(justiceTxConfTarget)
	if err != nil {
		return nil, err
	}
	txFee := feePerKw.FeeForWeight(txWeight)

	// At this point, we'll check to see if we have any extra outputs to
	// add from the aux sweeper.
	extraChangeOut := fn.MapOptionZ(
		b.cfg.AuxSweeper,
		func(aux sweep.AuxSweeper) fn.Result[sweep.SweepOutput] {
			return aux.DeriveSweepAddr(inputs, pkScript)
		},
	)
	if err := extraChangeOut.Err(); err != nil {
		return nil, err
	}

	// TODO(roasbeef): already start to siphon their funds into fees
	sweepAmt := int64(totalAmt - txFee)

	// With the fee calculated, we can now create the transaction using the
	// information gathered above and the provided retribution information.
	txn := wire.NewMsgTx(2)

	// First, we'll add the extra sweep output if it exists, subtracting the
	// amount from the sweep amt.
	var (
		hasAuxOut bool
		auxOutAmt btcutil.Amount
	)
	if b.cfg.AuxSweeper.IsSome() {
		extraChangeOut.WhenOk(func(o sweep.SweepOutput) {
			sweepAmt -= o.Value
			auxOutAmt = btcutil.Amount(o.Value)

			txn.AddTxOut(&o.TxOut)
			hasAuxOut = true
		})
	}

	dustLimit := int64(lnwallet.DustLimitForSize(input.UnknownWitnessSize))
	switch {
	// Without an aux output this is the justice transaction of a regular
	// channel: we add the output to which our funds will be deposited,
	// exactly as we always did.
	case !hasAuxOut:
		txn.AddTxOut(&wire.TxOut{
			PkScript: pkScript.DeliveryAddress,
			Value:    sweepAmt,
		})

	// With an aux output, the BTC sweep output is only added if it is
	// above the dust limit.
	case sweepAmt > dustLimit:
		txn.AddTxOut(&wire.TxOut{
			PkScript: pkScript.DeliveryAddress,
			Value:    sweepAmt,
		})

	// Otherwise we skip the BTC sweep output entirely: for custom (asset)
	// channels the real value is carried by the aux output, and whatever
	// BTC the inputs hold beyond it goes towards fees. Using the dust
	// limit instead of zero prevents the mempool from rejecting the
	// transaction. The inputs still have to cover the aux output, or the
	// transaction would spend more than it has.
	case totalAmt < auxOutAmt:
		return nil, fmt.Errorf("inputs of %v cannot fund the aux "+
			"sweep output of %v", totalAmt, auxOutAmt)

	default:
		txFee = totalAmt - auxOutAmt

		brarLog.Infof("Dropping BTC sweep output (amount=%d), all "+
			"input BTC (%v) goes to the aux output (%v) and fees "+
			"(%v)", sweepAmt, totalAmt, auxOutAmt, txFee)
	}

	// TODO(roasbeef): add other output change modify sweep amt

	// Next, we add all of the spendable outputs as inputs to the
	// transaction.
	for _, inp := range inputs {
		txn.AddTxIn(&wire.TxIn{
			PreviousOutPoint: inp.OutPoint(),
			Sequence:         inp.BlocksToMaturity(),
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
	prevOutputFetcher, err := input.MultiPrevOutFetcher(inputs)
	if err != nil {
		return nil, err
	}
	hashCache := txscript.NewTxSigHashes(txn, prevOutputFetcher)

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
			b.cfg.Signer, txn, hashCache, prevOutputFetcher, idx,
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
	for i, inp := range inputs {
		if err := addWitness(i, inp); err != nil {
			return nil, err
		}
	}

	return &justiceTxCtx{
		justiceTx:  txn,
		sweepAddr:  pkScript,
		extraTxOut: extraChangeOut.OkToSome(),
		fee:        txFee,
		hasSecondLevel: fn.Any(inputs, func(inp input.Input) bool {
			wt := inp.WitnessType()
			return wt == input.HtlcSecondLevelRevoke ||
				wt == input.TaprootHtlcSecondLevelRevoke
		}),
		inputs: inputs,
	}, nil
}

// RetributionStore handles persistence of retribution states to disk and is
// backed by a boltdb bucket. The primary responsibility of the retribution
// store is to ensure that we can recover from a restart in the middle of a
// breached contract retribution.
type RetributionStore struct {
	db kvdb.Backend
}

// NewRetributionStore creates a new instance of a RetributionStore.
func NewRetributionStore(db kvdb.Backend) *RetributionStore {
	return &RetributionStore{
		db: db,
	}
}

// taprootBriefcaseFromRetInfo creates a taprootBriefcase from a retribution
// info struct. This stores all the tap tweak information we need to inrder to
// be able to hadnel breaches after a restart.
func taprootBriefcaseFromRetInfo(retInfo *retributionInfo) *taprootBriefcase {
	tapCase := newTaprootBriefcase()

	for _, bo := range retInfo.breachedOutputs {
		switch bo.WitnessType() {
		// For spending from our commitment output on the remote
		// commitment, we'll need to stash the control block.
		case input.TaprootRemoteCommitSpend,
			input.TaprootRemoteCommitSpendFinal:

			//nolint:ll
			tapCase.CtrlBlocks.Val.CommitSweepCtrlBlock = bo.signDesc.ControlBlock

			bo.resolutionBlob.WhenSome(func(blob tlv.Blob) {
				tapCase.SettledCommitBlob = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType2](
						blob,
					),
				)
			})

		// To spend the revoked output again, we'll store the same
		// control block value as above, but in a different place.
		case input.TaprootCommitmentRevoke,
			input.TaprootCommitmentRevokeFinal:

			//nolint:ll
			tapCase.CtrlBlocks.Val.RevokeSweepCtrlBlock = bo.signDesc.ControlBlock

			bo.resolutionBlob.WhenSome(func(blob tlv.Blob) {
				tapCase.BreachedCommitBlob = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType3](
						blob,
					),
				)
			})

		// For spending the HTLC outputs, we'll store the first and
		// second level tweak values.
		case input.TaprootHtlcAcceptedRevoke:
			fallthrough
		case input.TaprootHtlcOfferedRevoke:
			resID := newResolverID(bo.OutPoint())

			var firstLevelTweak [32]byte
			copy(firstLevelTweak[:], bo.signDesc.TapTweak)
			secondLevelTweak := bo.secondLevelTapTweak

			//nolint:ll
			tapCase.TapTweaks.Val.BreachedHtlcTweaks[resID] = firstLevelTweak

			//nolint:ll
			tapCase.TapTweaks.Val.BreachedSecondLevelHltcTweaks[resID] = secondLevelTweak

			// The aux resolution blob of the HTLC is only known
			// at breach detection time, so it has to be stored
			// to still be able to sweep the HTLC's aux value
			// after a restart.
			bo.resolutionBlob.WhenSome(func(blob tlv.Blob) {
				blobsOpt := tapCase.HtlcBlobs.ValOpt()
				htlcBlobs := blobsOpt.UnwrapOr(
					newAuxHtlcBlobs(),
				)
				htlcBlobs[resID] = blob
				tapCase.HtlcBlobs = tlv.SomeRecordT(
					tlv.NewRecordT[tlv.TlvType4](htlcBlobs),
				)
			})
		}
	}

	return tapCase
}

// applyTaprootRetInfo attaches the taproot specific information in the tapCase
// to the passed retInfo struct.
func applyTaprootRetInfo(tapCase *taprootBriefcase,
	retInfo *retributionInfo) error {

	for i := range retInfo.breachedOutputs {
		bo := retInfo.breachedOutputs[i]

		switch bo.WitnessType() {
		// For spending from our commitment output on the remote
		// commitment, we'll apply the control block.
		case input.TaprootRemoteCommitSpend,
			input.TaprootRemoteCommitSpendFinal:

			//nolint:ll
			bo.signDesc.ControlBlock = tapCase.CtrlBlocks.Val.CommitSweepCtrlBlock

			tapCase.SettledCommitBlob.WhenSomeV(
				func(blob tlv.Blob) {
					bo.resolutionBlob = fn.Some(blob)
				},
			)

		// To spend the revoked output again, we'll apply the same
		// control block value as above, but to a different place.
		case input.TaprootCommitmentRevoke,
			input.TaprootCommitmentRevokeFinal:

			//nolint:ll
			bo.signDesc.ControlBlock = tapCase.CtrlBlocks.Val.RevokeSweepCtrlBlock

			tapCase.BreachedCommitBlob.WhenSomeV(
				func(blob tlv.Blob) {
					bo.resolutionBlob = fn.Some(blob)
				},
			)

		// For spending the HTLC outputs, we'll apply the first and
		// second level tweak values.
		case input.TaprootHtlcAcceptedRevoke:
			fallthrough
		case input.TaprootHtlcOfferedRevoke:
			resID := newResolverID(bo.OutPoint())

			//nolint:ll
			tap1, ok := tapCase.TapTweaks.Val.BreachedHtlcTweaks[resID]
			if !ok {
				return fmt.Errorf("unable to find taproot "+
					"tweak for: %v", bo.OutPoint())
			}
			bo.signDesc.TapTweak = tap1[:]

			//nolint:ll
			tap2, ok := tapCase.TapTweaks.Val.BreachedSecondLevelHltcTweaks[resID]
			if !ok {
				return fmt.Errorf("unable to find taproot "+
					"tweak for: %v", bo.OutPoint())
			}
			bo.secondLevelTapTweak = tap2

			tapCase.HtlcBlobs.WhenSomeV(func(blobs htlcAuxBlobs) {
				if blob, ok := blobs[resID]; ok {
					bo.resolutionBlob = fn.Some(blob)
				}
			})
		}

		retInfo.breachedOutputs[i] = bo
	}

	return nil
}

// Add adds a retribution state to the RetributionStore, which is then persisted
// to disk.
func (rs *RetributionStore) Add(ret *retributionInfo) error {
	return kvdb.Update(rs.db, func(tx kvdb.RwTx) error {
		// If this is our first contract breach, the retributionBucket
		// won't exist, in which case, we just create a new bucket.
		retBucket, err := tx.CreateTopLevelBucket(retributionBucket)
		if err != nil {
			return err
		}
		tapRetBucket, err := tx.CreateTopLevelBucket(
			taprootRetributionBucket,
		)
		if err != nil {
			return err
		}

		var outBuf bytes.Buffer
		err = graphdb.WriteOutpoint(&outBuf, &ret.chanPoint)
		if err != nil {
			return err
		}

		var retBuf bytes.Buffer
		if err := ret.Encode(&retBuf); err != nil {
			return err
		}

		err = retBucket.Put(outBuf.Bytes(), retBuf.Bytes())
		if err != nil {
			return err
		}

		// For aux channels, store what we need to resolve the HTLCs
		// again after a restart. This is a no-op for every other
		// channel.
		err = putAuxBreachContext(tx, outBuf.Bytes(), ret)
		if err != nil {
			return err
		}

		// If this isn't a taproot channel, then we can exit early here
		// as there's no extra data to write.
		switch {
		case len(ret.breachedOutputs) == 0:
			return nil

		case !txscript.IsPayToTaproot(
			ret.breachedOutputs[0].signDesc.Output.PkScript,
		):
			return nil
		}

		// We'll also map the ret info into the taproot storage
		// structure we need for taproot channels.
		var b bytes.Buffer
		tapRetcase := taprootBriefcaseFromRetInfo(ret)
		if err := tapRetcase.Encode(&b); err != nil {
			return err
		}

		return tapRetBucket.Put(outBuf.Bytes(), b.Bytes())
	}, func() {})
}

// IsBreached queries the retribution store to discern if this channel was
// previously breached. This is used when connecting to a peer to determine if
// it is safe to add a link to the htlcswitch, as we should never add a channel
// that has already been breached.
func (rs *RetributionStore) IsBreached(chanPoint *wire.OutPoint) (bool, error) {
	var found bool
	err := kvdb.View(rs.db, func(tx kvdb.RTx) error {
		retBucket := tx.ReadBucket(retributionBucket)
		if retBucket == nil {
			return nil
		}

		var chanBuf bytes.Buffer
		err := graphdb.WriteOutpoint(&chanBuf, chanPoint)
		if err != nil {
			return err
		}

		retInfo := retBucket.Get(chanBuf.Bytes())
		if retInfo != nil {
			found = true
		}

		return nil
	}, func() {
		found = false
	})

	return found, err
}

// Remove removes a retribution state and finalized justice transaction by
// channel point  from the retribution store.
func (rs *RetributionStore) Remove(chanPoint *wire.OutPoint) error {
	return kvdb.Update(rs.db, func(tx kvdb.RwTx) error {
		retBucket := tx.ReadWriteBucket(retributionBucket)
		tapRetBucket, err := tx.CreateTopLevelBucket(
			taprootRetributionBucket,
		)
		if err != nil {
			return err
		}

		// We return an error if the bucket is not already created,
		// since normal operation of the breach arbiter should never
		// try to remove a finalized retribution state that is not
		// already stored in the db.
		if retBucket == nil {
			return errors.New("unable to remove retribution " +
				"because the retribution bucket doesn't exist")
		}

		// Serialize the channel point we are intending to remove.
		var chanBuf bytes.Buffer
		err = graphdb.WriteOutpoint(&chanBuf, chanPoint)
		if err != nil {
			return err
		}
		chanBytes := chanBuf.Bytes()

		// Remove the persisted retribution info and finalized justice
		// transaction.
		if err := retBucket.Delete(chanBytes); err != nil {
			return err
		}

		if err := deleteAuxBreachState(tx, chanBytes); err != nil {
			return err
		}

		return tapRetBucket.Delete(chanBytes)
	}, func() {})
}

// ForAll iterates through all stored retributions and executes the passed
// callback function on each retribution.
func (rs *RetributionStore) ForAll(cb func(*retributionInfo) error,
	reset func()) error {

	return kvdb.View(rs.db, func(tx kvdb.RTx) error {
		// If the bucket does not exist, then there are no pending
		// retributions.
		retBucket := tx.ReadBucket(retributionBucket)
		if retBucket == nil {
			return nil
		}
		tapRetBucket := tx.ReadBucket(
			taprootRetributionBucket,
		)

		// Otherwise, we fetch each serialized retribution info,
		// deserialize it, and execute the passed in callback function
		// on it.
		return retBucket.ForEach(func(k, retBytes []byte) error {
			ret := &retributionInfo{}
			err := ret.Decode(bytes.NewBuffer(retBytes))
			if err != nil {
				return err
			}

			tapInfoBytes := tapRetBucket.Get(k)
			if tapInfoBytes != nil {
				var tapCase taprootBriefcase
				err := tapCase.Decode(
					bytes.NewReader(tapInfoBytes),
				)
				if err != nil {
					return err
				}

				err = applyTaprootRetInfo(&tapCase, ret)
				if err != nil {
					return err
				}
			}

			// Restore the aux state, if any was stored. This
			// needs the fully restored sign descriptors, so it
			// comes last.
			if err := fetchAuxBreachState(tx, k, ret); err != nil {
				return err
			}

			return cb(ret)
		})
	}, reset)
}

// Encode serializes the retribution into the passed byte stream.
func (ret *retributionInfo) Encode(w io.Writer) error {
	var scratch [4]byte

	if _, err := w.Write(ret.commitHash[:]); err != nil {
		return err
	}

	if err := graphdb.WriteOutpoint(w, &ret.chanPoint); err != nil {
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

// Decode deserializes a retribution from the passed byte stream.
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

	if err := graphdb.ReadOutpoint(r, &ret.chanPoint); err != nil {
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

	if err := graphdb.WriteOutpoint(w, &bo.outpoint); err != nil {
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

	if err := graphdb.ReadOutpoint(r, &bo.outpoint); err != nil {
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
	bo.witnessType = input.StandardWitnessType(
		binary.BigEndian.Uint16(scratch[:2]),
	)

	return nil
}
