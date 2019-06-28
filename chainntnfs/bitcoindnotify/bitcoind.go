package bitcoindnotify

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "bitcoind"
)

// chainUpdate encapsulates an update to the current main chain. This struct is
// used as an element within an unbounded queue in order to avoid blocking the
// main rpc dispatch rule.
type chainUpdate struct {
	blockHash   *chainhash.Hash
	blockHeight int32
}

// TODO(roasbeef): generalize struct below:
//  * move chans to config
//  * extract common code
//  * allow outside callers to handle send conditions

// BitcoindNotifier implements the ChainNotifier interface using a bitcoind
// chain client. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type BitcoindNotifier struct {
	confClientCounter  uint64 // To be used atomically.
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn   *chain.BitcoindClient
	chainParams *chaincfg.Params

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	txNotifier *chainntnfs.TxNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	bestBlock chainntnfs.BlockEpoch

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

// Ensure BitcoindNotifier implements the ChainNotifier interface at compile
// time.
var _ chainntnfs.ChainNotifier = (*BitcoindNotifier)(nil)

// New returns a new BitcoindNotifier instance. This function assumes the
// bitcoind node detailed in the passed configuration is already running, and
// willing to accept RPC requests and new zmq clients.
func New(chainConn *chain.BitcoindConn, chainParams *chaincfg.Params,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache) *BitcoindNotifier {

	notifier := &BitcoindNotifier{
		chainParams: chainParams,

		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendHintCache:   spendHintCache,
		confirmHintCache: confirmHintCache,

		quit: make(chan struct{}),
	}

	notifier.chainConn = chainConn.NewBitcoindClient()

	return notifier
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

	currentHash, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.txNotifier = chainntnfs.NewTxNotifier(
		uint32(currentHeight), chainntnfs.ReorgSafetyLimit,
		b.confirmHintCache, b.spendHintCache,
	)

	b.bestBlock = chainntnfs.BlockEpoch{
		Height: currentHeight,
		Hash:   currentHash,
	}

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
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()

		close(epochClient.epochChan)
	}
	b.txNotifier.TearDown()

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
						msg.ConfRequest,
						msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Rescan to "+
							"determine the conf "+
							"details of %v within "+
							"range %d-%d failed: %v",
							msg.ConfRequest,
							msg.StartHeight,
							msg.EndHeight, err)
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
						msg.ConfRequest, confDetails,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Unable "+
							"to update conf "+
							"details of %v: %v",
							msg.ConfRequest, err)
					}
				}()

			case *chainntnfs.HistoricalSpendDispatch:
				// In order to ensure we don't block the caller
				// on what may be a long rescan, we'll launch a
				// goroutine to do so in the background.
				//
				// TODO(wilmer): add retry logic if rescan fails?
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()

					spendDetails, err := b.historicalSpendDetails(
						msg.SpendRequest,
						msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Rescan to "+
							"determine the spend "+
							"details of %v within "+
							"range %d-%d failed: %v",
							msg.SpendRequest,
							msg.StartHeight,
							msg.EndHeight, err)
						return
					}

					// If the historical dispatch finished
					// without error, we will invoke
					// UpdateSpendDetails even if none were
					// found. This allows the notifier to
					// begin safely updating the height hint
					// cache at tip, since any pending
					// rescans have now completed.
					err = b.txNotifier.UpdateSpendDetails(
						msg.SpendRequest, spendDetails,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Unable "+
							"to update spend "+
							"details of %v: %v",
							msg.SpendRequest, err)
					}
				}()

			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")

				b.blockEpochClients[msg.epochID] = msg

				// If the client did not provide their best
				// known block, then we'll immediately dispatch
				// a notification for the current tip.
				if msg.bestBlock == nil {
					b.notifyBlockEpochClient(
						msg, b.bestBlock.Height,
						b.bestBlock.Hash,
					)

					msg.errorChan <- nil
					continue
				}

				// Otherwise, we'll attempt to deliver the
				// backlog of notifications from their best
				// known block.
				missedBlocks, err := chainntnfs.GetClientMissedBlocks(
					b.chainConn, msg.bestBlock,
					b.bestBlock.Height, true,
				)
				if err != nil {
					msg.errorChan <- err
					continue
				}

				for _, block := range missedBlocks {
					b.notifyBlockEpochClient(
						msg, block.Height, block.Hash,
					)
				}

				msg.errorChan <- nil
			}

		case ntfn := <-b.chainConn.Notifications():
			switch item := ntfn.(type) {
			case chain.BlockConnected:
				blockHeader, err :=
					b.chainConn.GetBlockHeader(&item.Hash)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to fetch "+
						"block header: %v", err)
					continue
				}

				if blockHeader.PrevBlock != *b.bestBlock.Hash {
					// Handle the case where the notifier
					// missed some blocks from its chain
					// backend.
					chainntnfs.Log.Infof("Missed blocks, " +
						"attempting to catch up")
					newBestBlock, missedBlocks, err :=
						chainntnfs.HandleMissedBlocks(
							b.chainConn,
							b.txNotifier,
							b.bestBlock, item.Height,
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
					Height: item.Height,
					Hash:   &item.Hash,
				}
				if err := b.handleBlockConnected(newBlock); err != nil {
					chainntnfs.Log.Error(err)
				}

				continue

			case chain.BlockDisconnected:
				if item.Height != b.bestBlock.Height {
					chainntnfs.Log.Infof("Missed disconnected" +
						"blocks, attempting to catch up")
				}

				newBestBlock, err := chainntnfs.RewindChain(
					b.chainConn, b.txNotifier,
					b.bestBlock, item.Height-1,
				)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to rewind chain "+
						"from height %d to height %d: %v",
						b.bestBlock.Height, item.Height-1, err)
				}

				// Set the bestBlock here in case a chain
				// rewind partially completed.
				b.bestBlock = newBestBlock

			case chain.RelevantTx:
				// We only care about notifying on confirmed
				// spends, so if this is a mempool spend, we can
				// ignore it and wait for the spend to appear in
				// on-chain.
				if item.Block == nil {
					continue
				}

				tx := btcutil.NewTx(&item.TxRecord.MsgTx)
				err := b.txNotifier.ProcessRelevantSpendTx(
					tx, uint32(item.Block.Height),
				)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to "+
						"process transaction %v: %v",
						tx.Hash(), err)
				}
			}

		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// historicalConfDetails looks up whether a confirmation request (txid/output
// script) has already been included in a block in the active chain and, if so,
// returns details about said block.
func (b *BitcoindNotifier) historicalConfDetails(confRequest chainntnfs.ConfRequest,
	startHeight, endHeight uint32) (*chainntnfs.TxConfirmation,
	chainntnfs.TxConfStatus, error) {

	// If a txid was not provided, then we should dispatch upon seeing the
	// script on-chain, so we'll short-circuit straight to scanning manually
	// as there doesn't exist a script index to query.
	if confRequest.TxID == chainntnfs.ZeroHash {
		return b.confDetailsManually(
			confRequest, startHeight, endHeight,
		)
	}

	// Otherwise, we'll dispatch upon seeing a transaction on-chain with the
	// given hash.
	//
	// We'll first attempt to retrieve the transaction using the node's
	// txindex.
	txNotFoundErr := "No such mempool or blockchain transaction"
	txConf, txStatus, err := chainntnfs.ConfDetailsFromTxIndex(
		b.chainConn, confRequest, txNotFoundErr,
	)

	// We'll then check the status of the transaction lookup returned to
	// determine whether we should proceed with any fallback methods.
	switch {

	// We failed querying the index for the transaction, fall back to
	// scanning manually.
	case err != nil:
		chainntnfs.Log.Debugf("Failed getting conf details from "+
			"index (%v), scanning manually", err)
		return b.confDetailsManually(confRequest, startHeight, endHeight)

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

// confDetailsManually looks up whether a transaction/output script has already
// been included in a block in the active chain by scanning the chain's blocks
// within the given range. If the transaction/output script is found, its
// confirmation details are returned. Otherwise, nil is returned.
func (b *BitcoindNotifier) confDetailsManually(confRequest chainntnfs.ConfRequest,
	heightHint, currentHeight uint32) (*chainntnfs.TxConfirmation,
	chainntnfs.TxConfStatus, error) {

	// Begin scanning blocks at every height to determine where the
	// transaction was included in.
	for height := currentHeight; height >= heightHint && height > 0; height-- {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-b.quit:
			return nil, chainntnfs.TxNotFoundManually,
				chainntnfs.ErrChainNotifierShuttingDown
		default:
		}

		blockHash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return nil, chainntnfs.TxNotFoundManually,
				fmt.Errorf("unable to get hash from block "+
					"with height %d", height)
		}

		block, err := b.chainConn.GetBlock(blockHash)
		if err != nil {
			return nil, chainntnfs.TxNotFoundManually,
				fmt.Errorf("unable to get block with hash "+
					"%v: %v", blockHash, err)
		}

		// For every transaction in the block, check which one matches
		// our request. If we find one that does, we can dispatch its
		// confirmation details.
		for txIndex, tx := range block.Transactions {
			if !confRequest.MatchesTx(tx) {
				continue
			}

			return &chainntnfs.TxConfirmation{
				Tx:          tx,
				BlockHash:   blockHash,
				BlockHeight: height,
				TxIndex:     uint32(txIndex),
			}, chainntnfs.TxFoundManually, nil
		}
	}

	// If we reach here, then we were not able to find the transaction
	// within a block, so we avoid returning an error.
	return nil, chainntnfs.TxNotFoundManually, nil
}

// handleBlockConnected applies a chain update for a new block. Any watched
// transactions included this block will processed to either send notifications
// now or after numConfirmations confs.
func (b *BitcoindNotifier) handleBlockConnected(block chainntnfs.BlockEpoch) error {
	// First, we'll fetch the raw block as we'll need to gather all the
	// transactions to determine whether any are relevant to our registered
	// clients.
	rawBlock, err := b.chainConn.GetBlock(block.Hash)
	if err != nil {
		return fmt.Errorf("unable to get block: %v", err)
	}
	txns := btcutil.NewBlock(rawBlock).Transactions()

	// We'll then extend the txNotifier's height with the information of
	// this new block, which will handle all of the notification logic for
	// us.
	err = b.txNotifier.ConnectTip(block.Hash, uint32(block.Height), txns)
	if err != nil {
		return fmt.Errorf("unable to connect tip: %v", err)
	}

	chainntnfs.Log.Infof("New block: height=%v, sha=%v", block.Height,
		block.Hash)

	// Now that we've guaranteed the new block extends the txNotifier's
	// current tip, we'll proceed to dispatch notifications to all of our
	// registered clients whom have had notifications fulfilled. Before
	// doing so, we'll make sure update our in memory state in order to
	// satisfy any client requests based upon the new block.
	b.bestBlock = block

	b.notifyBlockEpochs(block.Height, block.Hash)
	return b.txNotifier.NotifyHeight(uint32(block.Height))
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (b *BitcoindNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	for _, client := range b.blockEpochClients {
		b.notifyBlockEpochClient(client, newHeight, newSha)
	}
}

// notifyBlockEpochClient sends a registered block epoch client a notification
// about a specific block.
func (b *BitcoindNotifier) notifyBlockEpochClient(epochClient *blockEpochRegistration,
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
// outpoint/output script has been spent by a transaction on-chain. When
// intending to be notified of the spend of an output script, a nil outpoint
// must be used. The heightHint should represent the earliest height in the
// chain of the transaction that spent the outpoint/output script.
//
// Once a spend of has been detected, the details of the spending event will be
// sent across the 'Spend' channel.
func (b *BitcoindNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// First, we'll construct a spend notification request and hand it off
	// to the txNotifier.
	spendID := atomic.AddUint64(&b.spendClientCounter, 1)
	spendRequest, err := chainntnfs.NewSpendRequest(outpoint, pkScript)
	if err != nil {
		return nil, err
	}
	ntfn := &chainntnfs.SpendNtfn{
		SpendID:      spendID,
		SpendRequest: spendRequest,
		Event: chainntnfs.NewSpendEvent(func() {
			b.txNotifier.CancelSpend(spendRequest, spendID)
		}),
		HeightHint: heightHint,
	}

	historicalDispatch, _, err := b.txNotifier.RegisterSpend(ntfn)
	if err != nil {
		return nil, err
	}

	// We'll then request the backend to notify us when it has detected the
	// outpoint/output script as spent.
	//
	// TODO(wilmer): use LoadFilter API instead.
	if spendRequest.OutPoint == chainntnfs.ZeroOutPoint {
		addr, err := spendRequest.PkScript.Address(b.chainParams)
		if err != nil {
			return nil, err
		}
		addrs := []btcutil.Address{addr}
		if err := b.chainConn.NotifyReceived(addrs); err != nil {
			return nil, err
		}
	} else {
		ops := []*wire.OutPoint{&spendRequest.OutPoint}
		if err := b.chainConn.NotifySpent(ops); err != nil {
			return nil, err
		}
	}

	// If the txNotifier didn't return any details to perform a historical
	// scan of the chain, then we can return early as there's nothing left
	// for us to do.
	if historicalDispatch == nil {
		return ntfn.Event, nil
	}

	// Otherwise, we'll need to dispatch a historical rescan to determine if
	// the outpoint was already spent at a previous height.
	//
	// We'll short-circuit the path when dispatching the spend of a script,
	// rather than an outpoint, as there aren't any additional checks we can
	// make for scripts.
	if spendRequest.OutPoint == chainntnfs.ZeroOutPoint {
		select {
		case b.notificationRegistry <- historicalDispatch:
		case <-b.quit:
			return nil, chainntnfs.ErrChainNotifierShuttingDown
		}

		return ntfn.Event, nil
	}

	// When dispatching spends of outpoints, there are a number of checks we
	// can make to start our rescan from a better height or completely avoid
	// it.
	//
	// We'll start by checking the backend's UTXO set to determine whether
	// the outpoint has been spent. If it hasn't, we can return to the
	// caller as well.
	txOut, err := b.chainConn.GetTxOut(
		&spendRequest.OutPoint.Hash, spendRequest.OutPoint.Index, true,
	)
	if err != nil {
		return nil, err
	}
	if txOut != nil {
		// We'll let the txNotifier know the outpoint is still unspent
		// in order to begin updating its spend hint.
		err := b.txNotifier.UpdateSpendDetails(spendRequest, nil)
		if err != nil {
			return nil, err
		}

		return ntfn.Event, nil
	}

	// Since the outpoint was spent, as it no longer exists within the UTXO
	// set, we'll determine when it happened by scanning the chain.
	//
	// As a minimal optimization, we'll query the backend's transaction
	// index (if enabled) to determine if we have a better rescan starting
	// height. We can do this as the GetRawTransaction call will return the
	// hash of the block it was included in within the chain.
	tx, err := b.chainConn.GetRawTransactionVerbose(&spendRequest.OutPoint.Hash)
	if err != nil {
		// Avoid returning an error if the transaction was not found to
		// proceed with fallback methods.
		jsonErr, ok := err.(*btcjson.RPCError)
		if !ok || jsonErr.Code != btcjson.ErrRPCNoTxInfo {
			return nil, fmt.Errorf("unable to query for txid %v: %v",
				spendRequest.OutPoint.Hash, err)
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
		blockHeight, err := b.chainConn.GetBlockHeight(blockHash)
		if err != nil {
			return nil, err
		}

		if uint32(blockHeight) > historicalDispatch.StartHeight {
			historicalDispatch.StartHeight = uint32(blockHeight)
		}
	}

	// Now that we've determined the starting point of our rescan, we can
	// dispatch it and return.
	select {
	case b.notificationRegistry <- historicalDispatch:
	case <-b.quit:
		return nil, chainntnfs.ErrChainNotifierShuttingDown
	}

	return ntfn.Event, nil
}

// historicalSpendDetails attempts to manually scan the chain within the given
// height range for a transaction that spends the given outpoint/output script.
// If one is found, the spend details are assembled and returned to the caller.
// If the spend is not found, a nil spend detail will be returned.
func (b *BitcoindNotifier) historicalSpendDetails(
	spendRequest chainntnfs.SpendRequest, startHeight, endHeight uint32) (
	*chainntnfs.SpendDetail, error) {

	// Begin scanning blocks at every height to determine if the outpoint
	// was spent.
	for height := endHeight; height >= startHeight && height > 0; height-- {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-b.quit:
			return nil, chainntnfs.ErrChainNotifierShuttingDown
		default:
		}

		// First, we'll fetch the block for the current height.
		blockHash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve hash for "+
				"block with height %d: %v", height, err)
		}
		block, err := b.chainConn.GetBlock(blockHash)
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve block "+
				"with hash %v: %v", blockHash, err)
		}

		// Then, we'll manually go over every input in every transaction
		// in it and determine whether it spends the request in
		// question. If we find one, we'll dispatch the spend details.
		for _, tx := range block.Transactions {
			matches, inputIdx, err := spendRequest.MatchesTx(tx)
			if err != nil {
				return nil, err
			}
			if !matches {
				continue
			}

			txHash := tx.TxHash()
			return &chainntnfs.SpendDetail{
				SpentOutPoint:     &tx.TxIn[inputIdx].PreviousOutPoint,
				SpenderTxHash:     &txHash,
				SpendingTx:        tx,
				SpenderInputIndex: inputIdx,
				SpendingHeight:    int32(height),
			}, nil
		}
	}

	return nil, nil
}

// RegisterConfirmationsNtfn registers an intent to be notified once the target
// txid/output script has reached numConfs confirmations on-chain. When
// intending to be notified of the confirmation of an output script, a nil txid
// must be used. The heightHint should represent the earliest height at which
// the txid/output script could have been included in the chain.
//
// Progress on the number of confirmations left can be read from the 'Updates'
// channel. Once it has reached all of its confirmations, a notification will be
// sent across the 'Confirmed' channel.
func (b *BitcoindNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte,
	numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {

	// Construct a notification request for the transaction and send it to
	// the main event loop.
	confID := atomic.AddUint64(&b.confClientCounter, 1)
	confRequest, err := chainntnfs.NewConfRequest(txid, pkScript)
	if err != nil {
		return nil, err
	}
	ntfn := &chainntnfs.ConfNtfn{
		ConfID:           confID,
		ConfRequest:      confRequest,
		NumConfirmations: numConfs,
		Event: chainntnfs.NewConfirmationEvent(numConfs, func() {
			b.txNotifier.CancelConf(confRequest, confID)
		}),
		HeightHint: heightHint,
	}

	chainntnfs.Log.Infof("New confirmation subscription: %v, num_confs=%v",
		confRequest, numConfs)

	// Register the conf notification with the TxNotifier. A non-nil value
	// for `dispatch` will be returned if we are required to perform a
	// manual scan for the confirmation. Otherwise the notifier will begin
	// watching at tip for the transaction to confirm.
	dispatch, _, err := b.txNotifier.RegisterConf(ntfn)
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
		return nil, chainntnfs.ErrChainNotifierShuttingDown
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

// epochCancel is a message sent to the BitcoindNotifier when a client wishes
// to cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications, of each new block connected to the main
// chain. Clients have the option of passing in their best known block, which
// the notifier uses to check if they are behind on blocks and catch them up. If
// they do not provide one, then a notification will be dispatched immediately
// for the current tip of the chain upon a successful registration.
func (b *BitcoindNotifier) RegisterBlockEpochNtfn(
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
					// Cancellation is being handled, drain the epoch channel until it is
					// closed before yielding to caller.
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
