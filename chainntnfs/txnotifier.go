package chainntnfs

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	// ErrTxNotifierExiting is an error returned when attempting to interact
	// with the TxNotifier but it been shut down.
	ErrTxNotifierExiting = errors.New("TxNotifier is exiting")

	// ErrTxMaxConfs signals that the user requested a number of
	// confirmations beyond the reorg safety limit.
	ErrTxMaxConfs = errors.New("too many confirmations requested")
)

// rescanState indicates the progression of a registration before the notifier
// can begin dispatching confirmations at tip.
type rescanState byte

const (
	// rescanNotStarted is the initial state, denoting that a historical
	// dispatch may be required.
	rescanNotStarted rescanState = iota

	// rescanPending indicates that a dispatch has already been made, and we
	// are waiting for its completion. No other rescans should be dispatched
	// while in this state.
	rescanPending

	// rescanComplete signals either that a rescan was dispatched and has
	// completed, or that we began watching at tip immediately. In either
	// case, the notifier can only dispatch notifications from tip when in
	// this state.
	rescanComplete
)

// confNtfnSet holds all known, registered confirmation notifications for a
// single txid. If duplicates notifications are requested, only one historical
// dispatch will be spawned to ensure redundant scans are not permitted. A
// single conf detail will be constructed and dispatched to all interested
// clients.
type confNtfnSet struct {
	// ntfns keeps tracks of all the active client notification requests for
	// a transaction.
	ntfns map[uint64]*ConfNtfn

	// rescanStatus represents the current rescan state for the transaction.
	rescanStatus rescanState

	// details serves as a cache of the confirmation details of a
	// transaction that we'll use to determine if a transaction has already
	// confirmed at the time of registration.
	details *TxConfirmation
}

// newConfNtfnSet constructs a fresh confNtfnSet for a group of clients
// interested in a notification for a particular txid.
func newConfNtfnSet() *confNtfnSet {
	return &confNtfnSet{
		ntfns:        make(map[uint64]*ConfNtfn),
		rescanStatus: rescanNotStarted,
	}
}

// spendNtfnSet holds all known, registered spend notifications for an outpoint.
// If duplicate notifications are requested, only one historical dispatch will
// be spawned to ensure redundant scans are not permitted.
type spendNtfnSet struct {
	// ntfns keeps tracks of all the active client notification requests for
	// an outpoint.
	ntfns map[uint64]*SpendNtfn

	// rescanStatus represents the current rescan state for the outpoint.
	rescanStatus rescanState

	// details serves as a cache of the spend details for an outpoint that
	// we'll use to determine if an outpoint has already been spent at the
	// time of registration.
	details *SpendDetail
}

// newSpendNtfnSet constructs a new spend notification set.
func newSpendNtfnSet() *spendNtfnSet {
	return &spendNtfnSet{
		ntfns:        make(map[uint64]*SpendNtfn),
		rescanStatus: rescanNotStarted,
	}
}

// ConfNtfn represents a notifier client's request to receive a notification
// once the target transaction gets sufficient confirmations. The client is
// asynchronously notified via the ConfirmationEvent channels.
type ConfNtfn struct {
	// ConfID uniquely identifies the confirmation notification request for
	// the specified transaction.
	ConfID uint64

	// TxID is the hash of the transaction for which confirmation notifications
	// are requested.
	TxID *chainhash.Hash

	// PkScript is the public key script of an outpoint created in this
	// transaction.
	//
	// NOTE: This value MUST be set when the dispatch is to be performed
	// using compact filters.
	PkScript []byte

	// NumConfirmations is the number of confirmations after which the
	// notification is to be sent.
	NumConfirmations uint32

	// Event contains references to the channels that the notifications are to
	// be sent over.
	Event *ConfirmationEvent

	// HeightHint is the minimum height in the chain that we expect to find
	// this txid.
	HeightHint uint32

	// dispatched is false if the confirmed notification has not been sent yet.
	dispatched bool
}

// HistoricalConfDispatch parameterizes a manual rescan for a particular
// transaction identifier. The parameters include the start and end block
// heights specifying the range of blocks to scan.
type HistoricalConfDispatch struct {
	// TxID is the transaction ID to search for in the historical dispatch.
	TxID *chainhash.Hash

	// PkScript is a public key script from an output created by this
	// transaction.
	//
	// NOTE: This value MUST be set when the dispatch is to be performed
	// using compact filters.
	PkScript []byte

	// StartHeight specifies the block height at which to being the
	// historical rescan.
	StartHeight uint32

	// EndHeight specifies the last block height (inclusive) that the
	// historical scan should consider.
	EndHeight uint32
}

// SpendNtfn represents a client's request to receive a notification once an
// outpoint has been spent on-chain. The client is asynchronously notified via
// the SpendEvent channels.
type SpendNtfn struct {
	// SpendID uniquely identies the spend notification request for the
	// specified outpoint.
	SpendID uint64

	// OutPoint is the outpoint for which a client has requested a spend
	// notification for.
	OutPoint wire.OutPoint

	// PkScript is the script of the outpoint. This is needed in order to
	// match compact filters when attempting a historical rescan to
	// determine if the outpoint has already been spent.
	PkScript []byte

	// Event contains references to the channels that the notifications are
	// to be sent over.
	Event *SpendEvent

	// HeightHint is the earliest height in the chain that we expect to find
	// the spending transaction of the specified outpoint. This value will
	// be overridden by the spend hint cache if it contains an entry for it.
	HeightHint uint32

	// dispatched signals whether a spend notification has been disptached
	// to the client.
	dispatched bool
}

// HistoricalSpendDispatch parameterizes a manual rescan to determine the
// spending details (if any) of an outpoint. The parameters include the start
// and end block heights specifying the range of blocks to scan.
type HistoricalSpendDispatch struct {
	// OutPoint is the outpoint which we should attempt to find the spending
	OutPoint wire.OutPoint

	// PkScript is the script of the outpoint. This is needed in order to
	// match compact filters when attempting a historical rescan.
	PkScript []byte

	// StartHeight specified the block height at which to begin the
	// historical rescan.
	StartHeight uint32

	// EndHeight specifies the last block height (inclusive) that the
	// historical rescan should consider.
	EndHeight uint32
}

// TxNotifier is a struct responsible for delivering transaction notifications
// to subscribers. These notifications can be of two different types:
// transaction confirmations and/or outpoint spends. The TxNotifier will watch
// the blockchain as new blocks come in, in order to satisfy its client
// requests.
type TxNotifier struct {
	// currentHeight is the height of the tracked blockchain. It is used to
	// determine the number of confirmations a tx has and ensure blocks are
	// connected and disconnected in order.
	currentHeight uint32

	// reorgSafetyLimit is the chain depth beyond which it is assumed a
	// block will not be reorganized out of the chain. This is used to
	// determine when to prune old notification requests so that reorgs are
	// handled correctly. The coinbase maturity period is a reasonable value
	// to use.
	reorgSafetyLimit uint32

	// reorgDepth is the depth of a chain organization that this system is
	// being informed of. This is incremented as long as a sequence of
	// blocks are disconnected without being interrupted by a new block.
	reorgDepth uint32

	// confNotifications is an index of notification requests by transaction
	// hash.
	confNotifications map[chainhash.Hash]*confNtfnSet

	// txsByInitialHeight is an index of watched transactions by the height
	// that they are included at in the blockchain. This is tracked so that
	// incorrect notifications are not sent if a transaction is reorged out
	// of the chain and so that negative confirmations can be recognized.
	txsByInitialHeight map[uint32]map[chainhash.Hash]struct{}

	// ntfnsByConfirmHeight is an index of notification requests by the
	// height at which the transaction will have sufficient confirmations.
	ntfnsByConfirmHeight map[uint32]map[*ConfNtfn]struct{}

	// spendNotifications is an index of all active notification requests
	// per outpoint.
	spendNotifications map[wire.OutPoint]*spendNtfnSet

	// opsBySpendHeight is an index that keeps tracks of the spending height
	// of an outpoint we are currently tracking notifications for. This is
	// used in order to recover from the spending transaction of an outpoint
	// being reorged out of the chain.
	opsBySpendHeight map[uint32]map[wire.OutPoint]struct{}

	// confirmHintCache is a cache used to maintain the latest height hints
	// for transactions. Each height hint represents the earliest height at
	// which the transactions could have been confirmed within the chain.
	confirmHintCache ConfirmHintCache

	// spendHintCache is a cache used to maintain the latest height hints
	// for outpoints. Each height hint represents the earliest height at
	// which the outpoints could have been spent within the chain.
	spendHintCache SpendHintCache

	// quit is closed in order to signal that the notifier is gracefully
	// exiting.
	quit chan struct{}

	sync.Mutex
}

// NewTxNotifier creates a TxNotifier. The current height of the blockchain is
// accepted as a parameter. The different hint caches (confirm and spend) are
// used as an optimization in order to retrieve a better starting point when
// dispatching a recan for a historical event in the chain.
func NewTxNotifier(startHeight uint32, reorgSafetyLimit uint32,
	confirmHintCache ConfirmHintCache,
	spendHintCache SpendHintCache) *TxNotifier {

	return &TxNotifier{
		currentHeight:        startHeight,
		reorgSafetyLimit:     reorgSafetyLimit,
		confNotifications:    make(map[chainhash.Hash]*confNtfnSet),
		txsByInitialHeight:   make(map[uint32]map[chainhash.Hash]struct{}),
		ntfnsByConfirmHeight: make(map[uint32]map[*ConfNtfn]struct{}),
		spendNotifications:   make(map[wire.OutPoint]*spendNtfnSet),
		opsBySpendHeight:     make(map[uint32]map[wire.OutPoint]struct{}),
		confirmHintCache:     confirmHintCache,
		spendHintCache:       spendHintCache,
		quit:                 make(chan struct{}),
	}
}

// RegisterConf handles a new notification request. The client will be notified
// when the transaction gets a sufficient number of confirmations on the
// blockchain. The registration succeeds if no error is returned. If the
// returned HistoricalConfDispatch is non-nil, the caller is responsible for
// attempting to manually rescan blocks for the txid between the start and end
// heights.
//
// NOTE: If the transaction has already been included in a block on the chain,
// the confirmation details must be provided with the UpdateConfDetails method,
// otherwise we will wait for the transaction to confirm even though it already
// has.
func (n *TxNotifier) RegisterConf(ntfn *ConfNtfn) (*HistoricalConfDispatch, error) {
	select {
	case <-n.quit:
		return nil, ErrTxNotifierExiting
	default:
	}

	// Enforce that we will not dispatch confirmations beyond the reorg
	// safety limit.
	if ntfn.NumConfirmations > n.reorgSafetyLimit {
		return nil, ErrTxMaxConfs
	}

	// Before proceeding to register the notification, we'll query our
	// height hint cache to determine whether a better one exists.
	//
	// TODO(conner): verify that all submitted height hints are identical.
	startHeight := ntfn.HeightHint
	hint, err := n.confirmHintCache.QueryConfirmHint(*ntfn.TxID)
	if err == nil {
		if hint > startHeight {
			Log.Debugf("Using height hint %d retrieved "+
				"from cache for %v", hint, *ntfn.TxID)
			startHeight = hint
		}
	} else if err != ErrConfirmHintNotFound {
		Log.Errorf("Unable to query confirm hint for %v: %v",
			*ntfn.TxID, err)
	}

	n.Lock()
	defer n.Unlock()

	confSet, ok := n.confNotifications[*ntfn.TxID]
	if !ok {
		// If this is the first registration for this txid, construct a
		// confSet to coalesce all notifications for the same txid.
		confSet = newConfNtfnSet()
		n.confNotifications[*ntfn.TxID] = confSet
	}

	confSet.ntfns[ntfn.ConfID] = ntfn

	switch confSet.rescanStatus {

	// A prior rescan has already completed and we are actively watching at
	// tip for this txid.
	case rescanComplete:
		// If conf details for this set of notifications has already
		// been found, we'll attempt to deliver them immediately to this
		// client.
		Log.Debugf("Attempting to dispatch conf for txid=%v "+
			"on registration since rescan has finished", ntfn.TxID)
		return nil, n.dispatchConfDetails(ntfn, confSet.details)

	// A rescan is already in progress, return here to prevent dispatching
	// another. When the scan returns, this notifications details will be
	// updated as well.
	case rescanPending:
		Log.Debugf("Waiting for pending rescan to finish before "+
			"notifying txid=%v at tip", ntfn.TxID)
		return nil, nil

	// If no rescan has been dispatched, attempt to do so now.
	case rescanNotStarted:
	}

	// If the provided or cached height hint indicates that the transaction
	// is to be confirmed at a height greater than the conf notifier's
	// current height, we'll refrain from spawning a historical dispatch.
	if startHeight > n.currentHeight {
		Log.Debugf("Height hint is above current height, not dispatching "+
			"historical rescan for txid=%v ", ntfn.TxID)
		// Set the rescan status to complete, which will allow the conf
		// notifier to start delivering messages for this set
		// immediately.
		confSet.rescanStatus = rescanComplete
		return nil, nil
	}

	Log.Debugf("Dispatching historical rescan for txid=%v ", ntfn.TxID)

	// Construct the parameters for historical dispatch, scanning the range
	// of blocks between our best known height hint and the notifier's
	// current height. The notifier will begin also watching for
	// confirmations at tip starting with the next block.
	dispatch := &HistoricalConfDispatch{
		TxID:        ntfn.TxID,
		PkScript:    ntfn.PkScript,
		StartHeight: startHeight,
		EndHeight:   n.currentHeight,
	}

	// Set this confSet's status to pending, ensuring subsequent
	// registrations don't also attempt a dispatch.
	confSet.rescanStatus = rescanPending

	return dispatch, nil
}

// UpdateConfDetails attempts to update the confirmation details for an active
// notification within the notifier. This should only be used in the case of a
// transaction that has confirmed before the notifier's current height.
//
// NOTE: The notification should be registered first to ensure notifications are
// dispatched correctly.
func (n *TxNotifier) UpdateConfDetails(txid chainhash.Hash,
	details *TxConfirmation) error {

	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	// Ensure we hold the lock throughout handling the notification to
	// prevent the notifier from advancing its height underneath us.
	n.Lock()
	defer n.Unlock()

	// First, we'll determine whether we have an active notification for
	// this transaction with the given ID.
	confSet, ok := n.confNotifications[txid]
	if !ok {
		return fmt.Errorf("no notification found with TxID %v", txid)
	}

	// If the conf details were already found at tip, all existing
	// notifications will have been dispatched or queued for dispatch. We
	// can exit early to avoid sending too many notifications on the
	// buffered channels.
	if confSet.details != nil {
		return nil
	}

	// The historical dispatch has been completed for this confSet. We'll
	// update the rescan status and cache any details that were found. If
	// the details are nil, that implies we did not find them and will
	// continue to watch for them at tip.
	confSet.rescanStatus = rescanComplete

	// The notifier has yet to reach the height at which the transaction was
	// included in a block, so we should defer until handling it then within
	// ConnectTip.
	if details == nil {
		Log.Debugf("Conf details for txid=%v not found during "+
			"historical dispatch, waiting to dispatch at tip", txid)

		// We'll commit the current height as the confirm hint to
		// prevent another potentially long rescan if we restart before
		// a new block comes in.
		err := n.confirmHintCache.CommitConfirmHint(
			n.currentHeight, txid,
		)
		if err != nil {
			// The error is not fatal as this is an optimistic
			// optimization, so we'll avoid returning an error.
			Log.Debugf("Unable to update confirm hint to %d for "+
				"%v: %v", n.currentHeight, txid, err)
		}

		return nil
	}

	if details.BlockHeight > n.currentHeight {
		Log.Debugf("Conf details for txid=%v found above current "+
			"height, waiting to dispatch at tip", txid)
		return nil
	}

	Log.Debugf("Updating conf details for txid=%v details", txid)

	err := n.confirmHintCache.CommitConfirmHint(details.BlockHeight, txid)
	if err != nil {
		// The error is not fatal, so we should not return an error to
		// the caller.
		Log.Errorf("Unable to update confirm hint to %d for %v: %v",
			details.BlockHeight, txid, err)
	}

	// Cache the details found in the rescan and attempt to dispatch any
	// notifications that have not yet been delivered.
	confSet.details = details
	for _, ntfn := range confSet.ntfns {
		err = n.dispatchConfDetails(ntfn, details)
		if err != nil {
			return err
		}
	}

	return nil
}

// dispatchConfDetails attempts to cache and dispatch details to a particular
// client if the transaction has sufficiently confirmed. If the provided details
// are nil, this method will be a no-op.
func (n *TxNotifier) dispatchConfDetails(
	ntfn *ConfNtfn, details *TxConfirmation) error {

	// If no details are provided, return early as we can't dispatch.
	if details == nil {
		Log.Debugf("Unable to dispatch %v, no details provided",
			ntfn.TxID)
		return nil
	}

	// Now, we'll examine whether the transaction of this
	// notification request has reached its required number of
	// confirmations. If it has, we'll dispatch a confirmation
	// notification to the caller.
	confHeight := details.BlockHeight + ntfn.NumConfirmations - 1
	if confHeight <= n.currentHeight {
		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)

		// We'll send a 0 value to the Updates channel,
		// indicating that the transaction has already been
		// confirmed.
		select {
		case ntfn.Event.Updates <- 0:
		case <-n.quit:
			return ErrTxNotifierExiting
		}

		select {
		case ntfn.Event.Confirmed <- details:
			ntfn.dispatched = true
		case <-n.quit:
			return ErrTxNotifierExiting
		}
	} else {
		Log.Debugf("Queueing %v conf notification for %v at tip ",
			ntfn.NumConfirmations, ntfn.TxID)

		// Otherwise, we'll keep track of the notification
		// request by the height at which we should dispatch the
		// confirmation notification.
		ntfnSet, exists := n.ntfnsByConfirmHeight[confHeight]
		if !exists {
			ntfnSet = make(map[*ConfNtfn]struct{})
			n.ntfnsByConfirmHeight[confHeight] = ntfnSet
		}
		ntfnSet[ntfn] = struct{}{}

		// We'll also send an update to the client of how many
		// confirmations are left for the transaction to be
		// confirmed.
		numConfsLeft := confHeight - n.currentHeight
		select {
		case ntfn.Event.Updates <- numConfsLeft:
		case <-n.quit:
			return ErrTxNotifierExiting
		}
	}

	// As a final check, we'll also watch the transaction if it's
	// still possible for it to get reorged out of the chain.
	blockHeight := details.BlockHeight
	reorgSafeHeight := blockHeight + n.reorgSafetyLimit
	if reorgSafeHeight > n.currentHeight {
		txSet, exists := n.txsByInitialHeight[blockHeight]
		if !exists {
			txSet = make(map[chainhash.Hash]struct{})
			n.txsByInitialHeight[blockHeight] = txSet
		}
		txSet[*ntfn.TxID] = struct{}{}
	}

	return nil
}

// RegisterSpend handles a new spend notification request. The client will be
// notified once the outpoint is detected as spent within the chain.
//
// The registration succeeds if no error is returned. If the returned
// HistoricalSpendDisaptch is non-nil, the caller is responsible for attempting
// to determine whether the outpoint has been spent between the start and end
// heights.
//
// NOTE: If the outpoint has already been spent within the chain before the
// notifier's current tip, the spend details must be provided with the
// UpdateSpendDetails method, otherwise we will wait for the outpoint to
// be spent at tip, even though it already has.
func (n *TxNotifier) RegisterSpend(ntfn *SpendNtfn) (*HistoricalSpendDispatch, error) {
	select {
	case <-n.quit:
		return nil, ErrTxNotifierExiting
	default:
	}

	// Before proceeding to register the notification, we'll query our spend
	// hint cache to determine whether a better one exists.
	startHeight := ntfn.HeightHint
	hint, err := n.spendHintCache.QuerySpendHint(ntfn.OutPoint)
	if err == nil {
		if hint > startHeight {
			Log.Debugf("Using height hint %d retrieved from cache "+
				"for %v", startHeight, ntfn.OutPoint)
			startHeight = hint
		}
	} else if err != ErrSpendHintNotFound {
		Log.Errorf("Unable to query spend hint for %v: %v",
			ntfn.OutPoint, err)
	}

	n.Lock()
	defer n.Unlock()

	Log.Infof("New spend subscription: spend_id=%d, outpoint=%v, "+
		"height_hint=%d", ntfn.SpendID, ntfn.OutPoint, ntfn.HeightHint)

	// Keep track of the notification request so that we can properly
	// dispatch a spend notification later on.
	spendSet, ok := n.spendNotifications[ntfn.OutPoint]
	if !ok {
		// If this is the first registration for the outpoint, we'll
		// construct a spendNtfnSet to coalesce all notifications.
		spendSet = newSpendNtfnSet()
		n.spendNotifications[ntfn.OutPoint] = spendSet
	}
	spendSet.ntfns[ntfn.SpendID] = ntfn

	// We'll now let the caller know whether a historical rescan is needed
	// depending on the current rescan status.
	switch spendSet.rescanStatus {

	// If the spending details for this outpoint have already been
	// determined and cached, then we can use them to immediately dispatch
	// the spend notification to the client.
	case rescanComplete:
		return nil, n.dispatchSpendDetails(ntfn, spendSet.details)

	// If there is an active rescan to determine whether the outpoint has
	// been spent, then we won't trigger another one.
	case rescanPending:
		return nil, nil

	// Otherwise, we'll fall through and let the caller know that a rescan
	// should be dispatched to determine whether the outpoint has already
	// been spent.
	case rescanNotStarted:
	}

	// However, if the spend hint, either provided by the caller or
	// retrieved from the cache, is found to be at a later height than the
	// TxNotifier is aware of, then we'll refrain from dispatching a
	// historical rescan and wait for the spend to come in at tip.
	if startHeight > n.currentHeight {
		Log.Debugf("Spend hint of %d for %v is above current height %d",
			startHeight, ntfn.OutPoint, n.currentHeight)

		// We'll also set the rescan status as complete to ensure that
		// spend hints for this outpoint get updated upon
		// connected/disconnected blocks.
		spendSet.rescanStatus = rescanComplete
		return nil, nil
	}

	// We'll set the rescan status to pending to ensure subsequent
	// notifications don't also attempt a historical dispatch.
	spendSet.rescanStatus = rescanPending

	return &HistoricalSpendDispatch{
		OutPoint:    ntfn.OutPoint,
		PkScript:    ntfn.PkScript,
		StartHeight: startHeight,
		EndHeight:   n.currentHeight,
	}, nil
}

// CancelSpend cancels an existing request for a spend notification of an
// outpoint. The request is identified by its spend ID.
func (n *TxNotifier) CancelSpend(op wire.OutPoint, spendID uint64) {
	select {
	case <-n.quit:
		return
	default:
	}

	n.Lock()
	defer n.Unlock()

	Log.Infof("Canceling spend notification: spend_id=%d, outpoint=%v",
		spendID, op)

	spendSet, ok := n.spendNotifications[op]
	if !ok {
		return
	}
	ntfn, ok := spendSet.ntfns[spendID]
	if !ok {
		return
	}

	// We'll close all the notification channels to let the client know
	// their cancel request has been fulfilled.
	close(ntfn.Event.Spend)
	close(ntfn.Event.Reorg)
	delete(spendSet.ntfns, spendID)
}

// ProcessRelevantSpendTx processes a transaction provided externally. This will
// check whether the transaction is relevant to the notifier if it spends any
// outputs for which we currently have registered notifications for. If it is
// relevant, spend notifications will be dispatched to the caller.
func (n *TxNotifier) ProcessRelevantSpendTx(tx *wire.MsgTx, txHeight int32) error {
	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	// Ensure we hold the lock throughout handling the notification to
	// prevent the notifier from advancing its height underneath us.
	n.Lock()
	defer n.Unlock()

	// Grab the set of active registered outpoints to determine if the
	// transaction spends any of them.
	spendNtfns := n.spendNotifications

	// We'll check if this transaction spends an output that has an existing
	// spend notification for it.
	for i, txIn := range tx.TxIn {
		// If this input doesn't spend an existing registered outpoint,
		// we'll go on to the next.
		prevOut := txIn.PreviousOutPoint
		if _, ok := spendNtfns[prevOut]; !ok {
			continue
		}

		// Otherwise, we'll create a spend summary and send off the
		// details to the notification subscribers.
		txHash := tx.TxHash()
		details := &SpendDetail{
			SpentOutPoint:     &prevOut,
			SpenderTxHash:     &txHash,
			SpendingTx:        tx,
			SpenderInputIndex: uint32(i),
			SpendingHeight:    txHeight,
		}
		if err := n.updateSpendDetails(prevOut, details); err != nil {
			return err
		}
	}

	return nil
}

// UpdateSpendDetails attempts to update the spend details for all active spend
// notification requests for an outpoint. This method should be used once a
// historical scan of the chain has finished. If the historical scan did not
// find a spending transaction for the outpoint, the spend details may be nil.
//
// NOTE: A notification request for the outpoint must be registered first to
// ensure notifications are delivered.
func (n *TxNotifier) UpdateSpendDetails(op wire.OutPoint,
	details *SpendDetail) error {

	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	// Ensure we hold the lock throughout handling the notification to
	// prevent the notifier from advancing its height underneath us.
	n.Lock()
	defer n.Unlock()

	return n.updateSpendDetails(op, details)
}

// updateSpendDetails attempts to update the spend details for all active spend
// notification requests for an outpoint. This method should be used once a
// historical scan of the chain has finished. If the historical scan did not
// find a spending transaction for the outpoint, the spend details may be nil.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) updateSpendDetails(op wire.OutPoint,
	details *SpendDetail) error {

	// Mark the ongoing historical rescan for this outpoint as finished.
	// This will allow us to update the spend hints for this outpoint at
	// tip.
	spendSet, ok := n.spendNotifications[op]
	if !ok {
		return fmt.Errorf("no notifications found for outpoint %v", op)
	}

	// If the spend details have already been found either at tip, then the
	// notifications should have already been dispatched, so we can exit
	// early to prevent sending duplicate notifications.
	if spendSet.details != nil {
		return nil
	}

	// Since the historical rescan has completed for this outpoint, we'll
	// mark its rescan status as complete in order to ensure that the
	// TxNotifier can properly update its spend hints upon
	// connected/disconnected blocks.
	spendSet.rescanStatus = rescanComplete

	// If the historical rescan was not able to find a spending transaction
	// for this outpoint, then we can track the spend at tip.
	if details == nil {
		// We'll commit the current height as the spend hint to prevent
		// another potentially long rescan if we restart before a new
		// block comes in.
		err := n.spendHintCache.CommitSpendHint(n.currentHeight, op)
		if err != nil {
			// The error is not fatal as this is an optimistic
			// optimization, so we'll avoid returning an error.
			Log.Debugf("Unable to update spend hint to %d for %v: %v",
				n.currentHeight, op, err)
		}

		return nil
	}

	// If the historical rescan found the spending transaction for this
	// outpoint, but it's at a later height than the notifier (this can
	// happen due to latency with the backend during a reorg), then we'll
	// defer handling the notification until the notifier has caught up to
	// such height.
	if uint32(details.SpendingHeight) > n.currentHeight {
		return nil
	}

	// Now that we've determined the outpoint has been spent, we'll commit
	// its spending height as its hint in the cache and dispatch
	// notifications to all of its respective clients.
	err := n.spendHintCache.CommitSpendHint(
		uint32(details.SpendingHeight), op,
	)
	if err != nil {
		// The error is not fatal as this is an optimistic optimization,
		// so we'll avoid returning an error.
		Log.Debugf("Unable to update spend hint to %d for %v: %v",
			details.SpendingHeight, op, err)
	}

	spendSet.details = details
	for _, ntfn := range spendSet.ntfns {
		err := n.dispatchSpendDetails(ntfn, spendSet.details)
		if err != nil {
			return err
		}
	}

	return nil
}

// dispatchSpendDetails dispatches a spend notification to the client.
//
// NOTE: This must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchSpendDetails(ntfn *SpendNtfn, details *SpendDetail) error {
	// If there are no spend details to dispatch or if the notification has
	// already been dispatched, then we can skip dispatching to this client.
	if details == nil || ntfn.dispatched {
		return nil
	}

	Log.Infof("Dispatching spend notification for outpoint=%v at height=%d",
		ntfn.OutPoint, n.currentHeight)

	select {
	case ntfn.Event.Spend <- details:
		ntfn.dispatched = true
	case <-n.quit:
		return ErrTxNotifierExiting
	}

	return nil
}

// ConnectTip handles a new block extending the current chain. It will go
// through every transaction and determine if it is relevant to any of its
// clients. A transaction can be relevant in either of the following two ways:
//
//   1. One of the inputs in the transaction spends an outpoint for which we
//   currently have an active spend registration for.
//
//   2. The transaction is a transaction for which we currently have an active
//   confirmation registration for.
//
// In the event that the transaction is relevant, a confirmation/spend
// notification will be queued for dispatch to the relevant clients.
// Confirmation notifications will only be dispatched for transactions that have
// met the required number of confirmations required by the client.
//
// NOTE: In order to actually dispatch the relevant transaction notifications to
// clients, NotifyHeight must be called with the same block height in order to
// maintain correctness.
func (n *TxNotifier) ConnectTip(blockHash *chainhash.Hash, blockHeight uint32,
	txns []*btcutil.Tx) error {

	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	n.Lock()
	defer n.Unlock()

	if blockHeight != n.currentHeight+1 {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, new height=%d",
			n.currentHeight, blockHeight)
	}
	n.currentHeight++
	n.reorgDepth = 0

	// First, we'll iterate over all the transactions found in this block to
	// determine if it includes any relevant transactions to the TxNotifier.
	for _, tx := range txns {
		txHash := tx.Hash()

		// In order to determine if this transaction is relevant to the
		// notifier, we'll check its inputs for any outstanding spend
		// notifications.
		for i, txIn := range tx.MsgTx().TxIn {
			prevOut := txIn.PreviousOutPoint
			spendSet, ok := n.spendNotifications[prevOut]
			if !ok {
				continue
			}

			// If we have any, we'll record its spend height so that
			// notifications get dispatched to the respective
			// clients.
			spendDetails := &SpendDetail{
				SpentOutPoint:     &prevOut,
				SpenderTxHash:     txHash,
				SpendingTx:        tx.MsgTx(),
				SpenderInputIndex: uint32(i),
				SpendingHeight:    int32(blockHeight),
			}

			// TODO(wilmer): cancel pending historical rescans if any?
			spendSet.rescanStatus = rescanComplete
			spendSet.details = spendDetails
			for _, ntfn := range spendSet.ntfns {
				// In the event that this notification was aware
				// that the spending transaction of its outpoint
				// was reorged out of the chain, we'll consume
				// the reorg notification if it hasn't been
				// done yet already.
				select {
				case <-ntfn.Event.Reorg:
				default:
				}
			}

			// We'll note the outpoints spending height in order to
			// correctly handle dispatching notifications when the
			// spending transactions gets reorged out of the chain.
			opSet, exists := n.opsBySpendHeight[blockHeight]
			if !exists {
				opSet = make(map[wire.OutPoint]struct{})
				n.opsBySpendHeight[blockHeight] = opSet
			}
			opSet[prevOut] = struct{}{}
		}

		// Check if we have any pending notifications for this txid. If
		// none are found, we can proceed to the next transaction.
		confSet, ok := n.confNotifications[*txHash]
		if !ok {
			continue
		}

		Log.Debugf("Block contains txid=%v, constructing details",
			txHash)

		// If we have any, we'll record its confirmed height so that
		// notifications get dispatched when the transaction reaches the
		// clients' desired number of confirmations.
		details := &TxConfirmation{
			BlockHash:   blockHash,
			BlockHeight: blockHeight,
			TxIndex:     uint32(tx.Index()),
		}

		// TODO(wilmer): cancel pending historical rescans if any?
		confSet.rescanStatus = rescanComplete
		confSet.details = details
		for _, ntfn := range confSet.ntfns {
			// In the event that this notification was aware that
			// the transaction was reorged out of the chain, we'll
			// consume the reorg notification if it hasn't been done
			// yet already.
			select {
			case <-ntfn.Event.NegativeConf:
			default:
			}

			// We'll note this client's required number of
			// confirmations so that we can notify them when
			// expected.
			confHeight := blockHeight + ntfn.NumConfirmations - 1
			ntfnSet, exists := n.ntfnsByConfirmHeight[confHeight]
			if !exists {
				ntfnSet = make(map[*ConfNtfn]struct{})
				n.ntfnsByConfirmHeight[confHeight] = ntfnSet
			}
			ntfnSet[ntfn] = struct{}{}

			// We'll also note the initial confirmation height in
			// order to correctly handle dispatching notifications
			// when the transaction gets reorged out of the chain.
			txSet, exists := n.txsByInitialHeight[blockHeight]
			if !exists {
				txSet = make(map[chainhash.Hash]struct{})
				n.txsByInitialHeight[blockHeight] = txSet
			}
			txSet[*txHash] = struct{}{}
		}
	}

	// Finally, now that we've determined which transactions were confirmed
	// and which outpoints were spent within the new block, we can update
	// their entries in their respective caches, along with all of our
	// unconfirmed transactions and unspent outpoints.
	n.updateHints(blockHeight)

	return nil
}

// NotifyHeight dispatches confirmation and spend notifications to the clients
// who registered for a notification which has been fulfilled at the passed
// height.
func (n *TxNotifier) NotifyHeight(height uint32) error {
	n.Lock()
	defer n.Unlock()

	// First, we'll dispatch an update to all of the notification clients
	// for our watched transactions with the number of confirmations left at
	// this new height.
	for _, txHashes := range n.txsByInitialHeight {
		for txHash := range txHashes {
			confSet := n.confNotifications[txHash]
			for _, ntfn := range confSet.ntfns {
				txConfHeight := confSet.details.BlockHeight +
					ntfn.NumConfirmations - 1
				numConfsLeft := txConfHeight - height

				// Since we don't clear notifications until
				// transactions are no longer under the risk of
				// being reorganized out of the chain, we'll
				// skip sending updates for transactions that
				// have already been confirmed.
				if int32(numConfsLeft) < 0 {
					continue
				}

				select {
				case ntfn.Event.Updates <- numConfsLeft:
				case <-n.quit:
					return ErrTxNotifierExiting
				}
			}
		}
	}

	// Then, we'll dispatch notifications for all the transactions that have
	// become confirmed at this new block height.
	for ntfn := range n.ntfnsByConfirmHeight[height] {
		confSet := n.confNotifications[*ntfn.TxID]

		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)

		select {
		case ntfn.Event.Confirmed <- confSet.details:
			ntfn.dispatched = true
		case <-n.quit:
			return ErrTxNotifierExiting
		}
	}
	delete(n.ntfnsByConfirmHeight, height)

	// We'll also dispatch spend notifications for all the outpoints that
	// were spent at this new block height.
	for op := range n.opsBySpendHeight[height] {
		spendSet := n.spendNotifications[op]
		for _, ntfn := range spendSet.ntfns {
			err := n.dispatchSpendDetails(ntfn, spendSet.details)
			if err != nil {
				return err
			}
		}
	}

	// Finally, we'll clear the entries from our set of notifications for
	// transactions and outpoints that are no longer under the risk of being
	// reorged out of the chain.
	if height >= n.reorgSafetyLimit {
		matureBlockHeight := height - n.reorgSafetyLimit
		for tx := range n.txsByInitialHeight[matureBlockHeight] {
			delete(n.confNotifications, tx)
		}
		delete(n.txsByInitialHeight, matureBlockHeight)
		for op := range n.opsBySpendHeight[matureBlockHeight] {
			delete(n.spendNotifications, op)
		}
		delete(n.opsBySpendHeight, matureBlockHeight)
	}

	return nil
}

// DisconnectTip handles the tip of the current chain being disconnected during
// a chain reorganization. If any watched transactions or spending transactions
// for registered outpoints were included in this block, internal structures are
// updated to ensure confirmation/spend notifications are consumed (if not
// already), and reorg notifications are dispatched instead. Confirmation/spend
// notifications will be dispatched again upon block inclusion.
func (n *TxNotifier) DisconnectTip(blockHeight uint32) error {
	select {
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	n.Lock()
	defer n.Unlock()

	if blockHeight != n.currentHeight {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, disconnected height=%d",
			n.currentHeight, blockHeight)
	}
	n.currentHeight--
	n.reorgDepth++

	// With the block disconnected, we'll update the confirm and spend hints
	// for our transactions and outpoints to reflect the new height, except
	// for those that have confirmed/spent at previous heights.
	n.updateHints(blockHeight)

	// We'll go through all of our watched transactions and attempt to drain
	// their notification channels to ensure sending notifications to the
	// clients is always non-blocking.
	for initialHeight, txHashes := range n.txsByInitialHeight {
		for txHash := range txHashes {
			// If the transaction has been reorged out of the chain,
			// we'll make sure to remove the cached confirmation
			// details to prevent notifying clients with old
			// information.
			confSet := n.confNotifications[txHash]
			if initialHeight == blockHeight {
				confSet.details = nil
			}

			for _, ntfn := range confSet.ntfns {
				// First, we'll attempt to drain an update
				// from each notification to ensure sends to the
				// Updates channel are always non-blocking.
				select {
				case <-ntfn.Event.Updates:
				case <-n.quit:
					return ErrTxNotifierExiting
				default:
				}

				// Then, we'll check if the current transaction
				// was included in the block currently being
				// disconnected. If it was, we'll need to
				// dispatch a reorg notification to the client.
				if initialHeight == blockHeight {
					err := n.dispatchConfReorg(
						ntfn, blockHeight,
					)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// We'll also go through our watched outpoints and attempt to drain
	// their dispatched notifications to ensure dispatching notifications to
	// clients later on is always non-blocking.  We're only interested in
	// outpoints whose spending transaction was included at the height being
	// disconnected.
	for op := range n.opsBySpendHeight[blockHeight] {
		// Since the spending transaction is being reorged out of the
		// chain, we'll need to clear out the spending details of the
		// outpoint.
		spendSet := n.spendNotifications[op]
		spendSet.details = nil

		// For all requests which have had a spend notification
		// dispatched, we'll attempt to drain it and send a reorg
		// notification instead.
		for _, ntfn := range spendSet.ntfns {
			if err := n.dispatchSpendReorg(ntfn); err != nil {
				return err
			}
		}
	}

	// Finally, we can remove the transactions that were confirmed and the
	// outpoints that were spent at the height being disconnected. We'll
	// still continue to track them until they have been confirmed/spent and
	// are no longer under the risk of being reorged out of the chain again.
	delete(n.txsByInitialHeight, blockHeight)
	delete(n.opsBySpendHeight, blockHeight)

	return nil
}

// updateHints attempts to update the confirm and spend hints for all relevant
// transactions and outpoints respectively. The height parameter is used to
// determine which transactions and outpoints we should update based on whether
// a new block is being connected/disconnected.
//
// NOTE: This must be called with the TxNotifier's lock held and after its
// height has already been reflected by a block being connected/disconnected.
func (n *TxNotifier) updateHints(height uint32) {
	// TODO(wilmer): update under one database transaction.
	//
	// To update the height hint for all the required transactions under one
	// database transaction, we'll gather the set of unconfirmed
	// transactions along with the ones that confirmed at the height being
	// connected/disconnected.
	txsToUpdateHints := n.unconfirmedTxs()
	for confirmedTx := range n.txsByInitialHeight[height] {
		txsToUpdateHints = append(txsToUpdateHints, confirmedTx)
	}
	err := n.confirmHintCache.CommitConfirmHint(
		n.currentHeight, txsToUpdateHints...,
	)
	if err != nil {
		// The error is not fatal as this is an optimistic optimization,
		// so we'll avoid returning an error.
		Log.Debugf("Unable to update confirm hints to %d for "+
			"%v: %v", n.currentHeight, txsToUpdateHints, err)
	}

	// Similarly, to update the height hint for all the required outpoints
	// under one database transaction, we'll gather the set of unspent
	// outpoints along with the ones that were spent at the height being
	// connected/disconnected.
	opsToUpdateHints := n.unspentOutPoints()
	for spentOp := range n.opsBySpendHeight[height] {
		opsToUpdateHints = append(opsToUpdateHints, spentOp)
	}
	err = n.spendHintCache.CommitSpendHint(
		n.currentHeight, opsToUpdateHints...,
	)
	if err != nil {
		// The error is not fatal as this is an optimistic optimization,
		// so we'll avoid returning an error.
		Log.Debugf("Unable to update spend hints to %d for "+
			"%v: %v", n.currentHeight, opsToUpdateHints, err)
	}
}

// unconfirmedTxs returns the set of transactions that are still seen as
// unconfirmed by the TxNotifier.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) unconfirmedTxs() []chainhash.Hash {
	var unconfirmedTxs []chainhash.Hash
	for tx, confNtfnSet := range n.confNotifications {
		// If the notification is already aware of its confirmation
		// details, or it's in the process of learning them, we'll skip
		// it as we can't yet determine if it's confirmed or not.
		if confNtfnSet.rescanStatus != rescanComplete ||
			confNtfnSet.details != nil {
			continue
		}

		unconfirmedTxs = append(unconfirmedTxs, tx)
	}

	return unconfirmedTxs
}

// unspentOutPoints returns the set of outpoints that are still seen as unspent
// by the TxNotifier.
//
// NOTE: This method must be called with the TxNotifier's lock held.
func (n *TxNotifier) unspentOutPoints() []wire.OutPoint {
	var unspentOps []wire.OutPoint
	for op, spendNtfnSet := range n.spendNotifications {
		// If the notification is already aware of its spend details, or
		// it's in the process of learning them, we'll skip it as we
		// can't yet determine if it's unspent or not.
		if spendNtfnSet.rescanStatus != rescanComplete ||
			spendNtfnSet.details != nil {
			continue
		}

		unspentOps = append(unspentOps, op)
	}

	return unspentOps
}

// dispatchConfReorg dispatches a reorg notification to the client if the
// confirmation notification was already delivered.
//
// NOTE: This must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchConfReorg(ntfn *ConfNtfn,
	heightDisconnected uint32) error {

	// If the transaction's confirmation notification has yet to be
	// dispatched, we'll need to clear its entry within the
	// ntfnsByConfirmHeight index to prevent from notifying the client once
	// the notifier reaches the confirmation height.
	if !ntfn.dispatched {
		confHeight := heightDisconnected + ntfn.NumConfirmations - 1
		ntfnSet, exists := n.ntfnsByConfirmHeight[confHeight]
		if exists {
			delete(ntfnSet, ntfn)
		}
		return nil
	}

	// Otherwise, the entry within the ntfnsByConfirmHeight has already been
	// deleted, so we'll attempt to drain the confirmation notification to
	// ensure sends to the Confirmed channel are always non-blocking.
	select {
	case <-ntfn.Event.Confirmed:
	case <-n.quit:
		return ErrTxNotifierExiting
	default:
	}

	ntfn.dispatched = false

	// Send a negative confirmation notification to the client indicating
	// how many blocks have been disconnected successively.
	select {
	case ntfn.Event.NegativeConf <- int32(n.reorgDepth):
	case <-n.quit:
		return ErrTxNotifierExiting
	}

	return nil
}

// dispatchSpendReorg dispatches a reorg notification to the client if a spend
// notiification was already delivered.
//
// NOTE: This must be called with the TxNotifier's lock held.
func (n *TxNotifier) dispatchSpendReorg(ntfn *SpendNtfn) error {
	if !ntfn.dispatched {
		return nil
	}

	// Attempt to drain the spend notification to ensure sends to the Spend
	// channel are always non-blocking.
	select {
	case <-ntfn.Event.Spend:
	default:
	}

	// Send a reorg notification to the client in order for them to
	// correctly handle reorgs.
	select {
	case ntfn.Event.Reorg <- struct{}{}:
	case <-n.quit:
		return ErrTxNotifierExiting
	}

	ntfn.dispatched = false

	return nil
}

// TearDown is to be called when the owner of the TxNotifier is exiting. This
// closes the event channels of all registered notifications that have not been
// dispatched yet.
func (n *TxNotifier) TearDown() {
	n.Lock()
	defer n.Unlock()

	close(n.quit)

	for _, confSet := range n.confNotifications {
		for _, ntfn := range confSet.ntfns {
			close(ntfn.Event.Confirmed)
			close(ntfn.Event.Updates)
			close(ntfn.Event.NegativeConf)
		}
	}

	for _, spendSet := range n.spendNotifications {
		for _, ntfn := range spendSet.ntfns {
			close(ntfn.Event.Spend)
			close(ntfn.Event.Reorg)
		}
	}
}
