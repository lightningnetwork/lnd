package chainntnfs

import (
	"fmt"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

// ConfNtfn represents a notifier client's request to receive a notification
// once the target transaction gets sufficient confirmations. The client is
// asynchronously notified via the ConfirmationEvent channels.
type ConfNtfn struct {
	// TxID is the hash of the transaction for which confirmatino notifications
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
func NewConfirmationEvent() *ConfirmationEvent {
	return &ConfirmationEvent{
		Confirmed:    make(chan *TxConfirmation, 1),
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

	// confNotifications is an index of notification requests by transaction
	// hash.
	confNotifications map[chainhash.Hash][]*ConfNtfn

	// confTxsByInitialHeight is an index of watched transactions by the height
	// that they are included at in the blockchain. This is tracked so that
	// incorrect notifications are not sent if a transaction is reorganized out
	// of the chain and so that negative confirmations can be recognized.
	confTxsByInitialHeight map[uint32][]*chainhash.Hash

	// ntfnsByConfirmHeight is an index of notification requests by the height
	// at which the transaction will have sufficient confirmations.
	ntfnsByConfirmHeight map[uint32]map[*ConfNtfn]struct{}
}

// NewTxConfNotifier creates a TxConfNotifier. The current height of the
// blockchain is accepted as a parameter.
func NewTxConfNotifier(startHeight uint32, reorgSafetyLimit uint32) *TxConfNotifier {
	return &TxConfNotifier{
		currentHeight:          startHeight,
		reorgSafetyLimit:       reorgSafetyLimit,
		confNotifications:      make(map[chainhash.Hash][]*ConfNtfn),
		confTxsByInitialHeight: make(map[uint32][]*chainhash.Hash),
		ntfnsByConfirmHeight:   make(map[uint32]map[*ConfNtfn]struct{}),
	}
}

// Register handles a new notification request. The client will be notified when
// the transaction gets a sufficient number of confirmations on the blockchain.
// If the transaction has already been included in a block on the chain, the
// confirmation details must be given as the txConf argument, otherwise it
// should be nil. If the transaction already has the sufficient number of
// confirmations, this dispatches the notification immediately.
func (tcn *TxConfNotifier) Register(ntfn *ConfNtfn, txConf *TxConfirmation) {
	if txConf == nil || txConf.BlockHeight > tcn.currentHeight {
		// Transaction is unconfirmed.
		tcn.confNotifications[*ntfn.TxID] =
			append(tcn.confNotifications[*ntfn.TxID], ntfn)
		return
	}

	// If the transaction already has the required confirmations, dispatch
	// notification immediately, otherwise record along with the height at
	// which to notify.
	confHeight := txConf.BlockHeight + ntfn.NumConfirmations - 1
	if confHeight <= tcn.currentHeight {
		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)
		ntfn.Event.Confirmed <- txConf
		ntfn.dispatched = true
	} else {
		ntfn.details = txConf
		ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
		if !exists {
			ntfnSet = make(map[*ConfNtfn]struct{})
			tcn.ntfnsByConfirmHeight[confHeight] = ntfnSet
		}
		ntfnSet[ntfn] = struct{}{}
	}

	// Unless the transaction is finalized, include transaction information in
	// confNotifications and confTxsByInitialHeight in case the tx gets
	// reorganized out of the chain.
	if txConf.BlockHeight > tcn.currentHeight-tcn.reorgSafetyLimit {
		tcn.confNotifications[*ntfn.TxID] =
			append(tcn.confNotifications[*ntfn.TxID], ntfn)
		tcn.confTxsByInitialHeight[txConf.BlockHeight] =
			append(tcn.confTxsByInitialHeight[txConf.BlockHeight], ntfn.TxID)
	}
}

// ConnectTip handles a new block extending the current chain. This checks each
// transaction in the block to see if any watched transactions are included.
// Also, if any watched transactions now have the required number of
// confirmations as a result of this block being connected, this dispatches
// notifications.
func (tcn *TxConfNotifier) ConnectTip(blockHash *chainhash.Hash,
	blockHeight uint32, txns []*btcutil.Tx) error {

	if blockHeight != tcn.currentHeight+1 {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, new height=%d",
			tcn.currentHeight, blockHeight)
	}
	tcn.currentHeight++

	// Record any newly confirmed transactions in ntfnsByConfirmHeight so that
	// notifications get dispatched when the tx gets sufficient confirmations.
	// Also record txs in confTxsByInitialHeight so reorgs can be handled
	// correctly.
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

			tcn.confTxsByInitialHeight[blockHeight] =
				append(tcn.confTxsByInitialHeight[blockHeight], tx.Hash())
		}
	}

	// Dispatch notifications for all transactions that are considered confirmed
	// at this new block height.
	for ntfn := range tcn.ntfnsByConfirmHeight[tcn.currentHeight] {
		Log.Infof("Dispatching %v conf notification for %v",
			ntfn.NumConfirmations, ntfn.TxID)
		ntfn.Event.Confirmed <- ntfn.details
		ntfn.dispatched = true
	}
	delete(tcn.ntfnsByConfirmHeight, tcn.currentHeight)

	// Clear entries from confNotifications and confTxsByInitialHeight. We
	// assume that reorgs deeper than the reorg safety limit do not happen, so
	// we can clear out entries for the block that is now mature.
	matureBlockHeight := tcn.currentHeight - tcn.reorgSafetyLimit
	for _, txHash := range tcn.confTxsByInitialHeight[matureBlockHeight] {
		delete(tcn.confNotifications, *txHash)
	}
	delete(tcn.confTxsByInitialHeight, matureBlockHeight)

	return nil
}

// DisconnectTip handles the tip of the current chain being disconnected during
// a chain reorganization. If any watched transactions were included in this
// block, internal structures are updated to ensure a confirmation notification
// is not sent unless the transaction is included in the new chain.
func (tcn *TxConfNotifier) DisconnectTip(blockHeight uint32) error {
	if blockHeight != tcn.currentHeight {
		return fmt.Errorf("Received blocks out of order: "+
			"current height=%d, disconnected height=%d",
			tcn.currentHeight, blockHeight)
	}
	tcn.currentHeight--

	for _, txHash := range tcn.confTxsByInitialHeight[blockHeight] {
		for _, ntfn := range tcn.confNotifications[*txHash] {
			confHeight := blockHeight + ntfn.NumConfirmations - 1
			ntfnSet, exists := tcn.ntfnsByConfirmHeight[confHeight]
			if !exists {
				continue
			}
			delete(ntfnSet, ntfn)
		}
	}
	delete(tcn.confTxsByInitialHeight, blockHeight)

	return nil
}

// TearDown is to be called when the owner of the TxConfNotifier is exiting.
// This closes the event channels of all registered notifications that have
// not been dispatched yet.
func (tcn *TxConfNotifier) TearDown() {
	for _, ntfns := range tcn.confNotifications {
		for _, ntfn := range ntfns {
			if ntfn.dispatched {
				continue
			}

			close(ntfn.Event.Confirmed)
			close(ntfn.Event.NegativeConf)
		}
	}
}
