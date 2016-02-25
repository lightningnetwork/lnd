package btcdnotify

import (
	"bytes"
	"container/heap"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/chainntfs"
)

// BtcdNotifier...
type BtcdNotifier struct {
	started int32 // To be used atomically
	stopped int32 // To be used atomically

	ntfnSource *btcwallet.NotificationServer
	chainConn  *chain.RPCClient

	notificationRegistry chan interface{}

	// TODO(roasbeef): make map point to slices? Would allow for multiple
	// clients to listen for same spend. Would we ever need this?
	spendNotifications map[wire.OutPoint]*spendNotification
	confNotifications  map[wire.ShaHash]*confirmationsNotification
	confHeap           *confirmationHeap

	connectedBlocks    <-chan wtxmgr.BlockMeta
	disconnectedBlocks <-chan wtxmgr.BlockMeta
	relevantTxs        <-chan chain.RelevantTx

	wg   sync.WaitGroup
	quit chan struct{}
}

var _ chainntnfs.ChainNotifier = (*BtcdNotifier)(nil)

// NewBtcdNotifier...
// TODO(roasbeef): chain client + notification sever
//  * use server for notifications
//  * when asked for spent, request via client
func NewBtcdNotifier(ntfnSource *btcwallet.NotificationServer,
	chainConn *chain.RPCClient) (*BtcdNotifier, error) {

	return &BtcdNotifier{
		ntfnSource: ntfnSource,
		chainConn:  chainConn,

		notificationRegistry: make(chan interface{}),

		spendNotifications: make(map[wire.OutPoint]*spendNotification),
		confNotifications:  make(map[wire.ShaHash]*confirmationsNotification),
		confHeap:           newConfirmationHeap(),

		connectedBlocks:    make(chan wtxmgr.BlockMeta),
		disconnectedBlocks: make(chan wtxmgr.BlockMeta),
		relevantTxs:        make(chan chain.RelevantTx),

		quit: make(chan struct{}),
	}, nil
}

// Start...
func (b *BtcdNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// Stop...
func (b *BtcdNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	close(b.quit)
	b.wg.Wait()

	return nil
}

// notificationDispatcher...
func (b *BtcdNotifier) notificationDispatcher() {
	ntfnClient := b.ntfnSource.TransactionNotifications()

out:
	for {
		select {
		case registerMsg := <-b.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *spendNotification:
				b.spendNotifications[*msg.targetOutpoint] = msg
			case *confirmationsNotification:
				b.confNotifications[*msg.txid] = msg
			}
		case txNtfn := <-ntfnClient.C:
			// We're only concerned with newly mined blocks which
			// may or may not include transactions we are interested
			// in.
			if txNtfn.AttachedBlocks == nil {
				break
			}

			newBlocks := txNtfn.AttachedBlocks
			for _, block := range newBlocks {
				blockHeight := uint32(block.Height)

				// Examine all transactions within the block
				// in order to determine if this block includes a
				// transactions spending one of the registered
				// outpoints of interest.
				for _, txSummary := range block.Transactions {
					txBytes := bytes.NewReader(txSummary.Transaction)
					tx := wire.NewMsgTx()
					if err := tx.Deserialize(txBytes); err != nil {
						// TODO(roasbeef): err
						fmt.Println("unable to des tx: ", err)
						continue
					}

					// Check if the inclusion of this transaction
					// within a block by itself triggers a block
					// confirmation threshold, if so send a
					// notification. Otherwise, place the notification
					// on a heap to be triggered in the future once
					// additional confirmations are attained.
					txSha := tx.TxSha()
					b.checkConfirmationTrigger(&txSha, blockHeight)

					// Next, examine all the inputs spent, firing
					// of a notification if it spends any of the
					// outpoints within the set of our registered
					// outputs.
					b.checkSpendTrigger(tx)
				}

				// A new block has been connected to the main
				// chain. Send out any N confirmation notifications
				// which may have been triggered by this new block.
				b.notifyConfs(blockHeight)
			}

			// TODO(roasbeef): re-orgs
			//  * second channel to notify of confirmation decrementing
			//    re-org?
			//  * notify of negative confirmations
			fmt.Println(txNtfn.DetachedBlocks)
		case <-b.quit:
			break out
		}
	}
}

// notifyConfs...
func (b *BtcdNotifier) notifyConfs(newBlockHeight uint32) {
	// Traverse our confirmation heap. The heap is a
	// min-heap, so the confirmation notification which requires
	// the smallest block-height will always be at the top
	// of the heap. If a confirmation notification is eligible
	// for triggering, then fire it off, and check if another
	// is eligible until there are no more eligible entries.
	nextConf := heap.Pop(b.confHeap).(*confEntry)
	for nextConf.triggerHeight <= newBlockHeight {
		nextConf.finConf <- struct{}{}

		nextConf = heap.Pop(b.confHeap).(*confEntry)
	}

	heap.Push(b.confHeap, nextConf)
}

// checkSpendTrigger...
func (b *BtcdNotifier) checkSpendTrigger(tx *wire.MsgTx) {
	// First, check if this transaction spends an output
	// that has an existing spend notification for it.
	for i, txIn := range tx.TxIn {
		prevOut := txIn.PreviousOutPoint

		// If this transaction indeed does spend an
		// output which we have a registered notification
		// for, then create a spend summary, finally
		// sending off the details to the notification
		// subscriber.
		if ntfn, ok := b.spendNotifications[prevOut]; ok {
			spenderSha := tx.TxSha()
			spendDetails := &chainntnfs.SpendDetail{
				SpentOutPoint: ntfn.targetOutpoint,
				SpenderTxHash: &spenderSha,
				// TODO(roasbeef): copy tx?
				SpendingTx:        tx,
				SpenderInputIndex: uint32(i),
			}

			ntfn.spendChan <- spendDetails
			delete(b.spendNotifications, prevOut)
		}
	}
}

// checkConfirmationTrigger...
func (b *BtcdNotifier) checkConfirmationTrigger(txSha *wire.ShaHash,
	blockHeight uint32) {
	// If a confirmation notification has been registered
	// for this txid, then either trigger a notification
	// event if only a single confirmation notification was
	// requested, or place the notification on the
	// confirmation heap for future usage.
	if confNtfn, ok := b.confNotifications[*txSha]; ok {
		if confNtfn.numConfirmations == 1 {
			confNtfn.finConf <- struct{}{}
			return
		}

		// The registered notification requires more
		// than one confirmation before triggering. So
		// we create a heapConf entry for this notification.
		// The heapConf allows us to easily keep track of
		// which notification(s) we should fire off with
		// each incoming block.
		confNtfn.initialConfirmHeight = blockHeight
		heapEntry := &confEntry{
			confNtfn,
			confNtfn.initialConfirmHeight + confNtfn.numConfirmations,
		}
		heap.Push(b.confHeap, heapEntry)
	}
}

// spendNotification....
type spendNotification struct {
	targetOutpoint *wire.OutPoint

	spendChan chan *chainntnfs.SpendDetail
}

// RegisterSpendNotification...
// NOTE: eventChan MUST be buffered
func (b *BtcdNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint) (*chainntnfs.SpendEvent, error) {

	// TODO(roasbeef): also register with rpc client? bool?

	ntfn := &spendNotification{
		targetOutpoint: outpoint,
		spendChan:      make(chan *chainntnfs.SpendDetail, 1),
	}

	b.notificationRegistry <- ntfn

	return &chainntnfs.SpendEvent{ntfn.spendChan}, nil
}

// confirmationNotification...
// TODO(roasbeef): re-org funny business
type confirmationsNotification struct {
	txid *wire.ShaHash

	initialConfirmHeight uint32
	numConfirmations     uint32

	finConf      chan struct{}
	negativeConf chan uint32
}

// RegisterConfirmationsNotification...
func (b *BtcdNotifier) RegisterConfirmationsNtfn(txid *wire.ShaHash,
	numConfs uint32) (*chainntnfs.ConfirmationEvent, error) {

	ntfn := &confirmationsNotification{
		txid:             txid,
		numConfirmations: numConfs,
		finConf:          make(chan struct{}, 1),
		negativeConf:     make(chan uint32, 1),
	}

	b.notificationRegistry <- ntfn

	return &chainntnfs.ConfirmationEvent{
		Confirmed:    ntfn.finConf,
		NegativeConf: ntfn.negativeConf,
	}, nil
}
