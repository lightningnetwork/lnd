package chainntnfs

import (
	"errors"
	"fmt"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

// ConfNtfn represents a notifier client's request to receive a notification
// once the target transaction gets sufficient confirmations. The client is
// asynchronously notified via the ConfirmationEvent channels.
type ConfNtfn struct {
	// TxID is the hash of the transaction for which confirmation notifications
	// are requested.
	TxID *chainhash.Hash

	// NumConfirmations is the number of confirmations after which the
	// notification is to be sent.
	NumConfirmations uint32

	// Event contains references to the channels that the notifications are to
	// be sent over.
	Event *ConfirmationEvent

	// details describes the transaction's position is the blockchain. May be
	// nil for unconfirmed transactions.
	details *TxConfirmation

	// dispatched is false if the confirmed notification has not been sent yet.
	dispatched bool
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
	confNotifications map[chainhash.Hash][]*ConfNtfn

	// txsByInitialHeight is an index of watched transactions by the height
	// that they are included at in the blockchain. This is tracked so that
	// incorrect notifications are not sent if a transaction is reorganized
	// out of the chain and so that negative confirmations can be recognized.
	txsByInitialHeight map[uint32]map[chainhash.Hash]struct{}

	// ntfnsByConfirmHeight is an index of notification requests by the height
	// at which the transaction will have sufficient confirmations.
	ntfnsByConfirmHeight map[uint32]map[*ConfNtfn]struct{}

	// quit is closed in order to signal that the notifier is gracefully
	// exiting.
	quit chan struct{}
}

// NewTxConfNotifier creates a TxConfNotifier. The current height of the
// blockchain is accepted as a parameter.
func NewTxConfNotifier(startHeight uint32, reorgSafetyLimit uint32) *TxConfNotifier {
	return &TxConfNotifier{
		currentHeight:        startHeight,
		reorgSafetyLimit:     reorgSafetyLimit,
		confNotifications:    make(map[chainhash.Hash][]*ConfNtfn),
		txsByInitialHeight:   make(map[uint32]map[chainhash.Hash]struct{}),
		ntfnsByConfirmHeight: make(map[uint32]map[*ConfNtfn]struct{}),
		quit:                 make(chan struct{}),
	}
}

// Register handles a new notification request. The client will be notified when
// the transaction gets a sufficient number of confirmations on the blockchain.
// If the transaction has already been included in a block on the chain, the
// confirmation details must be given as the txConf argument, otherwise it
// should be nil. If the transaction already has the sufficient number of
// confirmations, this dispatches the notification immediately.
func (tcn *TxConfNotifier) Register(ntfn *ConfNtfn, txConf *TxConfirmation) error {
	select {
	case <-tcn.quit:
		return fmt.Errorf("TxConfNotifier is exiting")
	default:
	}

	if txConf == nil || txConf.BlockHeight > tcn.currentHeight {
		// Transaction is unconfirmed.
		tcn.confNotifications[*ntfn.TxID] =
			append(tcn.confNotifications[*ntfn.TxID], ntfn)
		return nil
	}

	// If the transaction already has the required confirmations, we'll
	// dispatch the notification immediately.
	confHeight := txConf.BlockHeight + ntfn.NumConfirmations - 1
	if confHeight <= tcn.currentHeight {
		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)

		// We'll send a 0 value to the Updates channel, indicating that
		// the transaction has already been confirmed.
		select {
		case <-tcn.quit:
			return fmt.Errorf("TxConfNotifier is exiting")
		case ntfn.Event.Updates <- 0:
		}

		select {
		case <-tcn.quit:
			return fmt.Errorf("TxConfNotifier is exiting")
		case ntfn.Event.Confirmed <- txConf:
			ntfn.dispatched = true
		}
	} else {
		// Otherwise, we'll record the transaction along with the height
		// at which we should notify the client.
		ntfn.details = txConf
		ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
		if !exists {
			ntfnSet = make(map[*ConfNtfn]struct{})
			tcn.ntfnsByConfirmHeight[confHeight] = ntfnSet
		}
		ntfnSet[ntfn] = struct{}{}

		// We'll also send an update to the client of how many
		// confirmations are left for the transaction to be confirmed.
		numConfsLeft := confHeight - tcn.currentHeight
		select {
		case ntfn.Event.Updates <- numConfsLeft:
		case <-tcn.quit:
			return errors.New("TxConfNotifier is exiting")
		}
	}

	// As a final check, we'll also watch the transaction if it's still
	// possible for it to get reorganized out of the chain.
	if txConf.BlockHeight+tcn.reorgSafetyLimit > tcn.currentHeight {
		tcn.confNotifications[*ntfn.TxID] =
			append(tcn.confNotifications[*ntfn.TxID], ntfn)

		txSet, exists := tcn.txsByInitialHeight[txConf.BlockHeight]
		if !exists {
			txSet = make(map[chainhash.Hash]struct{})
			tcn.txsByInitialHeight[txConf.BlockHeight] = txSet
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
		return fmt.Errorf("TxConfNotifier is exiting")
	default:
	}

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
		for _, ntfn := range tcn.confNotifications[*txHash] {
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

	// Next, we'll dispatch an update to all of the notification clients for
	// our watched transactions with the number of confirmations left at
	// this new height.
	for _, txHashes := range tcn.txsByInitialHeight {
		for txHash := range txHashes {
			for _, ntfn := range tcn.confNotifications[txHash] {
				// If the transaction still hasn't been included
				// in a block, we'll skip it.
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
					return errors.New("TxConfNotifier is exiting")
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
			return fmt.Errorf("TxConfNotifier is exiting")
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
		return fmt.Errorf("TxConfNotifier is exiting")
	default:
	}

	if blockHeight != tcn.currentHeight {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, disconnected height=%d",
			tcn.currentHeight, blockHeight)
	}
	tcn.currentHeight--
	tcn.reorgDepth++

	// We'll go through all of our watched transactions and attempt to drain
	// their notification channels to ensure sending notifications to the
	// clients is always non-blocking.
	for initialHeight, txHashes := range tcn.txsByInitialHeight {
		for txHash := range txHashes {
			for _, ntfn := range tcn.confNotifications[txHash] {
				// First, we'll attempt to drain an update
				// from each notification to ensure sends to the
				// Updates channel are always non-blocking.
				select {
				case <-ntfn.Event.Updates:
				case <-tcn.quit:
					return errors.New("TxConfNotifier is exiting")
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
							return errors.New("TxConfNotifier is exiting")
						default:
						}

						ntfn.dispatched = false

						// Send a negative confirmation notification to the
						// client indicating how many blocks have been
						// disconnected successively.
						select {
						case ntfn.Event.NegativeConf <- int32(tcn.reorgDepth):
						case <-tcn.quit:
							return errors.New("TxConfNotifier is exiting")
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
	close(tcn.quit)

	for _, ntfns := range tcn.confNotifications {
		for _, ntfn := range ntfns {
			if ntfn.dispatched {
				continue
			}

			close(ntfn.Event.Confirmed)
			close(ntfn.Event.Updates)
			close(ntfn.Event.NegativeConf)
		}
	}
}
