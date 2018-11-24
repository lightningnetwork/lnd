package btcdnotify

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "btcd"

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

	// connected is true if this update is a new block and false if it is a
	// disconnected block.
	connect bool
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
	confClientCounter  uint64 // To be used aotmically.
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn *rpcclient.Client

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	txNotifier *chainntnfs.TxNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	bestBlock chainntnfs.BlockEpoch

	chainUpdates *queue.ConcurrentQueue
	txUpdates    *queue.ConcurrentQueue

	// spendHintCache is a cache used to query and update the latest height
	// hints for an outpoint. Each height hint represents the earliest
	// height at which the outpoint could have been spent within the chain.
	spendHintCache chainntnfs.SpendHintCache

	// confirmHintCache is a cache used to query the latest height hints for
	// a transaction. Each height hint represents the earliest height at
	// which the transaction could have confirmed within the chain.
	confirmHintCache chainntnfs.ConfirmHintCache

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure BtcdNotifier implements the ChainNotifier interface at compile time.
var _ chainntnfs.ChainNotifier = (*BtcdNotifier)(nil)

// New returns a new BtcdNotifier instance. This function assumes the btcd node
// detailed in the passed configuration is already running, and willing to
// accept new websockets clients.
func New(config *rpcclient.ConnConfig, spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache) (*BtcdNotifier, error) {

	notifier := &BtcdNotifier{
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		chainUpdates: queue.NewConcurrentQueue(10),
		txUpdates:    queue.NewConcurrentQueue(10),

		spendHintCache:   spendHintCache,
		confirmHintCache: confirmHintCache,

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

	currentHash, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.txNotifier = chainntnfs.NewTxNotifier(
		uint32(currentHeight), reorgSafetyLimit, b.confirmHintCache,
		b.spendHintCache,
	)

	b.bestBlock = chainntnfs.BlockEpoch{
		Height: currentHeight,
		Hash:   currentHash,
	}

	b.chainUpdates.Start()
	b.txUpdates.Start()

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

	b.chainUpdates.Stop()
	b.txUpdates.Stop()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()

		close(epochClient.epochChan)
	}
	b.txNotifier.TearDown()

	return nil
}

// onBlockConnected implements on OnBlockConnected callback for rpcclient.
// Ingesting a block updates the wallet's internal utxo state based on the
// outputs created and destroyed within each block.
func (b *BtcdNotifier) onBlockConnected(hash *chainhash.Hash, height int32, t time.Time) {
	// Append this new chain update to the end of the queue of new chain
	// updates.
	b.chainUpdates.ChanIn() <- &chainUpdate{
		blockHash:   hash,
		blockHeight: height,
		connect:     true,
	}
}

// filteredBlock represents a new block which has been connected to the main
// chain. The slice of transactions will only be populated if the block
// includes a transaction that confirmed one of our watched txids, or spends
// one of the outputs currently being watched.
// TODO(halseth): this is currently used for complete blocks. Change to use
// onFilteredBlockConnected and onFilteredBlockDisconnected, making it easier
// to unify with the Neutrino implementation.
type filteredBlock struct {
	hash   chainhash.Hash
	height uint32
	txns   []*btcutil.Tx

	// connected is true if this update is a new block and false if it is a
	// disconnected block.
	connect bool
}

// onBlockDisconnected implements on OnBlockDisconnected callback for rpcclient.
func (b *BtcdNotifier) onBlockDisconnected(hash *chainhash.Hash, height int32, t time.Time) {
	// Append this new chain update to the end of the queue of new chain
	// updates.
	b.chainUpdates.ChanIn() <- &chainUpdate{
		blockHash:   hash,
		blockHeight: height,
		connect:     false,
	}
}

// onRedeemingTx implements on OnRedeemingTx callback for rpcclient.
func (b *BtcdNotifier) onRedeemingTx(tx *btcutil.Tx, details *btcjson.BlockDetails) {
	// Append this new transaction update to the end of the queue of new
	// chain updates.
	b.txUpdates.ChanIn() <- &txUpdate{tx, details}
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (b *BtcdNotifier) notificationDispatcher() {
out:
	for {
		select {
		case cancelMsg := <-b.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				// First, we'll lookup the original
				// registration in order to stop the active
				// queue goroutine.
				reg := b.blockEpochClients[msg.epochID]
				reg.epochQueue.Stop()

				// Next, close the cancel channel for this
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
			case *chainntnfs.HistoricalConfDispatch:
				// Look up whether the transaction is already
				// included in the active chain. We'll do this
				// in a goroutine to prevent blocking
				// potentially long rescans.
				//
				// TODO(wilmer): add retry logic if rescan fails?
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()

					confDetails, _, err := b.historicalConfDetails(
						msg.TxID, msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Error(err)
						return
					}

					// If the historical dispatch finished
					// without error, we will invoke
					// UpdateConfDetails even if none were
					// found. This allows the notifier to
					// begin safely updating the height hint
					// cache at tip, since any pending
					// rescans have now completed.
					err = b.txNotifier.UpdateConfDetails(
						*msg.TxID, confDetails,
					)
					if err != nil {
						chainntnfs.Log.Error(err)
					}
				}()

			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")
				b.blockEpochClients[msg.epochID] = msg
				if msg.bestBlock != nil {
					missedBlocks, err :=
						chainntnfs.GetClientMissedBlocks(
							b.chainConn, msg.bestBlock,
							b.bestBlock.Height, true,
						)
					if err != nil {
						msg.errorChan <- err
						continue
					}
					for _, block := range missedBlocks {
						b.notifyBlockEpochClient(msg,
							block.Height, block.Hash)
					}

				}
				msg.errorChan <- nil
			}

		case item := <-b.chainUpdates.ChanOut():
			update := item.(*chainUpdate)
			if update.connect {
				blockHeader, err :=
					b.chainConn.GetBlockHeader(update.blockHash)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to fetch "+
						"block header: %v", err)
					continue
				}

				if blockHeader.PrevBlock != *b.bestBlock.Hash {
					// Handle the case where the notifier
					// missed some blocks from its chain
					// backend
					chainntnfs.Log.Infof("Missed blocks, " +
						"attempting to catch up")
					newBestBlock, missedBlocks, err :=
						chainntnfs.HandleMissedBlocks(
							b.chainConn,
							b.txNotifier,
							b.bestBlock,
							update.blockHeight,
							true,
						)
					if err != nil {
						// Set the bestBlock here in case
						// a catch up partially completed.
						b.bestBlock = newBestBlock
						chainntnfs.Log.Error(err)
						continue
					}

					for _, block := range missedBlocks {
						err := b.handleBlockConnected(block)
						if err != nil {
							chainntnfs.Log.Error(err)
							continue out
						}
					}
				}

				newBlock := chainntnfs.BlockEpoch{
					Height: update.blockHeight,
					Hash:   update.blockHash,
				}
				if err := b.handleBlockConnected(newBlock); err != nil {
					chainntnfs.Log.Error(err)
				}
				continue
			}

			if update.blockHeight != b.bestBlock.Height {
				chainntnfs.Log.Infof("Missed disconnected" +
					"blocks, attempting to catch up")
			}

			newBestBlock, err := chainntnfs.RewindChain(
				b.chainConn, b.txNotifier, b.bestBlock,
				update.blockHeight-1,
			)
			if err != nil {
				chainntnfs.Log.Errorf("Unable to rewind chain "+
					"from height %d to height %d: %v",
					b.bestBlock.Height, update.blockHeight-1, err)
			}

			// Set the bestBlock here in case a chain rewind
			// partially completed.
			b.bestBlock = newBestBlock

		case item := <-b.txUpdates.ChanOut():
			newSpend := item.(*txUpdate)

			// We only care about notifying on confirmed spends, so
			// if this is a mempool spend, we can ignore it and wait
			// for the spend to appear in on-chain.
			if newSpend.details == nil {
				continue
			}

			tx := newSpend.tx.MsgTx()
			err := b.txNotifier.ProcessRelevantSpendTx(
				tx, newSpend.details.Height,
			)
			if err != nil {
				chainntnfs.Log.Errorf("Unable to process "+
					"transaction %v: %v", tx.TxHash(), err)
			}

		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// historicalConfDetails looks up whether a transaction is already included in a
// block in the active chain and, if so, returns details about the confirmation.
func (b *BtcdNotifier) historicalConfDetails(txid *chainhash.Hash,
	startHeight, endHeight uint32) (*chainntnfs.TxConfirmation,
	chainntnfs.TxConfStatus, error) {

	// We'll first attempt to retrieve the transaction using the node's
	// txindex.
	txConf, txStatus, err := b.confDetailsFromTxIndex(txid)

	// We'll then check the status of the transaction lookup returned to
	// determine whether we should proceed with any fallback methods.
	switch {

	// We failed querying the index for the transaction, fall back to
	// scanning manually.
	case err != nil:
		chainntnfs.Log.Debugf("Failed getting conf details from "+
			"index (%v), scanning manually", err)
		return b.confDetailsManually(txid, startHeight, endHeight)

	// The transaction was found within the node's mempool.
	case txStatus == chainntnfs.TxFoundMempool:

	// The transaction was found within the node's txindex.
	case txStatus == chainntnfs.TxFoundIndex:

	// The transaction was not found within the node's mempool or txindex.
	case txStatus == chainntnfs.TxNotFoundIndex:

	// Unexpected txStatus returned.
	default:
		return nil, txStatus,
			fmt.Errorf("Got unexpected txConfStatus: %v", txStatus)
	}

	return txConf, txStatus, nil
}

// confDetailsFromTxIndex looks up whether a transaction is already included in
// a block in the active chain by using the backend node's transaction index.
// If the transaction is found its TxConfStatus is returned. If it was found in
// the mempool this will be TxFoundMempool, if it is found in a block this will
// be TxFoundIndex. Otherwise TxNotFoundIndex is returned. If the tx is found
// in a block its confirmation details are also returned.
func (b *BtcdNotifier) confDetailsFromTxIndex(txid *chainhash.Hash,
) (*chainntnfs.TxConfirmation, chainntnfs.TxConfStatus, error) {

	// If the transaction has some or all of its confirmations required,
	// then we may be able to dispatch it immediately.
	tx, err := b.chainConn.GetRawTransactionVerbose(txid)
	if err != nil {
		// If the transaction lookup was successful, but it wasn't found
		// within the index itself, then we can exit early. We'll also
		// need to look at the error message returned as the error code
		// is used for multiple errors.
		txNotFoundErr := "No information available about transaction"
		jsonErr, ok := err.(*btcjson.RPCError)
		if ok && jsonErr.Code == btcjson.ErrRPCNoTxInfo &&
			strings.Contains(jsonErr.Message, txNotFoundErr) {

			return nil, chainntnfs.TxNotFoundIndex, nil
		}

		return nil, chainntnfs.TxNotFoundIndex,
			fmt.Errorf("unable to query for txid %v: %v", txid, err)
	}

	// Make sure we actually retrieved a transaction that is included in a
	// block. If not, the transaction must be unconfirmed (in the mempool),
	// and we'll return TxFoundMempool together with a nil TxConfirmation.
	if tx.BlockHash == "" {
		return nil, chainntnfs.TxFoundMempool, nil
	}

	// As we need to fully populate the returned TxConfirmation struct,
	// grab the block in which the transaction was confirmed so we can
	// locate its exact index within the block.
	blockHash, err := chainhash.NewHashFromStr(tx.BlockHash)
	if err != nil {
		return nil, chainntnfs.TxNotFoundIndex,
			fmt.Errorf("unable to get block hash %v for "+
				"historical dispatch: %v", tx.BlockHash, err)
	}

	block, err := b.chainConn.GetBlockVerbose(blockHash)
	if err != nil {
		return nil, chainntnfs.TxNotFoundIndex,
			fmt.Errorf("unable to get block with hash %v for "+
				"historical dispatch: %v", blockHash, err)
	}

	// If the block was obtained, locate the transaction's index within the
	// block so we can give the subscriber full confirmation details.
	targetTxidStr := txid.String()
	for txIndex, txHash := range block.Tx {
		if txHash == targetTxidStr {
			details := &chainntnfs.TxConfirmation{
				BlockHash:   blockHash,
				BlockHeight: uint32(block.Height),
				TxIndex:     uint32(txIndex),
			}
			return details, chainntnfs.TxFoundIndex, nil
		}
	}

	// We return an error because we should have found the transaction
	// within the block, but didn't.
	return nil, chainntnfs.TxNotFoundIndex,
		fmt.Errorf("unable to locate tx %v in block %v", txid,
			blockHash)
}

// confDetailsManually looks up whether a transaction is already included in a
// block in the active chain by scanning the chain's blocks, starting from the
// earliest height the transaction could have been included in, to the current
// height in the chain. If the transaction is found, its confirmation details
// are returned. Otherwise, nil is returned.
func (b *BtcdNotifier) confDetailsManually(txid *chainhash.Hash, startHeight,
	endHeight uint32) (*chainntnfs.TxConfirmation,
	chainntnfs.TxConfStatus, error) {

	targetTxidStr := txid.String()

	// Begin scanning blocks at every height to determine where the
	// transaction was included in.
	for height := startHeight; height <= endHeight; height++ {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-b.quit:
			return nil, chainntnfs.TxNotFoundManually,
				ErrChainNotifierShuttingDown
		default:
		}

		blockHash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return nil, chainntnfs.TxNotFoundManually,
				fmt.Errorf("unable to get hash from block "+
					"with height %d", height)
		}

		// TODO: fetch the neutrino filters instead.
		block, err := b.chainConn.GetBlockVerbose(blockHash)
		if err != nil {
			return nil, chainntnfs.TxNotFoundManually,
				fmt.Errorf("unable to get block with hash "+
					"%v: %v", blockHash, err)
		}

		for txIndex, txHash := range block.Tx {
			// If we're able to find the transaction in this block,
			// return its confirmation details.
			if txHash == targetTxidStr {
				details := &chainntnfs.TxConfirmation{
					BlockHash:   blockHash,
					BlockHeight: height,
					TxIndex:     uint32(txIndex),
				}
				return details, chainntnfs.TxFoundManually, nil
			}
		}
	}

	// If we reach here, then we were not able to find the transaction
	// within a block, so we avoid returning an error.
	return nil, chainntnfs.TxNotFoundManually, nil
}

// handleBlockConnected applies a chain update for a new block. Any watched
// transactions included this block will processed to either send notifications
// now or after numConfirmations confs.
// TODO(halseth): this is reusing the neutrino notifier implementation, unify
// them.
func (b *BtcdNotifier) handleBlockConnected(epoch chainntnfs.BlockEpoch) error {
	// First, we'll fetch the raw block as we'll need to gather all the
	// transactions to determine whether any are relevant to our registered
	// clients.
	rawBlock, err := b.chainConn.GetBlock(epoch.Hash)
	if err != nil {
		return fmt.Errorf("unable to get block: %v", err)
	}
	newBlock := &filteredBlock{
		hash:    *epoch.Hash,
		height:  uint32(epoch.Height),
		txns:    btcutil.NewBlock(rawBlock).Transactions(),
		connect: true,
	}

	// We'll then extend the txNotifier's height with the information of
	// this new block, which will handle all of the notification logic for
	// us.
	err = b.txNotifier.ConnectTip(
		&newBlock.hash, newBlock.height, newBlock.txns,
	)
	if err != nil {
		return fmt.Errorf("unable to connect tip: %v", err)
	}

	chainntnfs.Log.Infof("New block: height=%v, sha=%v", epoch.Height,
		epoch.Hash)

	// Now that we've guaranteed the new block extends the txNotifier's
	// current tip, we'll proceed to dispatch notifications to all of our
	// registered clients whom have had notifications fulfilled. Before
	// doing so, we'll make sure update our in memory state in order to
	// satisfy any client requests based upon the new block.
	b.bestBlock = epoch

	b.notifyBlockEpochs(epoch.Height, epoch.Hash)
	return b.txNotifier.NotifyHeight(uint32(epoch.Height))
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (b *BtcdNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	for _, client := range b.blockEpochClients {
		b.notifyBlockEpochClient(client, newHeight, newSha)
	}
}

// notifyBlockEpochClient sends a registered block epoch client a notification
// about a specific block.
func (b *BtcdNotifier) notifyBlockEpochClient(epochClient *blockEpochRegistration,
	height int32, sha *chainhash.Hash) {

	epoch := &chainntnfs.BlockEpoch{
		Height: height,
		Hash:   sha,
	}

	select {
	case epochClient.epochQueue.ChanIn() <- epoch:
	case <-epochClient.cancelChan:
	case <-b.quit:
	}
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint has been spent by a transaction on-chain. Once a spend of the target
// outpoint has been detected, the details of the spending event will be sent
// across the 'Spend' channel. The heightHint should represent the earliest
// height in the chain where the transaction could have been spent in.
func (b *BtcdNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// First, we'll construct a spend notification request and hand it off
	// to the txNotifier.
	spendID := atomic.AddUint64(&b.spendClientCounter, 1)
	cancel := func() {
		b.txNotifier.CancelSpend(*outpoint, spendID)
	}
	ntfn := &chainntnfs.SpendNtfn{
		SpendID:    spendID,
		OutPoint:   *outpoint,
		PkScript:   pkScript,
		Event:      chainntnfs.NewSpendEvent(cancel),
		HeightHint: heightHint,
	}

	historicalDispatch, err := b.txNotifier.RegisterSpend(ntfn)
	if err != nil {
		return nil, err
	}

	// If the txNotifier didn't return any details to perform a historical
	// scan of the chain, then we can return early as there's nothing left
	// for us to do.
	if historicalDispatch == nil {
		return ntfn.Event, nil
	}

	// We'll then request the backend to notify us when it has detected the
	// outpoint as spent.
	ops := []*wire.OutPoint{outpoint}
	if err := b.chainConn.NotifySpent(ops); err != nil {
		return nil, err
	}

	// In addition to the check above, we'll also check the backend's UTXO
	// set to determine whether the outpoint has been spent. If it hasn't,
	// we can return to the caller as well.
	txOut, err := b.chainConn.GetTxOut(&outpoint.Hash, outpoint.Index, true)
	if err != nil {
		return nil, err
	}
	if txOut != nil {
		// We'll let the txNotifier know the outpoint is still unspent
		// in order to begin updating its spend hint.
		err := b.txNotifier.UpdateSpendDetails(*outpoint, nil)
		if err != nil {
			return nil, err
		}

		return ntfn.Event, nil
	}

	// Otherwise, we'll determine when the output was spent by scanning the
	// chain. We'll begin by determining where to start our historical
	// rescan.
	startHash, err := b.chainConn.GetBlockHash(
		int64(historicalDispatch.StartHeight),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to get block hash for height "+
			"%d: %v", historicalDispatch.StartHeight, err)
	}

	// As a minimal optimization, we'll query the backend's transaction
	// index (if enabled) to determine if we have a better rescan starting
	// height. We can do this as the GetRawTransaction call will return the
	// hash of the block it was included in within the chain.
	tx, err := b.chainConn.GetRawTransactionVerbose(&outpoint.Hash)
	if err != nil {
		// Avoid returning an error if the transaction was not found to
		// proceed with fallback methods.
		jsonErr, ok := err.(*btcjson.RPCError)
		if !ok || jsonErr.Code != btcjson.ErrRPCNoTxInfo {
			return nil, fmt.Errorf("unable to query for "+
				"txid %v: %v", outpoint.Hash, err)
		}
	}

	// If the transaction index was enabled, we'll use the block's hash to
	// retrieve its height and check whether it provides a better starting
	// point for our rescan.
	if tx != nil {
		// If the transaction containing the outpoint hasn't confirmed
		// on-chain, then there's no need to perform a rescan.
		if tx.BlockHash == "" {
			return ntfn.Event, nil
		}

		blockHash, err := chainhash.NewHashFromStr(tx.BlockHash)
		if err != nil {
			return nil, err
		}
		blockHeader, err := b.chainConn.GetBlockHeaderVerbose(blockHash)
		if err != nil {
			return nil, fmt.Errorf("unable to get header for "+
				"block %v: %v", blockHash, err)
		}

		if uint32(blockHeader.Height) > historicalDispatch.StartHeight {
			startHash, err = b.chainConn.GetBlockHash(
				int64(blockHeader.Height),
			)
			if err != nil {
				return nil, fmt.Errorf("unable to get block "+
					"hash for height %d: %v",
					blockHeader.Height, err)
			}
		}
	}

	// In order to ensure that we don't block the caller on what may be a
	// long rescan, we'll launch a new goroutine to handle the async result
	// of the rescan. We purposefully prevent from adding this goroutine to
	// the WaitGroup as we cannnot wait for a quit signal due to the
	// asyncResult channel not being exposed.
	//
	// TODO(wilmer): add retry logic if rescan fails?
	asyncResult := b.chainConn.RescanAsync(startHash, nil, ops)
	go func() {
		if rescanErr := asyncResult.Receive(); rescanErr != nil {
			chainntnfs.Log.Errorf("Rescan to determine the spend "+
				"details of %v failed: %v", outpoint, rescanErr)
		}
	}()

	return ntfn.Event, nil
}

// RegisterConfirmationsNtfn registers a notification with BtcdNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (b *BtcdNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash, _ []byte,
	numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {

	// Construct a notification request for the transaction and send it to
	// the main event loop.
	ntfn := &chainntnfs.ConfNtfn{
		ConfID:           atomic.AddUint64(&b.confClientCounter, 1),
		TxID:             txid,
		NumConfirmations: numConfs,
		Event:            chainntnfs.NewConfirmationEvent(numConfs),
		HeightHint:       heightHint,
	}

	chainntnfs.Log.Infof("New confirmation subscription: "+
		"txid=%v, numconfs=%v", txid, numConfs)

	// Register the conf notification with the TxNotifier. A non-nil value
	// for `dispatch` will be returned if we are required to perform a
	// manual scan for the confirmation. Otherwise the notifier will begin
	// watching at tip for the transaction to confirm.
	dispatch, err := b.txNotifier.RegisterConf(ntfn)
	if err != nil {
		return nil, err
	}

	if dispatch == nil {
		return ntfn.Event, nil
	}

	select {
	case b.notificationRegistry <- dispatch:
		return ntfn.Event, nil
	case <-b.quit:
		return nil, ErrChainNotifierShuttingDown
	}
}

// blockEpochRegistration represents a client's intent to receive a
// notification with each newly connected block.
type blockEpochRegistration struct {
	epochID uint64

	epochChan chan *chainntnfs.BlockEpoch

	epochQueue *queue.ConcurrentQueue

	bestBlock *chainntnfs.BlockEpoch

	errorChan chan error

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
// chain. Clients have the option of passing in their best known block, which
// the notifier uses to check if they are behind on blocks and catch them up.
func (b *BtcdNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	reg := &blockEpochRegistration{
		epochQueue: queue.NewConcurrentQueue(20),
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&b.epochClientCounter, 1),
		bestBlock:  bestBlock,
		errorChan:  make(chan error, 1),
	}
	reg.epochQueue.Start()

	// Before we send the request to the main goroutine, we'll launch a new
	// goroutine to proxy items added to our queue to the client itself.
	// This ensures that all notifications are received *in order*.
	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()

		for {
			select {
			case ntfn := <-reg.epochQueue.ChanOut():
				blockNtfn := ntfn.(*chainntnfs.BlockEpoch)
				select {
				case reg.epochChan <- blockNtfn:

				case <-reg.cancelChan:
					return

				case <-b.quit:
					return
				}

			case <-reg.cancelChan:
				return

			case <-b.quit:
				return
			}
		}
	}()

	select {
	case <-b.quit:
		// As we're exiting before the registration could be sent,
		// we'll stop the queue now ourselves.
		reg.epochQueue.Stop()

		return nil, errors.New("chainntnfs: system interrupt while " +
			"attempting to register for block epoch notification.")
	case b.notificationRegistry <- reg:
		return &chainntnfs.BlockEpochEvent{
			Epochs: reg.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: reg.epochID,
				}

				// Submit epoch cancellation to notification dispatcher.
				select {
				case b.notificationCancels <- cancel:
					// Cancellation is being handled, drain
					// the epoch channel until it is closed
					// before yielding to caller.
					for {
						select {
						case _, ok := <-reg.epochChan:
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
