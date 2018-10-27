package chainntnfs

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
)

var (
	// ErrTxConfNotifierExiting is an error returned when attempting to
	// interact with the TxConfNotifier but it been shut down.
	ErrTxConfNotifierExiting = errors.New("TxConfNotifier is exiting")

	// ErrTxMaxConfs signals that the user requested a number of
	// confirmations beyond the reorg safety limit.
	ErrTxMaxConfs = errors.New("too many confirmations requested")
)

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

// NewConfirmationEvent constructs a new ConfirmationEvent with newly opened
// channels.
func NewConfirmationEvent(numConfs uint32) *ConfirmationEvent {
	return &ConfirmationEvent{
		Confirmed:    make(chan *TxConfirmation, 1),
		Updates:      make(chan uint32, numConfs),
		NegativeConf: make(chan int32, 1),
	}
}

// TxConfNotifier is used to register transaction confirmation notifications and
// dispatch them as the transactions confirm. A client can request to be
// notified when a particular transaction has sufficient on-chain confirmations
// (or be notified immediately if the tx already does), and the TxConfNotifier
// will watch changes to the blockchain in order to satisfy these requests.
type TxConfNotifier struct {
	// currentHeight is the height of the tracked blockchain. It is used to
	// determine the number of confirmations a tx has and ensure blocks are
	// connected and disconnected in order.
	currentHeight uint32

	// reorgSafetyLimit is the chain depth beyond which it is assumed a block
	// will not be reorganized out of the chain. This is used to determine when
	// to prune old confirmation requests so that reorgs are handled correctly.
	// The coinbase maturity period is a reasonable value to use.
	reorgSafetyLimit uint32

	// reorgDepth is the depth of a chain organization that this system is being
	// informed of. This is incremented as long as a sequence of blocks are
	// disconnected without being interrupted by a new block.
	reorgDepth uint32

	// confNotifications is an index of notification requests by transaction
	// hash.
	confNotifications map[chainhash.Hash]*confNtfnSet

	// txsByInitialHeight is an index of watched transactions by the height
	// that they are included at in the blockchain. This is tracked so that
	// incorrect notifications are not sent if a transaction is reorganized
	// out of the chain and so that negative confirmations can be recognized.
	txsByInitialHeight map[uint32]map[chainhash.Hash]struct{}

	// ntfnsByConfirmHeight is an index of notification requests by the height
	// at which the transaction will have sufficient confirmations.
	ntfnsByConfirmHeight map[uint32]map[*ConfNtfn]struct{}

	// hintCache is a cache used to maintain the latest height hints for
	// transactions. Each height hint represents the earliest height at
	// which the transactions could have been confirmed within the chain.
	hintCache ConfirmHintCache

	// quit is closed in order to signal that the notifier is gracefully
	// exiting.
	quit chan struct{}

	sync.Mutex
}

// rescanState indicates the progression of a registration before the notifier
// can begin dispatching confirmations at tip.
type rescanState uint8

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
	ntfns        map[uint64]*ConfNtfn
	rescanStatus rescanState
	details      *TxConfirmation
}

// newConfNtfnSet constructs a fresh confNtfnSet for a group of clients
// interested in a notification for a particular txid.
func newConfNtfnSet() *confNtfnSet {
	return &confNtfnSet{
		ntfns:        make(map[uint64]*ConfNtfn),
		rescanStatus: rescanNotStarted,
	}
}

// NewTxConfNotifier creates a TxConfNotifier. The current height of the
// blockchain is accepted as a parameter.
func NewTxConfNotifier(startHeight uint32, reorgSafetyLimit uint32,
	hintCache ConfirmHintCache) *TxConfNotifier {

	return &TxConfNotifier{
		currentHeight:        startHeight,
		reorgSafetyLimit:     reorgSafetyLimit,
		confNotifications:    make(map[chainhash.Hash]*confNtfnSet),
		txsByInitialHeight:   make(map[uint32]map[chainhash.Hash]struct{}),
		ntfnsByConfirmHeight: make(map[uint32]map[*ConfNtfn]struct{}),
		hintCache:            hintCache,
		quit:                 make(chan struct{}),
	}
}

// Register handles a new notification request. The client will be notified when
// the transaction gets a sufficient number of confirmations on the blockchain.
// The registration succeeds if no error is returned. If the returned
// HistoricalConfDispatch is non-nil, the caller is responsible for attempting
// to manually rescan blocks for the txid between the start and end heights.
//
// NOTE: If the transaction has already been included in a block on the chain,
// the confirmation details must be provided with the UpdateConfDetails method,
// otherwise we will wait for the transaction to confirm even though it already
// has.
func (tcn *TxConfNotifier) Register(
	ntfn *ConfNtfn) (*HistoricalConfDispatch, error) {

	select {
	case <-tcn.quit:
		return nil, ErrTxConfNotifierExiting
	default:
	}

	// Enforce that we will not dispatch confirmations beyond the reorg
	// safety limit.
	if ntfn.NumConfirmations > tcn.reorgSafetyLimit {
		return nil, ErrTxMaxConfs
	}

	// Before proceeding to register the notification, we'll query our
	// height hint cache to determine whether a better one exists.
	//
	// TODO(conner): verify that all submitted height hints are identical.
	startHeight := ntfn.HeightHint
	hint, err := tcn.hintCache.QueryConfirmHint(*ntfn.TxID)
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

	tcn.Lock()
	defer tcn.Unlock()

	confSet, ok := tcn.confNotifications[*ntfn.TxID]
	if !ok {
		// If this is the first registration for this txid, construct a
		// confSet to coalesce all notifications for the same txid.
		confSet = newConfNtfnSet()
		tcn.confNotifications[*ntfn.TxID] = confSet
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
		return nil, tcn.dispatchConfDetails(ntfn, confSet.details)

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
	if startHeight > tcn.currentHeight {
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
		EndHeight:   tcn.currentHeight,
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
func (tcn *TxConfNotifier) UpdateConfDetails(txid chainhash.Hash,
	details *TxConfirmation) error {

	select {
	case <-tcn.quit:
		return ErrTxConfNotifierExiting
	default:
	}

	// Ensure we hold the lock throughout handling the notification to
	// prevent the notifier from advancing its height underneath us.
	tcn.Lock()
	defer tcn.Unlock()

	// First, we'll determine whether we have an active notification for
	// this transaction with the given ID.
	confSet, ok := tcn.confNotifications[txid]
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
		return nil
	}

	if details.BlockHeight > tcn.currentHeight {
		Log.Debugf("Conf details for txid=%v found above current "+
			"height, waiting to dispatch at tip", txid)
		return nil
	}

	Log.Debugf("Updating conf details for txid=%v details", txid)

	err := tcn.hintCache.CommitConfirmHint(details.BlockHeight, txid)
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
		err = tcn.dispatchConfDetails(ntfn, details)
		if err != nil {
			return err
		}
	}

	return nil
}

// dispatchConfDetails attempts to cache and dispatch details to a particular
// client if the transaction has sufficiently confirmed. If the provided details
// are nil, this method will be a no-op.
func (tcn *TxConfNotifier) dispatchConfDetails(
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
	if confHeight <= tcn.currentHeight {
		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)

		// We'll send a 0 value to the Updates channel,
		// indicating that the transaction has already been
		// confirmed.
		select {
		case ntfn.Event.Updates <- 0:
		case <-tcn.quit:
			return ErrTxConfNotifierExiting
		}

		select {
		case ntfn.Event.Confirmed <- details:
			ntfn.dispatched = true
		case <-tcn.quit:
			return ErrTxConfNotifierExiting
		}
	} else {
		Log.Debugf("Queueing %v conf notification for %v at tip ",
			ntfn.NumConfirmations, ntfn.TxID)

		// Otherwise, we'll keep track of the notification
		// request by the height at which we should dispatch the
		// confirmation notification.
		ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
		if !exists {
			ntfnSet = make(map[*ConfNtfn]struct{})
			tcn.ntfnsByConfirmHeight[confHeight] = ntfnSet
		}
		ntfnSet[ntfn] = struct{}{}

		// We'll also send an update to the client of how many
		// confirmations are left for the transaction to be
		// confirmed.
		numConfsLeft := confHeight - tcn.currentHeight
		select {
		case ntfn.Event.Updates <- numConfsLeft:
		case <-tcn.quit:
			return ErrTxConfNotifierExiting
		}
	}

	// As a final check, we'll also watch the transaction if it's
	// still possible for it to get reorged out of the chain.
	blockHeight := details.BlockHeight
	reorgSafeHeight := blockHeight + tcn.reorgSafetyLimit
	if reorgSafeHeight > tcn.currentHeight {
		txSet, exists := tcn.txsByInitialHeight[blockHeight]
		if !exists {
			txSet = make(map[chainhash.Hash]struct{})
			tcn.txsByInitialHeight[blockHeight] = txSet
		}
		txSet[*ntfn.TxID] = struct{}{}
	}

	return nil
}

// ConnectTip handles a new block extending the current chain. This checks each
// transaction in the block to see if any watched transactions are included.
// Also, if any watched transactions now have the required number of
// confirmations as a result of this block being connected, this dispatches
// notifications.
func (tcn *TxConfNotifier) ConnectTip(blockHash *chainhash.Hash,
	blockHeight uint32, txns []*btcutil.Tx) error {

	select {
	case <-tcn.quit:
		return ErrTxConfNotifierExiting
	default:
	}

	tcn.Lock()
	defer tcn.Unlock()

	if blockHeight != tcn.currentHeight+1 {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, new height=%d",
			tcn.currentHeight, blockHeight)
	}
	tcn.currentHeight++
	tcn.reorgDepth = 0

	// Record any newly confirmed transactions by their confirmed height so
	// that notifications get dispatched when the transactions reach their
	// required number of confirmations. We'll also watch these transactions
	// at the height they were included in the chain so reorgs can be
	// handled correctly.
	for _, tx := range txns {
		txHash := tx.Hash()

		// Check if we have any pending notifications for this txid. If
		// none are found, we can proceed to the next transaction.
		confSet, ok := tcn.confNotifications[*txHash]
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
			ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
			if !exists {
				ntfnSet = make(map[*ConfNtfn]struct{})
				tcn.ntfnsByConfirmHeight[confHeight] = ntfnSet
			}
			ntfnSet[ntfn] = struct{}{}

			// We'll also note the initial confirmation height in
			// order to correctly handle dispatching notifications
			// when the transaction gets reorged out of the chain.
			txSet, exists := tcn.txsByInitialHeight[blockHeight]
			if !exists {
				txSet = make(map[chainhash.Hash]struct{})
				tcn.txsByInitialHeight[blockHeight] = txSet
			}
			txSet[*txHash] = struct{}{}
		}
	}

	// In order to update the height hint for all the required transactions
	// under one database transaction, we'll gather the set of unconfirmed
	// transactions along with the ones that confirmed at the current
	// height. To do so, we'll iterate over the confNotifications map, which
	// contains the transactions we currently have notifications for. Since
	// this map doesn't tell us whether the transaction has confirmed or
	// not, we'll need to look at txsByInitialHeight to determine so.
	var txsToUpdateHints []chainhash.Hash
	for confirmedTx := range tcn.txsByInitialHeight[tcn.currentHeight] {
		txsToUpdateHints = append(txsToUpdateHints, confirmedTx)
	}
out:
	for maybeUnconfirmedTx, confSet := range tcn.confNotifications {
		// We shouldn't update the confirm hints if we still have a
		// pending rescan in progress. We'll skip writing any for
		// notification sets that haven't reached rescanComplete.
		if confSet.rescanStatus != rescanComplete {
			continue
		}

		for height, confirmedTxs := range tcn.txsByInitialHeight {
			// Skip the transactions that confirmed at the new block
			// height as those have already been added.
			if height == blockHeight {
				continue
			}

			// If the transaction was found within the set of
			// confirmed transactions at this height, we'll skip it.
			if _, ok := confirmedTxs[maybeUnconfirmedTx]; ok {
				continue out
			}
		}
		txsToUpdateHints = append(txsToUpdateHints, maybeUnconfirmedTx)
	}

	if len(txsToUpdateHints) > 0 {
		err := tcn.hintCache.CommitConfirmHint(
			tcn.currentHeight, txsToUpdateHints...,
		)
		if err != nil {
			// The error is not fatal, so we should not return an
			// error to the caller.
			Log.Errorf("Unable to update confirm hint to %d for "+
				"%v: %v", tcn.currentHeight, txsToUpdateHints,
				err)
		}
	}

	// Next, we'll dispatch an update to all of the notification clients for
	// our watched transactions with the number of confirmations left at
	// this new height.
	for _, txHashes := range tcn.txsByInitialHeight {
		for txHash := range txHashes {
			confSet := tcn.confNotifications[txHash]
			for _, ntfn := range confSet.ntfns {
				txConfHeight := confSet.details.BlockHeight +
					ntfn.NumConfirmations - 1
				numConfsLeft := txConfHeight - blockHeight

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
				case <-tcn.quit:
					return ErrTxConfNotifierExiting
				}
			}
		}
	}

	// Then, we'll dispatch notifications for all the transactions that have
	// become confirmed at this new block height.
	for ntfn := range tcn.ntfnsByConfirmHeight[blockHeight] {
		confSet := tcn.confNotifications[*ntfn.TxID]

		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)

		select {
		case ntfn.Event.Confirmed <- confSet.details:
			ntfn.dispatched = true
		case <-tcn.quit:
			return ErrTxConfNotifierExiting
		}
	}
	delete(tcn.ntfnsByConfirmHeight, tcn.currentHeight)

	// Clear entries from confNotifications and confTxsByInitialHeight. We
	// assume that reorgs deeper than the reorg safety limit do not happen,
	// so we can clear out entries for the block that is now mature.
	if tcn.currentHeight >= tcn.reorgSafetyLimit {
		matureBlockHeight := tcn.currentHeight - tcn.reorgSafetyLimit
		for txHash := range tcn.txsByInitialHeight[matureBlockHeight] {
			delete(tcn.confNotifications, txHash)
		}
		delete(tcn.txsByInitialHeight, matureBlockHeight)
	}

	return nil
}

// DisconnectTip handles the tip of the current chain being disconnected during
// a chain reorganization. If any watched transactions were included in this
// block, internal structures are updated to ensure a confirmation notification
// is not sent unless the transaction is included in the new chain.
func (tcn *TxConfNotifier) DisconnectTip(blockHeight uint32) error {
	select {
	case <-tcn.quit:
		return ErrTxConfNotifierExiting
	default:
	}

	tcn.Lock()
	defer tcn.Unlock()

	if blockHeight != tcn.currentHeight {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, disconnected height=%d",
			tcn.currentHeight, blockHeight)
	}
	tcn.currentHeight--
	tcn.reorgDepth++

	// Rewind the height hint for all watched transactions.
	var txs []chainhash.Hash
	for tx := range tcn.confNotifications {
		txs = append(txs, tx)
	}

	err := tcn.hintCache.CommitConfirmHint(tcn.currentHeight, txs...)
	if err != nil {
		Log.Errorf("Unable to update confirm hint to %d for %v: %v",
			tcn.currentHeight, txs, err)
		return err
	}

	// We'll go through all of our watched transactions and attempt to drain
	// their notification channels to ensure sending notifications to the
	// clients is always non-blocking.
	for initialHeight, txHashes := range tcn.txsByInitialHeight {
		for txHash := range txHashes {
			// If the transaction has been reorged out of the chain,
			// we'll make sure to remove the cached confirmation
			// details to prevent notifying clients with old
			// information.
			confSet := tcn.confNotifications[txHash]
			if initialHeight == blockHeight {
				confSet.details = nil
			}

			for _, ntfn := range confSet.ntfns {
				// First, we'll attempt to drain an update
				// from each notification to ensure sends to the
				// Updates channel are always non-blocking.
				select {
				case <-ntfn.Event.Updates:
				case <-tcn.quit:
					return ErrTxConfNotifierExiting
				default:
				}

				// Then, we'll check if the current transaction
				// was included in the block currently being
				// disconnected. If it was, we'll need to
				// dispatch a reorg notification to the client.
				if initialHeight == blockHeight {
					err := tcn.dispatchConfReorg(
						ntfn, blockHeight,
					)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// Finally, we can remove the transactions we're currently watching that
	// were included in this block height.
	delete(tcn.txsByInitialHeight, blockHeight)

	return nil
}

// dispatchConfReorg dispatches a reorg notification to the client if the
// confirmation notification was already delivered.
//
// NOTE: This must be called with the TxNotifier's lock held.
func (tcn *TxConfNotifier) dispatchConfReorg(
	ntfn *ConfNtfn, heightDisconnected uint32) error {

	// If the transaction's confirmation notification has yet to be
	// dispatched, we'll need to clear its entry within the
	// ntfnsByConfirmHeight index to prevent from notifiying the client once
	// the notifier reaches the confirmation height.
	if !ntfn.dispatched {
		confHeight := heightDisconnected + ntfn.NumConfirmations - 1
		ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
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
	case <-tcn.quit:
		return ErrTxConfNotifierExiting
	default:
	}

	ntfn.dispatched = false

	// Send a negative confirmation notification to the client indicating
	// how many blocks have been disconnected successively.
	select {
	case ntfn.Event.NegativeConf <- int32(tcn.reorgDepth):
	case <-tcn.quit:
		return ErrTxConfNotifierExiting
	}

	return nil
}

// TearDown is to be called when the owner of the TxConfNotifier is exiting.
// This closes the event channels of all registered notifications that have
// not been dispatched yet.
func (tcn *TxConfNotifier) TearDown() {
	tcn.Lock()
	defer tcn.Unlock()

	close(tcn.quit)

	for _, confSet := range tcn.confNotifications {
		for _, ntfn := range confSet.ntfns {
			if ntfn.dispatched {
				continue
			}

			close(ntfn.Event.Confirmed)
			close(ntfn.Event.Updates)
			close(ntfn.Event.NegativeConf)
		}
	}
}
