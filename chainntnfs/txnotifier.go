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

// ConnectTip handles a new block extending the current chain. This checks each
// transaction in the block to see if any watched transactions are included.
// Also, if any watched transactions now have the required number of
// confirmations as a result of this block being connected, this dispatches
// notifications.
func (n *TxNotifier) ConnectTip(blockHash *chainhash.Hash,
	blockHeight uint32, txns []*btcutil.Tx) error {

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

	// Record any newly confirmed transactions by their confirmed height so
	// that notifications get dispatched when the transactions reach their
	// required number of confirmations. We'll also watch these transactions
	// at the height they were included in the chain so reorgs can be
	// handled correctly.
	for _, tx := range txns {
		txHash := tx.Hash()

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

	// In order to update the height hint for all the required transactions
	// under one database transaction, we'll gather the set of unconfirmed
	// transactions along with the ones that confirmed at the current
	// height. To do so, we'll iterate over the confNotifications map, which
	// contains the transactions we currently have notifications for. Since
	// this map doesn't tell us whether the transaction has confirmed or
	// not, we'll need to look at txsByInitialHeight to determine so.
	var txsToUpdateHints []chainhash.Hash
	for confirmedTx := range n.txsByInitialHeight[n.currentHeight] {
		txsToUpdateHints = append(txsToUpdateHints, confirmedTx)
	}
out:
	for maybeUnconfirmedTx, confSet := range n.confNotifications {
		// We shouldn't update the confirm hints if we still have a
		// pending rescan in progress. We'll skip writing any for
		// notification sets that haven't reached rescanComplete.
		if confSet.rescanStatus != rescanComplete {
			continue
		}

		for height, confirmedTxs := range n.txsByInitialHeight {
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
		err := n.confirmHintCache.CommitConfirmHint(
			n.currentHeight, txsToUpdateHints...,
		)
		if err != nil {
			// The error is not fatal, so we should not return an
			// error to the caller.
			Log.Errorf("Unable to update confirm hint to %d for "+
				"%v: %v", n.currentHeight, txsToUpdateHints,
				err)
		}
	}

	// Next, we'll dispatch an update to all of the notification clients for
	// our watched transactions with the number of confirmations left at
	// this new height.
	for _, txHashes := range n.txsByInitialHeight {
		for txHash := range txHashes {
			confSet := n.confNotifications[txHash]
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
				case <-n.quit:
					return ErrTxNotifierExiting
				}
			}
		}
	}

	// Then, we'll dispatch notifications for all the transactions that have
	// become confirmed at this new block height.
	for ntfn := range n.ntfnsByConfirmHeight[blockHeight] {
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
	delete(n.ntfnsByConfirmHeight, n.currentHeight)

	// Clear entries from confNotifications and confTxsByInitialHeight. We
	// assume that reorgs deeper than the reorg safety limit do not happen,
	// so we can clear out entries for the block that is now mature.
	if n.currentHeight >= n.reorgSafetyLimit {
		matureBlockHeight := n.currentHeight - n.reorgSafetyLimit
		for txHash := range n.txsByInitialHeight[matureBlockHeight] {
			delete(n.confNotifications, txHash)
		}
		delete(n.txsByInitialHeight, matureBlockHeight)
	}

	return nil
}

// DisconnectTip handles the tip of the current chain being disconnected during
// a chain reorganization. If any watched transactions were included in this
// block, internal structures are updated to ensure a confirmation notification
// is not sent unless the transaction is included in the new chain.
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

	// Rewind the height hint for all watched transactions.
	var txs []chainhash.Hash
	for tx := range n.confNotifications {
		txs = append(txs, tx)
	}

	err := n.confirmHintCache.CommitConfirmHint(n.currentHeight, txs...)
	if err != nil {
		Log.Errorf("Unable to update confirm hint to %d for %v: %v",
			n.currentHeight, txs, err)
		return err
	}

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

	// Finally, we can remove the transactions we're currently watching that
	// were included in this block height.
	delete(n.txsByInitialHeight, blockHeight)

	return nil
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

// TearDown is to be called when the owner of the TxNotifier is exiting. This
// closes the event channels of all registered notifications that have not been
// dispatched yet.
func (n *TxNotifier) TearDown() {
	n.Lock()
	defer n.Unlock()

	close(n.quit)

	for _, confSet := range n.confNotifications {
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
