package bitcoindnotify

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/btcjson"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/chain"
)

const (

	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "bitcoind"

	// reorgSafetyLimit is assumed maximum depth of a chain reorganization.
	// After this many confirmation, transaction confirmation info will be
	// pruned.
	reorgSafetyLimit = 100
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

// txUpdate encapsulates a transaction related notification sent from bitcoind
// to the registered RPC client. This struct is used as an element within an
// unbounded queue in order to avoid blocking the main rpc dispatch rule.
type txUpdate struct {
	tx      *btcutil.Tx
	details *btcjson.BlockDetails
}

// TODO(roasbeef): generalize struct below:
//  * move chans to config, allow outside callers to handle send conditions

// BitcoindNotifier implements the ChainNotifier interface using a bitcoind
// chain client. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type BitcoindNotifier struct {
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	heightMtx  sync.RWMutex
	bestHeight int32

	chainConn *chain.BitcoindClient

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	spendNotifications map[wire.OutPoint]map[uint64]*spendNotification

	txConfNotifier *chainntnfs.TxConfNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure BitcoindNotifier implements the ChainNotifier interface at compile
// time.
var _ chainntnfs.ChainNotifier = (*BitcoindNotifier)(nil)

// New returns a new BitcoindNotifier instance. This function assumes the
// bitcoind node  detailed in the passed configuration is already running, and
// willing to accept RPC requests and new zmq clients.
func New(config *rpcclient.ConnConfig, zmqConnect string,
	params chaincfg.Params) (*BitcoindNotifier, error) {
	notifier := &BitcoindNotifier{
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendNotifications: make(map[wire.OutPoint]map[uint64]*spendNotification),

		quit: make(chan struct{}),
	}

	// Disable connecting to bitcoind within the rpcclient.New method. We
	// defer establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := chain.NewBitcoindClient(&params, config.Host,
		config.User, config.Pass, zmqConnect, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	notifier.chainConn = chainConn

	return notifier, nil
}

// Start connects to the running bitcoind node over websockets, registers for
// block notifications, and finally launches all related helper goroutines.
func (b *BitcoindNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	// Connect to bitcoind, and register for notifications on connected,
	// and disconnected blocks.
	if err := b.chainConn.Start(); err != nil {
		return err
	}
	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	_, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.heightMtx.Lock()
	b.bestHeight = currentHeight
	b.heightMtx.Unlock()

	b.txConfNotifier = chainntnfs.NewTxConfNotifier(
		uint32(currentHeight), reorgSafetyLimit)

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// Stop shutsdown the BitcoindNotifier.
func (b *BitcoindNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from bitcoind,
	// and cleans up all related resources.
	b.chainConn.Stop()

	close(b.quit)
	b.wg.Wait()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, spendClients := range b.spendNotifications {
		for _, spendClient := range spendClients {
			close(spendClient.spendChan)
		}
	}
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.epochChan)
	}
	b.txConfNotifier.TearDown()

	return nil
}

// blockNtfn packages a notification of a connected/disconnected block along
// with its height at the time.
type blockNtfn struct {
	sha    *chainhash.Hash
	height int32
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (b *BitcoindNotifier) notificationDispatcher() {
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
				b.chainConn.NotifySpent([]*wire.OutPoint{&op})
			case *confirmationsNotification:
				chainntnfs.Log.Infof("New confirmations "+
					"subscription: txid=%v, numconfs=%v",
					msg.TxID, msg.NumConfirmations)

				// Lookup whether the transaction is already included in the
				// active chain.
				txConf, err := b.historicalConfDetails(msg.TxID)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
				b.heightMtx.RLock()
				err = b.txConfNotifier.Register(&msg.ConfNtfn, txConf)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
				b.heightMtx.RUnlock()
			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")
				b.blockEpochClients[msg.epochID] = msg
			}

		case ntfn := <-b.chainConn.Notifications():
			switch item := ntfn.(type) {
			case chain.BlockConnected:
				b.heightMtx.Lock()
				if item.Height != b.bestHeight+1 {
					chainntnfs.Log.Warnf("Received blocks out of order: "+
						"current height=%d, new height=%d",
						b.bestHeight, item.Height)
					b.heightMtx.Unlock()
					continue
				}
				b.bestHeight = item.Height

				rawBlock, err := b.chainConn.GetBlock(&item.Hash)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to get block: %v", err)
					b.heightMtx.Unlock()
					continue
				}

				chainntnfs.Log.Infof("New block: height=%v, sha=%v",
					item.Height, item.Hash)

				b.notifyBlockEpochs(item.Height, &item.Hash)

				txns := btcutil.NewBlock(rawBlock).Transactions()
				err = b.txConfNotifier.ConnectTip(&item.Hash,
					uint32(item.Height), txns)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
				b.heightMtx.Unlock()
				continue

			case chain.BlockDisconnected:
				b.heightMtx.Lock()
				if item.Height != b.bestHeight {
					chainntnfs.Log.Warnf("Received blocks "+
						"out of order: current height="+
						"%d, disconnected height=%d",
						b.bestHeight, item.Height)
					b.heightMtx.Unlock()
					continue
				}
				b.bestHeight = item.Height - 1

				chainntnfs.Log.Infof("Block disconnected from "+
					"main chain: height=%v, sha=%v",
					item.Height, item.Hash)

				err := b.txConfNotifier.DisconnectTip(
					uint32(item.Height))
				if err != nil {
					chainntnfs.Log.Error(err)
				}
				b.heightMtx.Unlock()

			case chain.RelevantTx:
				tx := item.TxRecord.MsgTx
				// First, check if this transaction spends an output
				// that has an existing spend notification for it.
				for i, txIn := range tx.TxIn {
					prevOut := txIn.PreviousOutPoint

					// If this transaction indeed does spend an
					// output which we have a registered
					// notification for, then create a spend
					// summary, finally sending off the details to
					// the notification subscriber.
					if clients, ok := b.spendNotifications[prevOut]; ok {
						spenderSha := tx.TxHash()
						spendDetails := &chainntnfs.SpendDetail{
							SpentOutPoint:     &prevOut,
							SpenderTxHash:     &spenderSha,
							SpendingTx:        &tx,
							SpenderInputIndex: uint32(i),
						}
						// TODO(roasbeef): after change to
						// loadfilter, only notify on block
						// inclusion?
						if item.Block != nil {
							spendDetails.SpendingHeight = item.Block.Height
						} else {
							b.heightMtx.RLock()
							spendDetails.SpendingHeight = b.bestHeight + 1
							b.heightMtx.RUnlock()
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
			}

		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// historicalConfDetails looks up whether a transaction is already included in a
// block in the active chain and, if so, returns details about the confirmation.
func (b *BitcoindNotifier) historicalConfDetails(txid *chainhash.Hash,
) (*chainntnfs.TxConfirmation, error) {

	// If the transaction already has some or all of the confirmations,
	// then we may be able to dispatch it immediately.
	// TODO: fall back to scanning blocks if txindex isn't on.
	tx, err := b.chainConn.GetRawTransactionVerbose(txid)
	if err != nil || tx == nil || tx.BlockHash == "" {
		if err == nil {
			return nil, nil
		}
		// Do not return an error if the transaction was not found.
		if jsonErr, ok := err.(*btcjson.RPCError); ok {
			if jsonErr.Code == btcjson.ErrRPCNoTxInfo {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("unable to query for txid(%v): %v", txid, err)
	}

	// As we need to fully populate the returned TxConfirmation struct,
	// grab the block in which the transaction was confirmed so we can
	// locate its exact index within the block.
	blockHash, err := chainhash.NewHashFromStr(tx.BlockHash)
	if err != nil {
		return nil, fmt.Errorf("unable to get block hash %v for historical "+
			"dispatch: %v", tx.BlockHash, err)
	}
	block, err := b.chainConn.GetBlockVerbose(blockHash)
	if err != nil {
		return nil, fmt.Errorf("unable to get block hash: %v", err)
	}

	// If the block obtained, locate the transaction's index within the
	// block so we can give the subscriber full confirmation details.
	txIndex := -1
	targetTxidStr := txid.String()
	for i, txHash := range block.Tx {
		if txHash == targetTxidStr {
			txIndex = i
			break
		}
	}

	if txIndex == -1 {
		return nil, fmt.Errorf("unable to locate tx %v in block %v",
			txid, blockHash)
	}

	txConf := chainntnfs.TxConfirmation{
		BlockHash:   blockHash,
		BlockHeight: uint32(block.Height),
		TxIndex:     uint32(txIndex),
	}
	return &txConf, nil
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (b *BitcoindNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
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

// spendNotification couples a target outpoint along with the channel used for
// notifications once a spend of the outpoint has been detected.
type spendNotification struct {
	targetOutpoint *wire.OutPoint

	spendChan chan *chainntnfs.SpendDetail

	spendID uint64
}

// spendCancel is a message sent to the BitcoindNotifier when a client wishes
// to cancel an outstanding spend notification that has yet to be dispatched.
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
func (b *BitcoindNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
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
		// TODO: fall back to scanning blocks if txindex isn't on.
		transaction, err := b.chainConn.GetRawTransactionVerbose(&outpoint.Hash)
		if err != nil {
			jsonErr, ok := err.(*btcjson.RPCError)
			if !ok || jsonErr.Code != btcjson.ErrRPCNoTxInfo {
				return nil, err
			}
		}

		if transaction != nil {
			blockhash, err := chainhash.NewHashFromStr(transaction.BlockHash)
			if err != nil {
				return nil, err
			}

			// Rewind the rescan, since the btcwallet bitcoind
			// back-end doesn't support that.
			blockHeight, err := b.chainConn.GetBlockHeight(blockhash)
			if err != nil {
				return nil, err
			}
			b.heightMtx.Lock()
			currentHeight := b.bestHeight
			b.bestHeight = blockHeight
			for i := currentHeight; i > blockHeight; i-- {
				err = b.txConfNotifier.DisconnectTip(uint32(i))
				if err != nil {
					return nil, err
				}
			}
			b.heightMtx.Unlock()

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
	chainntnfs.ConfNtfn
}

// RegisterConfirmationsNtfn registers a notification with BitcoindNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (b *BitcoindNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs, _ uint32) (*chainntnfs.ConfirmationEvent, error) {

	ntfn := &confirmationsNotification{
		chainntnfs.ConfNtfn{
			TxID:             txid,
			NumConfirmations: numConfs,
			Event:            chainntnfs.NewConfirmationEvent(),
		},
	}

	select {
	case <-b.quit:
		return nil, ErrChainNotifierShuttingDown
	case b.notificationRegistry <- ntfn:
		return ntfn.Event, nil
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

// epochCancel is a message sent to the BitcoindNotifier when a client wishes
// to cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications, of each new block connected to the main
// chain.
func (b *BitcoindNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
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
