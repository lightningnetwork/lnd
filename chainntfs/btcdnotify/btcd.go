package btcdnotify

import (
	"container/heap"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"li.lan/labs/plasma/chainntfs"
)

// BtcdNotifier...
type BtcdNotifier struct {
	// TODO(roasbeef): refactor to use the new NotificationServer
	conn ChainConnection

	notificationRegistry chan interface{}

	spendNotifications map[wire.OutPoint]*spendNotification
	confNotifications  map[wire.ShaHash]*confirmationsNotification
	confHeap           *confirmationHeap

	connectedBlocks    <-chan wtxmgr.BlockMeta
	disconnectedBlocks <-chan wtxmgr.BlockMeta
	relevantTxs        <-chan chain.RelevantTx

	rpcConnected chan struct{}

	wg      sync.WaitGroup
	started int32 // To be used atomically
	stopped int32 // To be used atomically
	quit    chan struct{}
}

var _ chainntnfs.ChainNotifier = (*BtcdNotifier)(nil)

// NewBtcdNotifier...
func NewBtcdNotifier(c ChainConnection) (*BtcdNotifier, error) {
	return &BtcdNotifier{
		conn:                 c,
		notificationRegistry: make(chan interface{}),

		spendNotifications: make(map[wire.OutPoint]*spendNotification),
		confNotifications:  make(map[wire.ShaHash]*confirmationsNotification),
		confHeap:           newConfirmationHeap(),

		connectedBlocks:    make(chan wtxmgr.BlockMeta),
		disconnectedBlocks: make(chan wtxmgr.BlockMeta),
		relevantTxs:        make(chan chain.RelevantTx),

		rpcConnected: make(chan struct{}, 1),

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
	b.rpcConnected <- struct{}{} // TODO(roasbeef) why?

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
out:
	for {
		select {
		case <-b.rpcConnected:
			err := b.initAllNotifications()
			fmt.Println(err)
		case registerMsg := <-b.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *spendNotification:
				b.spendNotifications[*msg.outpoint] = msg
			case *confirmationsNotification:
				b.confNotifications[*msg.txid] = msg
			}
		case txNtfn := <-b.relevantTxs:
			tx := txNtfn.TxRecord.MsgTx
			txMined := txNtfn.Block != nil

			// First, check if this transaction spends an output
			// that has an existing spend notification for it.
			for _, txIn := range tx.TxIn {
				prevOut := txIn.PreviousOutPoint

				if ntfn, ok := b.spendNotifications[prevOut]; ok {
					go triggerNtfn(ntfn.trigger)

					delete(b.spendNotifications, prevOut)
				}
			}

			// If the transaction has been mined, then we check if
			// a notification for the confirmation of this txid has
			// been registered previously. Otherwise, we're done,
			// for now.
			if !txMined {
				break
			}

			// If a confirmation notification has been registered
			// for this txid, then either trigger a notification
			// event if only a single confirmation notification was
			// requested, or place the notification on the
			// confirmation heap for future usage.
			if confNtfn, ok := b.confNotifications[tx.TxSha()]; ok {
				if confNtfn.numConfirmations == 1 {
					go triggerNtfn(confNtfn.trigger)
					break
				}

				// The registered notification requires more
				// than one confirmation before triggering. So
				// we create a heapConf entry for this notification.
				// The heapConf allows us to easily keep track of
				// which notification(s) we should fire off with
				// each incoming block.
				confNtfn.initialConfirmHeight = uint32(txNtfn.Block.Height)
				heapEntry := &confEntry{
					confNtfn,
					confNtfn.initialConfirmHeight + confNtfn.numConfirmations,
				}
				heap.Push(b.confHeap, heapEntry)
			}
		case blockNtfn := <-b.connectedBlocks:
			blockHeight := uint32(blockNtfn.Height)

			// Traverse our confirmation heap. The heap is a
			// min-heap, so the confirmation notification which requires
			// the smallest block-height will always be at the top
			// of the heap. If a confirmation notification is eligible
			// for triggering, then fire it off, and check if another
			// is eligible until there are no more eligible entries.
			nextConf := heap.Pop(b.confHeap).(*confEntry)
			for nextConf.triggerHeight <= blockHeight {
				triggerNtfn(nextConf.trigger)

				nextConf = heap.Pop(b.confHeap).(*confEntry)
			}

			heap.Push(b.confHeap, nextConf)
		case delBlockNtfn := <-b.disconnectedBlocks:
			// TODO(roasbeef): re-orgs
			//  * second channel to notify of confirmation decrementing
			//    re-org?
			fmt.Println(delBlockNtfn)
		case <-b.quit:
			break out
		}
	}
}

// initAllNotifications...
func (b *BtcdNotifier) initAllNotifications() error {
	var err error

	b.connectedBlocks, err = b.conn.ListenConnectedBlocks()
	if err != nil {
		return err
	}
	b.disconnectedBlocks, err = b.conn.ListenDisconnectedBlocks()
	if err != nil {
		return err
	}
	b.relevantTxs, err = b.conn.ListenRelevantTxs()
	if err != nil {
		return err
	}

	return nil
}

// spendNotification....
type spendNotification struct {
	outpoint *wire.OutPoint

	trigger *chainntnfs.NotificationTrigger
}

// confirmationNotification...
// TODO(roasbeef): re-org funny business
type confirmationsNotification struct {
	txid *wire.ShaHash

	initialConfirmHeight uint32
	numConfirmations     uint32

	trigger *chainntnfs.NotificationTrigger
}

// RegisterSpendNotification...
// NOTE: eventChan MUST be buffered
func (b *BtcdNotifier) RegisterSpendNotification(outpoint *wire.OutPoint,
	trigger *chainntnfs.NotificationTrigger) error {

	// TODO(roasbeef): also register with rpc client? bool?

	ntfn := &spendNotification{
		outpoint: outpoint,
		trigger:  trigger,
	}

	b.notificationRegistry <- ntfn

	return nil
}

// RegisterConfirmationsNotification...
func (b *BtcdNotifier) RegisterConfirmationsNotification(txid *wire.ShaHash,
	numConfs uint32, trigger *chainntnfs.NotificationTrigger) error {

	ntfn := &confirmationsNotification{
		txid:             txid,
		numConfirmations: numConfs,
		trigger:          trigger,
	}

	b.notificationRegistry <- ntfn

	return nil
}

func triggerNtfn(t *chainntnfs.NotificationTrigger) {
	if t.Callback != nil {
		go t.Callback()
	}

	t.TriggerChan <- struct{}{}
}
