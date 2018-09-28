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
	PkScript []byte

	// NumConfirmations is the number of confirmations after which the
	// notification is to be sent.
	NumConfirmations uint32

	// Event contains references to the channels that the notifications are to
	// be sent over.
	Event *ConfirmationEvent

	// HeightHint is the minimum height in the chain that we expect to find
	// this txid. This value will be overridden by the height hint cache if
	// a more recent value is available.
	HeightHint uint32

	// details describes the transaction's position is the blockchain. May be
	// nil for unconfirmed transactions.
	details *TxConfirmation

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
//
// NOTE: If the transaction has already been included in a block on the chain,
// the confirmation details must be provided with the UpdateConfDetails method,
// otherwise we will wait for the transaction to confirm even though it already
// has.
func (tcn *TxConfNotifier) Register(ntfn *ConfNtfn) (bool, uint32, error) {
	select {
	case <-tcn.quit:
		return false, 0, ErrTxConfNotifierExiting
	default:
	}

	// Before proceeding to register the notification, we'll query our
	// height hint cache to determine whether a better one exists.
	hint, err := tcn.hintCache.QueryConfirmHint(*ntfn.TxID)
	if err == nil {
		if hint > ntfn.HeightHint {
			Log.Debugf("Using height hint %d retrieved "+
				"from cache for %v", hint, *ntfn.TxID)
			ntfn.HeightHint = hint
		}
	}

	tcn.Lock()
	defer tcn.Unlock()

	// TODO(conner): promote immediately to confNotifications if a
	// historical dispatch has already completed.

	confSet, ok := tcn.confNotifications[*ntfn.TxID]
	if !ok {
		confSet = newConfNtfnSet()
		tcn.confNotifications[*ntfn.TxID] = confSet
	}

	confSet.ntfns[ntfn.ConfID] = ntfn

	switch confSet.rescanStatus {

	// A prior rescan has already completed and we are actively watching at
	// tip for this txid.
	case rescanComplete:
		return nil, nil

	// A rescan is already in progress, return here to prevent dispatching
	// another. When the scan returns, this notifications details will be
	// updated as well.
	case rescanPending:
		return nil, nil

	// If no rescan has been dispatched, attempt to do so now.
	case rescanNotStarted:
	}

	// If the provided or cached height hint indicates that the transaction
	// is to be confirmed at a height greater than the conf notifier's
	// current height, we'll refrain from spawning a historical dispatch.
	if startHeight > tcn.currentHeight {
		// Set the rescan status to complete, which will allow the conf
		// notifier to start delivering messages for this set
		// immediately.
		confSet.rescanStatus = rescanComplete
		return nil, nil
	}

	// Set this confSet's status to pending, ensuring subsequent
	// registrations don't also attempt a dispatch.
	confSet.rescanStatus = rescanPending
}

// UpdateConfDetails attempts to update the confirmation details for an active
// notification within the notifier. This should only be used in the case of a
// transaction that has confirmed before the notifier's current height.
//
// NOTE: The notification should be registered first to ensure notifications are
// dispatched correctly.
func (tcn *TxConfNotifier) UpdateConfDetails(txid chainhash.Hash,
	clientID uint64, details *TxConfirmation) error {

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

	// The historical dispatch has been completed for this confSet. We'll
	// update the rescan status and cache any details that were found. If
	// the details are nil, that implies we did not find them and will
	// continue to watch for them at tip.
	confSet.rescanStatus = rescanComplete

	// The notifier has yet to reach the height at which the transaction was
	// included in a block, so we should defer until handling it then within
	// ConnectTip.
	if details == nil || details.BlockHeight > tcn.currentHeight {
		return nil
	}

	err := tcn.hintCache.CommitConfirmHint(details.BlockHeight, txid)
	if err != nil {
		// The error is not fatal, so we should not return an error to
		// the caller.
		Log.Errorf("Unable to update confirm hint to %d for %v: %v",
			details.BlockHeight, txid, err)
	}

	// Update the conf details of all ntfns that don't yet have them.
	for _, ntfn := range confSet.ntfns {
		if ntfn.details != nil {
			continue
		}

		ntfn.details = details

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
			txSet[txid] = struct{}{}
		}
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
		confSet, ok := tcn.confNotifications[*txHash]
		if !ok {
			continue
		}

		for _, ntfn := range confSet.ntfns {
			ntfn.details = &TxConfirmation{
				BlockHash:   blockHash,
				BlockHeight: blockHeight,
				TxIndex:     uint32(tx.Index()),
			}

			confHeight := blockHeight + ntfn.NumConfirmations - 1
			ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
			if !exists {
				ntfnSet = make(map[*ConfNtfn]struct{})
				tcn.ntfnsByConfirmHeight[confHeight] = ntfnSet
			}
			ntfnSet[ntfn] = struct{}{}

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
	// this map doesn't tell us whether the transaction hsa confirmed or
	// not, we'll need to look at txsByInitialHeight to determine so.
	var txsToUpdateHints []chainhash.Hash
	for confirmedTx := range tcn.txsByInitialHeight[tcn.currentHeight] {
		txsToUpdateHints = append(txsToUpdateHints, confirmedTx)
	}
out:
	for maybeUnconfirmedTx, confSet := range tcn.confNotifications {
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
				// If the notification hasn't learned about the
				// confirmation of its transaction yet (in the
				// case of historical confirmations), we'll skip
				// it.
				if ntfn.details == nil {
					continue
				}

				txConfHeight := ntfn.details.BlockHeight +
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
	for ntfn := range tcn.ntfnsByConfirmHeight[tcn.currentHeight] {
		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)

		select {
		case ntfn.Event.Confirmed <- ntfn.details:
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
			confSet := tcn.confNotifications[txHash]
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
				// disconnected. If it was, we'll need to take
				// some necessary precautions.
				if initialHeight == blockHeight {
					// If the transaction's confirmation notification
					// has already been dispatched, we'll attempt to
					// notify the client it was reorged out of the chain.
					if ntfn.dispatched {
						// Attempt to drain the confirmation notification
						// to ensure sends to the Confirmed channel are
						// always non-blocking.
						select {
						case <-ntfn.Event.Confirmed:
						case <-tcn.quit:
							return ErrTxConfNotifierExiting
						default:
						}

						ntfn.dispatched = false

						// Send a negative confirmation notification to the
						// client indicating how many blocks have been
						// disconnected successively.
						select {
						case ntfn.Event.NegativeConf <- int32(tcn.reorgDepth):
						case <-tcn.quit:
							return ErrTxConfNotifierExiting
						}

						continue
					}

					// Otherwise, since the transactions was reorged out
					// of the chain, we can safely remove its accompanying
					// confirmation notification.
					confHeight := blockHeight + ntfn.NumConfirmations - 1
					ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
					if !exists {
						continue
					}
					delete(ntfnSet, ntfn)

					// Intuitively, we should also remove
					// the txHash from confNotifications if
					// the ntfnSet is now empty. However, we
					// will not do so since we may want to
					// continue rewinding the height hints
					// for this txid.
					//
					// NOTE(conner): safe to delete if
					// blockHeight is below client-provided
					// height hint?
				}
			}
		}
	}

	// Finally, we can remove the transactions we're currently watching that
	// were included in this block height.
	delete(tcn.txsByInitialHeight, blockHeight)

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
