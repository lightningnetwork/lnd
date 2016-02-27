package btcdnotify

import (
	"container/heap"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcrpcclient"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntfs"
)

// BtcdNotifier implements the ChainNotifier interface using btcd's websockets
// notifications. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type BtcdNotifier struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn *btcrpcclient.Client

	notificationRegistry chan interface{}

	// TODO(roasbeef): make map point to slices? Would allow for multiple
	// clients to listen for same spend. Would we ever need this?
	spendNotifications map[wire.OutPoint]*spendNotification
	confNotifications  map[wire.ShaHash]*confirmationsNotification
	confHeap           *confirmationHeap

	connectedBlockHashes    chan *blockNtfn
	disconnectedBlockHashes chan *blockNtfn
	relevantTxs             chan *btcutil.Tx

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure BtcdNotifier implements the ChainNotifier interface at compile time.
var _ chainntnfs.ChainNotifier = (*BtcdNotifier)(nil)

// NewBtcdNotifier returns a new BtcdNotifier instance. This function assumes
// the btcd node detailed in the passed configuration is already running, and
// willing to accept new websockets clients.
func NewBtcdNotifier(config *btcrpcclient.ConnConfig) (*BtcdNotifier, error) {
	notifier := &BtcdNotifier{
		notificationRegistry: make(chan interface{}),

		spendNotifications: make(map[wire.OutPoint]*spendNotification),
		confNotifications:  make(map[wire.ShaHash]*confirmationsNotification),
		confHeap:           newConfirmationHeap(),

		connectedBlockHashes:    make(chan *blockNtfn, 20),
		disconnectedBlockHashes: make(chan *blockNtfn, 20),
		relevantTxs:             make(chan *btcutil.Tx, 100),

		quit: make(chan struct{}),
	}

	ntfnCallbacks := &btcrpcclient.NotificationHandlers{
		OnBlockConnected:    notifier.onBlockConnected,
		OnBlockDisconnected: notifier.onBlockDisconnected,
		OnRedeemingTx:       notifier.onRedeemingTx,
	}

	// Disable connecting to btcd within the btcrpcclient.New method. We defer
	// establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := btcrpcclient.New(config, ntfnCallbacks)
	if err != nil {
		return nil, err
	}
	notifier.chainConn = chainConn

	return notifier, nil
}

// Start connects to the running btcd node over websockets, registers for block
// notifications, and finally launches all related helper goroutines.
func (b *BtcdNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	// Connect to btcd, and register for notifications on connected, and
	// disconnected blocks.
	if err := b.chainConn.Connect(20); err != nil {
		return err
	}
	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// Stop shutsdown the BtcdNotifier.
func (b *BtcdNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from btcd, and
	// cleans up all related resources.
	b.chainConn.Shutdown()

	close(b.quit)
	b.wg.Wait()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, spendClient := range b.spendNotifications {
		close(spendClient.spendChan)
	}
	for _, confClient := range b.confNotifications {
		close(confClient.finConf)
		close(confClient.negativeConf)
	}

	return nil
}

// blockNtfn packages a notification of a connected/disconnected block along
// with its height at the time.
type blockNtfn struct {
	sha    *wire.ShaHash
	height int32
}

// onBlockConnected implements on OnBlockConnected callback for btcrpcclient.
func (b *BtcdNotifier) onBlockConnected(hash *wire.ShaHash, height int32, t time.Time) {
	select {
	case b.connectedBlockHashes <- &blockNtfn{hash, height}:
	case <-b.quit:
	}
}

// onBlockDisconnected implements on OnBlockDisconnected callback for btcrpcclient.
func (b *BtcdNotifier) onBlockDisconnected(hash *wire.ShaHash, height int32, t time.Time) {
	b.onBlockDisconnected(hash, height, t)
}

// onRedeemingTx implements on OnRedeemingTx callback for btcrpcclient.
func (b *BtcdNotifier) onRedeemingTx(transaction *btcutil.Tx, details *btcjson.BlockDetails) {
	select {
	case b.relevantTxs <- transaction:
	case <-b.quit:
	}
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (b *BtcdNotifier) notificationDispatcher() {
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
		case staleBlockHash := <-b.disconnectedBlockHashes:
			// TODO(roasbeef): re-orgs
			//  * second channel to notify of confirmation decrementing
			//    re-org?
			//  * notify of negative confirmations
			fmt.Println(staleBlockHash)
		case connectedBlock := <-b.connectedBlockHashes:
			newBlock, err := b.chainConn.GetBlock(connectedBlock.sha)
			if err != nil {
				continue
			}

			newHeight := connectedBlock.height
			for _, tx := range newBlock.Transactions() {
				// Check if the inclusion of this transaction
				// within a block by itself triggers a block
				// confirmation threshold, if so send a
				// notification. Otherwise, place the notification
				// on a heap to be triggered in the future once
				// additional confirmations are attained.
				txSha := tx.Sha()
				b.checkConfirmationTrigger(txSha, newHeight)
			}

			// A new block has been connected to the main
			// chain. Send out any N confirmation notifications
			// which may have been triggered by this new block.
			b.notifyConfs(newHeight)
		case newSpend := <-b.relevantTxs:
			// First, check if this transaction spends an output
			// that has an existing spend notification for it.
			for i, txIn := range newSpend.MsgTx().TxIn {
				prevOut := txIn.PreviousOutPoint

				// If this transaction indeed does spend an
				// output which we have a registered notification
				// for, then create a spend summary, finally
				// sending off the details to the notification
				// subscriber.
				if ntfn, ok := b.spendNotifications[prevOut]; ok {
					spenderSha := newSpend.Sha()
					spendDetails := &chainntnfs.SpendDetail{
						SpentOutPoint: ntfn.targetOutpoint,
						SpenderTxHash: spenderSha,
						// TODO(roasbeef): copy tx?
						SpendingTx:        newSpend.MsgTx(),
						SpenderInputIndex: uint32(i),
					}

					ntfn.spendChan <- spendDetails
					delete(b.spendNotifications, prevOut)
				}
			}
		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// notifyConfs examines the current confirmation heap, sending off any
// notifications which have been triggered by the connection of a new block at
// newBlockHeight.
func (b *BtcdNotifier) notifyConfs(newBlockHeight int32) {
	// If the heap is empty, we have nothing to do.
	if b.confHeap.Len() == 0 {
		return
	}

	// Traverse our confirmation heap. The heap is a
	// min-heap, so the confirmation notification which requires
	// the smallest block-height will always be at the top
	// of the heap. If a confirmation notification is eligible
	// for triggering, then fire it off, and check if another
	// is eligible until there are no more eligible entries.
	nextConf := heap.Pop(b.confHeap).(*confEntry)
	for nextConf.triggerHeight <= uint32(newBlockHeight) {
		nextConf.finConf <- struct{}{}

		if b.confHeap.Len() == 0 {
			return
		}

		nextConf = heap.Pop(b.confHeap).(*confEntry)
	}

	heap.Push(b.confHeap, nextConf)
}

// checkConfirmationTrigger determines if the passed txSha included at blockHeight
// triggers any single confirmation notifications. In the event that the txid
// matches, yet needs additional confirmations, it is added to the confirmation
// heap to be triggered at a later time.
// TODO(roasbeef): perhaps lookup, then track by inputs instead?
func (b *BtcdNotifier) checkConfirmationTrigger(txSha *wire.ShaHash, blockHeight int32) {
	// If a confirmation notification has been registered
	// for this txid, then either trigger a notification
	// event if only a single confirmation notification was
	// requested, or place the notification on the
	// confirmation heap for future usage.
	if confNtfn, ok := b.confNotifications[*txSha]; ok {
		delete(b.confNotifications, *txSha)
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
		confNtfn.initialConfirmHeight = uint32(blockHeight)
		finalConfHeight := uint32(confNtfn.initialConfirmHeight + confNtfn.numConfirmations - 1)
		heapEntry := &confEntry{
			confNtfn,
			finalConfHeight,
		}
		heap.Push(b.confHeap, heapEntry)
	}
}

// spendNotification couples a target outpoint along with the channel used for
// notifications once a spend of the outpoint has been detected.
type spendNotification struct {
	targetOutpoint *wire.OutPoint

	spendChan chan *chainntnfs.SpendDetail
}

// RegisterSpendNotification registers an intent to be notified once the target
// outpoint has been spent by a transaction on-chain. Once a spend of the target
// outpoint has been detected, the details of the spending event will be sent
// across the 'Spend' channel.
func (b *BtcdNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint) (*chainntnfs.SpendEvent, error) {
	if err := b.chainConn.NotifySpent([]*wire.OutPoint{outpoint}); err != nil {
		return nil, err
	}

	ntfn := &spendNotification{
		targetOutpoint: outpoint,
		spendChan:      make(chan *chainntnfs.SpendDetail, 1),
	}

	b.notificationRegistry <- ntfn

	return &chainntnfs.SpendEvent{ntfn.spendChan}, nil
}

// confirmationNotification represents a client's intent to receive a
// notification once the target txid reaches numConfirmations confirmations.
type confirmationsNotification struct {
	txid *wire.ShaHash

	initialConfirmHeight uint32
	numConfirmations     uint32

	finConf      chan struct{}
	negativeConf chan int32 // TODO(roasbeef): re-org funny business
}

// RegisterConfirmationsNotification registers a notification with BtcdNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (b *BtcdNotifier) RegisterConfirmationsNtfn(txid *wire.ShaHash,
	numConfs uint32) (*chainntnfs.ConfirmationEvent, error) {

	ntfn := &confirmationsNotification{
		txid:             txid,
		numConfirmations: numConfs,
		finConf:          make(chan struct{}, 1),
		negativeConf:     make(chan int32, 1),
	}

	b.notificationRegistry <- ntfn

	return &chainntnfs.ConfirmationEvent{
		Confirmed:    ntfn.finConf,
		NegativeConf: ntfn.negativeConf,
	}, nil
}
