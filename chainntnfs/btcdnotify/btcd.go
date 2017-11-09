package btcdnotify

import (
	"container/heap"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/btcjson"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

const (

	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "btcd"
)

var (
	// ErrChainNotifierShuttingDown is used when we are trying to
	// measure a spend notification when notifier is already stopped.
	ErrChainNotifierShuttingDown = errors.New("chainntnfs: system interrupt " +
		"while attempting to register for spend notification.")
)

// chainUpdate encapsulates an update to the current main chain. This struct is
// used as an element within an unbounded queue in order to avoid blocking the
// main rpc dispatch rule.
type chainUpdate struct {
	blockHash   *chainhash.Hash
	blockHeight int32
}

// txUpdate encapsulates a transaction related notification sent from btcd to
// the registered RPC client. This struct is used as an element within an
// unbounded queue in order to avoid blocking the main rpc dispatch rule.
type txUpdate struct {
	tx      *btcutil.Tx
	details *btcjson.BlockDetails
}

// TODO(roasbeef): generalize struct below:
//  * move chans to config, allow outside callers to handle send conditions

// BtcdNotifier implements the ChainNotifier interface using btcd's websockets
// notifications. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type BtcdNotifier struct {
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn *rpcclient.Client

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	spendNotifications map[wire.OutPoint]map[uint64]*spendNotification

	confNotifications map[chainhash.Hash][]*confirmationsNotification
	confHeap          *confirmationHeap

	blockEpochClients map[uint64]*blockEpochRegistration

	disconnectedBlockHashes chan *blockNtfn

	chainUpdates *chainntnfs.ConcurrentQueue
	txUpdates    *chainntnfs.ConcurrentQueue

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure BtcdNotifier implements the ChainNotifier interface at compile time.
var _ chainntnfs.ChainNotifier = (*BtcdNotifier)(nil)

// New returns a new BtcdNotifier instance. This function assumes the btcd node
// detailed in the passed configuration is already running, and willing to
// accept new websockets clients.
func New(config *rpcclient.ConnConfig) (*BtcdNotifier, error) {
	notifier := &BtcdNotifier{
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendNotifications: make(map[wire.OutPoint]map[uint64]*spendNotification),

		confNotifications: make(map[chainhash.Hash][]*confirmationsNotification),
		confHeap:          newConfirmationHeap(),

		disconnectedBlockHashes: make(chan *blockNtfn, 20),

		chainUpdates: chainntnfs.NewConcurrentQueue(10),
		txUpdates:    chainntnfs.NewConcurrentQueue(10),

		quit: make(chan struct{}),
	}

	ntfnCallbacks := &rpcclient.NotificationHandlers{
		OnBlockConnected:    notifier.onBlockConnected,
		OnBlockDisconnected: notifier.onBlockDisconnected,
		OnRedeemingTx:       notifier.onRedeemingTx,
	}

	// Disable connecting to btcd within the rpcclient.New method. We
	// defer establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(config, ntfnCallbacks)
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

	_, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.chainUpdates.Start()
	b.txUpdates.Start()

	b.wg.Add(1)
	go b.notificationDispatcher(currentHeight)

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

	b.chainUpdates.Stop()
	b.txUpdates.Stop()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, spendClients := range b.spendNotifications {
		for _, spendClient := range spendClients {
			close(spendClient.spendChan)
		}
	}
	for _, confClients := range b.confNotifications {
		for _, confClient := range confClients {
			close(confClient.finConf)
			close(confClient.negativeConf)
		}
	}
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.epochChan)
	}

	return nil
}

// blockNtfn packages a notification of a connected/disconnected block along
// with its height at the time.
type blockNtfn struct {
	sha    *chainhash.Hash
	height int32
}

// onBlockConnected implements on OnBlockConnected callback for rpcclient.
// Ingesting a block updates the wallet's internal utxo state based on the
// outputs created and destroyed within each block.
func (b *BtcdNotifier) onBlockConnected(hash *chainhash.Hash, height int32, t time.Time) {
	// Append this new chain update to the end of the queue of new chain
	// updates.
	b.chainUpdates.ChanIn() <- &chainUpdate{hash, height}
}

// onBlockDisconnected implements on OnBlockDisconnected callback for rpcclient.
func (b *BtcdNotifier) onBlockDisconnected(hash *chainhash.Hash, height int32, t time.Time) {
}

// onRedeemingTx implements on OnRedeemingTx callback for rpcclient.
func (b *BtcdNotifier) onRedeemingTx(tx *btcutil.Tx, details *btcjson.BlockDetails) {
	// Append this new transaction update to the end of the queue of new
	// chain updates.
	b.txUpdates.ChanIn() <- &txUpdate{tx, details}
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (b *BtcdNotifier) notificationDispatcher(currentHeight int32) {
out:
	for {
		select {
		case cancelMsg := <-b.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *spendCancel:
				chainntnfs.Log.Infof("Cancelling spend "+
					"notification for out_point=%v, "+
					"spend_id=%v", msg.op, msg.spendID)

				// Before we attempt to close the spendChan,
				// ensure that the notification hasn't already
				// yet been dispatched.
				if outPointClients, ok := b.spendNotifications[msg.op]; ok {
					close(outPointClients[msg.spendID].spendChan)
					delete(b.spendNotifications[msg.op], msg.spendID)
				}

			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				// First, close the cancel channel for this
				// specific client, and wait for the client to
				// exit.
				close(b.blockEpochClients[msg.epochID].cancelChan)
				b.blockEpochClients[msg.epochID].wg.Wait()

				// Once the client has exited, we can then
				// safely close the channel used to send epoch
				// notifications, in order to notify any
				// listeners that the intent has been
				// cancelled.
				close(b.blockEpochClients[msg.epochID].epochChan)
				delete(b.blockEpochClients, msg.epochID)

			}
		case registerMsg := <-b.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *spendNotification:
				chainntnfs.Log.Infof("New spend subscription: "+
					"utxo=%v", msg.targetOutpoint)
				op := *msg.targetOutpoint

				if _, ok := b.spendNotifications[op]; !ok {
					b.spendNotifications[op] = make(map[uint64]*spendNotification)
				}
				b.spendNotifications[op][msg.spendID] = msg
			case *confirmationsNotification:
				chainntnfs.Log.Infof("New confirmations "+
					"subscription: txid=%v, numconfs=%v",
					*msg.txid, msg.numConfirmations)

				// If the notification can be partially or
				// fully dispatched, then we can skip the first
				// phase for ntfns.
				if b.attemptHistoricalDispatch(msg) {
					continue
				}

				txid := *msg.txid
				b.confNotifications[txid] = append(b.confNotifications[txid], msg)
			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")
				b.blockEpochClients[msg.epochID] = msg
			}

		case staleBlockHash := <-b.disconnectedBlockHashes:
			// TODO(roasbeef): re-orgs
			//  * second channel to notify of confirmation decrementing
			//    re-org?
			//  * notify of negative confirmations
			chainntnfs.Log.Warnf("Block disconnected from main "+
				"chain: %v", staleBlockHash)

		case item := <-b.chainUpdates.ChanOut():
			update := item.(*chainUpdate)
			currentHeight = update.blockHeight

			newBlock, err := b.chainConn.GetBlock(update.blockHash)
			if err != nil {
				chainntnfs.Log.Errorf("Unable to get block: %v", err)
				continue
			}

			chainntnfs.Log.Infof("New block: height=%v, sha=%v",
				update.blockHeight, update.blockHash)

			b.notifyBlockEpochs(update.blockHeight,
				update.blockHash)

			newHeight := update.blockHeight
			for i, tx := range newBlock.Transactions {
				// Check if the inclusion of this transaction
				// within a block by itself triggers a block
				// confirmation threshold, if so send a
				// notification. Otherwise, place the
				// notification on a heap to be triggered in
				// the future once additional confirmations are
				// attained.
				txSha := tx.TxHash()
				b.checkConfirmationTrigger(&txSha, update, i)
			}

			// A new block has been connected to the main
			// chain. Send out any N confirmation notifications
			// which may have been triggered by this new block.
			b.notifyConfs(newHeight)

		case item := <-b.txUpdates.ChanOut():
			newSpend := item.(*txUpdate)
			spendingTx := newSpend.tx

			// First, check if this transaction spends an output
			// that has an existing spend notification for it.
			for i, txIn := range spendingTx.MsgTx().TxIn {
				prevOut := txIn.PreviousOutPoint

				// If this transaction indeed does spend an
				// output which we have a registered
				// notification for, then create a spend
				// summary, finally sending off the details to
				// the notification subscriber.
				if clients, ok := b.spendNotifications[prevOut]; ok {
					spenderSha := newSpend.tx.Hash()
					spendDetails := &chainntnfs.SpendDetail{
						SpentOutPoint:     &prevOut,
						SpenderTxHash:     spenderSha,
						SpendingTx:        spendingTx.MsgTx(),
						SpenderInputIndex: uint32(i),
					}
					// TODO(roasbeef): after change to
					// loadfilter, only notify on block
					// inclusion?
					if newSpend.details != nil {
						spendDetails.SpendingHeight = newSpend.details.Height
					} else {
						spendDetails.SpendingHeight = currentHeight + 1
					}

					for _, ntfn := range clients {
						chainntnfs.Log.Infof("Dispatching "+
							"spend notification for "+
							"outpoint=%v", ntfn.targetOutpoint)
						ntfn.spendChan <- spendDetails

						// Close spendChan to ensure that any calls to Cancel will not
						// block. This is safe to do since the channel is buffered, and the
						// message can still be read by the receiver.
						close(ntfn.spendChan)
					}
					delete(b.spendNotifications, prevOut)
				}
			}

		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// attemptHistoricalDispatch tries to use historical information to decide if a
// notification ca be dispatched immediately, or is partially confirmed so it
// can skip straight to the confirmations heap.
//
// Returns true if the transaction was either partially or completely confirmed
func (b *BtcdNotifier) attemptHistoricalDispatch(
	msg *confirmationsNotification) bool {

	chainntnfs.Log.Infof("Attempting to trigger dispatch for %v from "+
		"historical chain", msg.txid)

	// If the transaction already has some or all of the confirmations,
	// then we may be able to dispatch it immediately.
	tx, err := b.chainConn.GetRawTransactionVerbose(msg.txid)
	if err != nil || tx == nil || tx.BlockHash == "" {
		jsonErr, ok := err.(*btcjson.RPCError)
		switch {
		case ok && jsonErr.Code == -5:
		default:
			chainntnfs.Log.Warnf("unable to query for txid(%v): %v",
				msg.txid, err)
		}
		return false
	}

	// As we need to fully populate the returned TxConfirmation struct,
	// grab the block in which the transaction was confirmed so we can
	// locate its exact index within the block.
	blockHash, err := chainhash.NewHashFromStr(tx.BlockHash)
	if err != nil {
		chainntnfs.Log.Errorf("unable to get block hash %v for "+
			"historical dispatch: %v", tx.BlockHash, err)
		return false
	}
	block, err := b.chainConn.GetBlockVerbose(blockHash)
	if err != nil {
		chainntnfs.Log.Errorf("unable to get block hash: %v", err)
		return false
	}

	// If the block obtained, locate the transaction's index within the
	// block so we can give the subscriber full confirmation details.
	var txIndex uint32
	targetTxidStr := msg.txid.String()
	for i, txHash := range block.Tx {
		if txHash == targetTxidStr {
			txIndex = uint32(i)
			break
		}
	}

	confDetails := &chainntnfs.TxConfirmation{
		BlockHash:   blockHash,
		BlockHeight: uint32(block.Height),
		TxIndex:     txIndex,
	}

	// If the transaction has more that enough confirmations, then we can
	// dispatch it immediately after obtaining for information w.r.t
	// exactly *when* if got all its confirmations.
	if uint32(tx.Confirmations) >= msg.numConfirmations {
		msg.finConf <- confDetails
		return true
	}

	// Otherwise, the transaction has only been *partially* confirmed, so
	// we need to insert it into the confirmation heap.
	// Find the block height at which this transaction will be confirmed
	confHeight := uint32(block.Height) + msg.numConfirmations - 1
	heapEntry := &confEntry{
		msg,
		confDetails,
		confHeight,
	}
	heap.Push(b.confHeap, heapEntry)

	return true
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (b *BtcdNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	epoch := &chainntnfs.BlockEpoch{
		Height: newHeight,
		Hash:   newSha,
	}

	for _, epochClient := range b.blockEpochClients {
		b.wg.Add(1)
		epochClient.wg.Add(1)
		go func(ntfnChan chan *chainntnfs.BlockEpoch, cancelChan chan struct{},
			clientWg *sync.WaitGroup) {

			// TODO(roasbeef): move to goroutine per client, use sync queue

			defer clientWg.Done()
			defer b.wg.Done()

			select {
			case ntfnChan <- epoch:

			case <-cancelChan:
				return

			case <-b.quit:
				return
			}

		}(epochClient.epochChan, epochClient.cancelChan, &epochClient.wg)
	}
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
		// TODO(roasbeef): shake out possible of by one in height calc
		// for historical dispatches
		nextConf.finConf <- nextConf.initialConfDetails

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
func (b *BtcdNotifier) checkConfirmationTrigger(txSha *chainhash.Hash,
	newTip *chainUpdate, txIndex int) {

	// If a confirmation notification has been registered
	// for this txid, then either trigger a notification
	// event if only a single confirmation notification was
	// requested, or place the notification on the
	// confirmation heap for future usage.
	if confClients, ok := b.confNotifications[*txSha]; ok {
		// Either all of the registered confirmations will be
		// dispatched due to a single confirmation, or added to the
		// conf head. Therefore we unconditionally delete the registered
		// confirmations from the staging zone.
		defer func() {
			delete(b.confNotifications, *txSha)
		}()

		for _, confClient := range confClients {
			confDetails := &chainntnfs.TxConfirmation{
				BlockHash:   newTip.blockHash,
				BlockHeight: uint32(newTip.blockHeight),
				TxIndex:     uint32(txIndex),
			}

			if confClient.numConfirmations == 1 {
				chainntnfs.Log.Infof("Dispatching single conf "+
					"notification, sha=%v, height=%v", txSha,
					newTip.blockHeight)
				confClient.finConf <- confDetails
				continue
			}

			// The registered notification requires more
			// than one confirmation before triggering. So
			// we create a heapConf entry for this notification.
			// The heapConf allows us to easily keep track of
			// which notification(s) we should fire off with
			// each incoming block.
			confClient.initialConfirmHeight = uint32(newTip.blockHeight)
			finalConfHeight := confClient.initialConfirmHeight + confClient.numConfirmations - 1
			heapEntry := &confEntry{
				confClient,
				confDetails,
				finalConfHeight,
			}
			heap.Push(b.confHeap, heapEntry)
		}
	}
}

// spendNotification couples a target outpoint along with the channel used for
// notifications once a spend of the outpoint has been detected.
type spendNotification struct {
	targetOutpoint *wire.OutPoint

	spendChan chan *chainntnfs.SpendDetail

	spendID uint64
}

// spendCancel is a message sent to the BtcdNotifier when a client wishes to
// cancel an outstanding spend notification that has yet to be dispatched.
type spendCancel struct {
	// op is the target outpoint of the notification to be cancelled.
	op wire.OutPoint

	// spendID the ID of the notification to cancel.
	spendID uint64
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint has been spent by a transaction on-chain. Once a spend of the target
// outpoint has been detected, the details of the spending event will be sent
// across the 'Spend' channel.
func (b *BtcdNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ uint32) (*chainntnfs.SpendEvent, error) {

	if err := b.chainConn.NotifySpent([]*wire.OutPoint{outpoint}); err != nil {
		return nil, err
	}

	ntfn := &spendNotification{
		targetOutpoint: outpoint,
		spendChan:      make(chan *chainntnfs.SpendDetail, 1),
		spendID:        atomic.AddUint64(&b.spendClientCounter, 1),
	}

	select {
	case <-b.quit:
		return nil, ErrChainNotifierShuttingDown
	case b.notificationRegistry <- ntfn:
	}

	// The following conditional checks to ensure that when a spend notification
	// is registered, the output hasn't already been spent. If the output
	// is no longer in the UTXO set, the chain will be rescanned from the point
	// where the output was added. The rescan will dispatch the notification.
	txout, err := b.chainConn.GetTxOut(&outpoint.Hash, outpoint.Index, true)
	if err != nil {
		return nil, err
	}

	if txout == nil {
		transaction, err := b.chainConn.GetRawTransactionVerbose(&outpoint.Hash)
		if err != nil {
			jsonErr, ok := err.(*btcjson.RPCError)
			switch {
			case ok && jsonErr.Code == -5:
			default:
				return nil, err
			}
		}

		if transaction != nil {
			blockhash, err := chainhash.NewHashFromStr(transaction.BlockHash)
			if err != nil {
				return nil, err
			}

			ops := []*wire.OutPoint{outpoint}
			if err := b.chainConn.Rescan(blockhash, nil, ops); err != nil {
				chainntnfs.Log.Errorf("Rescan for spend "+
					"notification txout failed: %v", err)
				return nil, err
			}
		}
	}

	return &chainntnfs.SpendEvent{
		Spend: ntfn.spendChan,
		Cancel: func() {
			cancel := &spendCancel{
				op:      *outpoint,
				spendID: ntfn.spendID,
			}

			// Submit spend cancellation to notification dispatcher.
			select {
			case b.notificationCancels <- cancel:
				// Cancellation is being handled, drain the spend chan until it is
				// closed before yielding to the caller.
				for {
					select {
					case _, ok := <-ntfn.spendChan:
						if !ok {
							return
						}
					case <-b.quit:
						return
					}
				}
			case <-b.quit:
			}
		},
	}, nil
}

// confirmationNotification represents a client's intent to receive a
// notification once the target txid reaches numConfirmations confirmations.
type confirmationsNotification struct {
	txid *chainhash.Hash

	initialConfirmHeight uint32
	numConfirmations     uint32

	finConf      chan *chainntnfs.TxConfirmation
	negativeConf chan int32 // TODO(roasbeef): re-org funny business
}

// RegisterConfirmationsNtfn registers a notification with BtcdNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (b *BtcdNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs, _ uint32) (*chainntnfs.ConfirmationEvent, error) {

	ntfn := &confirmationsNotification{
		txid:             txid,
		numConfirmations: numConfs,
		finConf:          make(chan *chainntnfs.TxConfirmation, 1),
		negativeConf:     make(chan int32, 1),
	}

	select {
	case <-b.quit:
		return nil, ErrChainNotifierShuttingDown
	case b.notificationRegistry <- ntfn:
		return &chainntnfs.ConfirmationEvent{
			Confirmed:    ntfn.finConf,
			NegativeConf: ntfn.negativeConf,
		}, nil
	}
}

// blockEpochRegistration represents a client's intent to receive a
// notification with each newly connected block.
type blockEpochRegistration struct {
	epochID uint64

	epochChan chan *chainntnfs.BlockEpoch

	cancelChan chan struct{}

	wg sync.WaitGroup
}

// epochCancel is a message sent to the BtcdNotifier when a client wishes to
// cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications, of each new block connected to the main
// chain.
func (b *BtcdNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	registration := &blockEpochRegistration{
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&b.epochClientCounter, 1),
	}

	select {
	case <-b.quit:
		return nil, errors.New("chainntnfs: system interrupt while " +
			"attempting to register for block epoch notification.")
	case b.notificationRegistry <- registration:
		return &chainntnfs.BlockEpochEvent{
			Epochs: registration.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: registration.epochID,
				}

				// Submit epoch cancellation to notification dispatcher.
				select {
				case b.notificationCancels <- cancel:
					// Cancellation is being handled, drain the epoch channel until it is
					// closed before yielding to caller.
					for {
						select {
						case _, ok := <-registration.epochChan:
							if !ok {
								return
							}
						case <-b.quit:
							return
						}
					}
				case <-b.quit:
				}
			},
		}, nil
	}
}
