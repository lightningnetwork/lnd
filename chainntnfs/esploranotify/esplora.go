package esploranotify

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/esplora"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "esplora"
)

var (
	// ErrEsploraNotifierShuttingDown is returned when the notifier is
	// shutting down.
	ErrEsploraNotifierShuttingDown = errors.New(
		"esplora notifier is shutting down",
	)
)

// EsploraNotifier implements the ChainNotifier interface using an Esplora
// HTTP API as the chain backend. This provides a lightweight way to receive
// chain notifications without running a full node.
type EsploraNotifier struct {
	epochClientCounter uint64 // To be used atomically.

	start   sync.Once
	active  int32 // To be used atomically.
	stopped int32 // To be used atomically.

	bestBlockMtx sync.RWMutex
	bestBlock    chainntnfs.BlockEpoch

	// client is the Esplora client used to communicate with the API.
	client *esplora.Client

	// subscriptionID is the ID of our block notification subscription.
	subscriptionID uint64

	// chainParams are the parameters of the chain we're connected to.
	chainParams *chaincfg.Params

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	txNotifier *chainntnfs.TxNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	// spendHintCache is a cache used to query and update the latest height
	// hints for an outpoint.
	spendHintCache chainntnfs.SpendHintCache

	// confirmHintCache is a cache used to query the latest height hints for
	// a transaction.
	confirmHintCache chainntnfs.ConfirmHintCache

	// blockCache is an LRU block cache.
	blockCache *blockcache.BlockCache

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure EsploraNotifier implements the ChainNotifier interface at compile
// time.
var _ chainntnfs.ChainNotifier = (*EsploraNotifier)(nil)

// New creates a new instance of the EsploraNotifier. The Esplora client
// should already be started and connected before being passed to this
// function.
func New(client *esplora.Client, chainParams *chaincfg.Params,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache,
	blockCache *blockcache.BlockCache) *EsploraNotifier {

	return &EsploraNotifier{
		client:      client,
		chainParams: chainParams,

		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendHintCache:   spendHintCache,
		confirmHintCache: confirmHintCache,

		blockCache: blockCache,

		quit: make(chan struct{}),
	}
}

// Start establishes the connection to the Esplora API and begins
// processing block notifications.
func (e *EsploraNotifier) Start() error {
	var startErr error
	e.start.Do(func() {
		startErr = e.startNotifier()
	})
	return startErr
}

// startNotifier is the internal method that performs the actual startup.
func (e *EsploraNotifier) startNotifier() error {
	log.Info("Esplora notifier starting...")

	// Ensure the client is connected.
	if !e.client.IsConnected() {
		return errors.New("esplora client is not connected")
	}

	// Get the current best block from the Esplora API.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tipHash, err := e.client.GetTipHash(ctx)
	if err != nil {
		return fmt.Errorf("failed to get tip hash: %w", err)
	}

	tipHeight, err := e.client.GetTipHeight(ctx)
	if err != nil {
		return fmt.Errorf("failed to get tip height: %w", err)
	}

	blockHeader, err := e.client.GetBlockHeader(ctx, tipHash)
	if err != nil {
		return fmt.Errorf("failed to get block header: %w", err)
	}

	blockHash, err := chainhash.NewHashFromStr(tipHash)
	if err != nil {
		return fmt.Errorf("failed to parse block hash: %w", err)
	}

	e.bestBlockMtx.Lock()
	e.bestBlock = chainntnfs.BlockEpoch{
		Height:      int32(tipHeight),
		Hash:        blockHash,
		BlockHeader: blockHeader,
	}
	e.bestBlockMtx.Unlock()

	log.Infof("Esplora notifier started at height %d, hash %s",
		tipHeight, tipHash)

	// Initialize the transaction notifier with the current best height.
	e.txNotifier = chainntnfs.NewTxNotifier(
		uint32(tipHeight), chainntnfs.ReorgSafetyLimit,
		e.confirmHintCache, e.spendHintCache,
	)

	// Start the notification dispatcher goroutine.
	e.wg.Add(1)
	go e.notificationDispatcher()

	// Start the block polling handler.
	e.wg.Add(1)
	go e.blockPollingHandler()

	// Mark the notifier as active.
	atomic.StoreInt32(&e.active, 1)

	log.Debug("Esplora notifier started successfully")

	return nil
}

// Stop shuts down the EsploraNotifier.
func (e *EsploraNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&e.stopped, 1) != 1 {
		return nil
	}

	log.Info("Esplora notifier shutting down...")
	defer log.Debug("Esplora notifier shutdown complete")

	close(e.quit)
	e.wg.Wait()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, epochClient := range e.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()
		close(epochClient.epochChan)
	}

	// Tear down the transaction notifier if it was initialized.
	if e.txNotifier != nil {
		e.txNotifier.TearDown()
	}

	return nil
}

// Started returns true if this instance has been started, and false otherwise.
func (e *EsploraNotifier) Started() bool {
	return atomic.LoadInt32(&e.active) != 0
}

// blockPollingHandler polls for new blocks from the Esplora API.
func (e *EsploraNotifier) blockPollingHandler() {
	defer e.wg.Done()

	// Subscribe to block notifications from the client.
	blockNotifs, subID := e.client.Subscribe()
	e.subscriptionID = subID

	defer e.client.Unsubscribe(subID)

	for {
		select {
		case blockInfo, ok := <-blockNotifs:
			if !ok {
				log.Warn("Block notification channel closed")
				return
			}

			if blockInfo == nil {
				continue
			}

			newHeight := int32(blockInfo.Height)

			// Fetch the block header.
			ctx, cancel := context.WithTimeout(
				context.Background(), 30*time.Second,
			)
			blockHeader, err := e.client.GetBlockHeader(ctx, blockInfo.ID)
			cancel()
			if err != nil {
				log.Errorf("Failed to get block header: %v", err)
				continue
			}

			blockHash, err := chainhash.NewHashFromStr(blockInfo.ID)
			if err != nil {
				log.Errorf("Failed to parse block hash: %v", err)
				continue
			}

			// Check if this is a new block or a reorg.
			e.bestBlockMtx.RLock()
			prevHeight := e.bestBlock.Height
			prevHash := e.bestBlock.Hash
			e.bestBlockMtx.RUnlock()

			// Handle the new block.
			if newHeight > prevHeight {
				// New block connected.
				e.handleBlockConnected(newHeight, blockHash, blockHeader)
			} else if newHeight <= prevHeight && !blockHash.IsEqual(prevHash) {
				// Potential reorg detected.
				log.Warnf("Potential reorg detected: "+
					"prev_height=%d, new_height=%d",
					prevHeight, newHeight)

				e.handleReorg(prevHeight, newHeight, blockHash, blockHeader)
			}

		case <-e.quit:
			return
		}
	}
}

// handleBlockConnected processes a newly connected block.
func (e *EsploraNotifier) handleBlockConnected(height int32,
	hash *chainhash.Hash, header *wire.BlockHeader) {

	log.Debugf("New block connected: height=%d, hash=%s", height, hash)

	// Update the best block.
	e.bestBlockMtx.Lock()
	e.bestBlock = chainntnfs.BlockEpoch{
		Height:      height,
		Hash:        hash,
		BlockHeader: header,
	}
	e.bestBlockMtx.Unlock()

	// Notify all block epoch clients about the new block.
	for _, client := range e.blockEpochClients {
		e.notifyBlockEpochClient(client, height, hash, header)
	}

	// Update the txNotifier's height.
	if e.txNotifier != nil {
		err := e.txNotifier.NotifyHeight(uint32(height))
		if err != nil {
			log.Errorf("Failed to notify height: %v", err)
		}

		// Check pending confirmations and spends in parallel.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			e.checkPendingConfirmations(uint32(height))
		}()
		go func() {
			defer wg.Done()
			e.checkPendingSpends(uint32(height))
		}()
		wg.Wait()
	}
}

// checkPendingConfirmations queries the Esplora API to check if any
// pending confirmation requests have been satisfied.
func (e *EsploraNotifier) checkPendingConfirmations(currentHeight uint32) {
	unconfirmed := e.txNotifier.UnconfirmedRequests()
	if len(unconfirmed) == 0 {
		return
	}

	log.Debugf("Checking %d pending confirmation requests at height %d",
		len(unconfirmed), currentHeight)

	for _, confRequest := range unconfirmed {
		confDetails, err := e.historicalConfDetails(
			confRequest, 0, currentHeight,
		)
		if err != nil {
			log.Debugf("Error checking confirmation for %v: %v",
				confRequest, err)
			continue
		}

		if confDetails == nil {
			continue
		}

		log.Infof("Found confirmation for pending request %v at "+
			"height %d", confRequest, confDetails.BlockHeight)

		err = e.txNotifier.UpdateConfDetails(confRequest, confDetails)
		if err != nil {
			log.Errorf("Failed to update conf details for %v: %v",
				confRequest, err)
		}
	}
}

// checkPendingSpends queries the Esplora API to check if any pending
// spend requests have been satisfied.
func (e *EsploraNotifier) checkPendingSpends(currentHeight uint32) {
	unspent := e.txNotifier.UnspentRequests()
	if len(unspent) == 0 {
		return
	}

	log.Debugf("Checking %d pending spend requests at height %d",
		len(unspent), currentHeight)

	for _, spendRequest := range unspent {
		spendDetails, err := e.historicalSpendDetails(
			spendRequest, 0, currentHeight,
		)
		if err != nil {
			log.Debugf("Error checking spend for %v: %v",
				spendRequest, err)
			continue
		}

		if spendDetails == nil {
			continue
		}

		log.Infof("Found spend for pending request %v at height %d",
			spendRequest, spendDetails.SpendingHeight)

		err = e.txNotifier.UpdateSpendDetails(spendRequest, spendDetails)
		if err != nil {
			log.Errorf("Failed to update spend details for %v: %v",
				spendRequest, err)
		}
	}
}

// handleReorg handles a chain reorganization.
func (e *EsploraNotifier) handleReorg(prevHeight, newHeight int32,
	newHash *chainhash.Hash, newHeader *wire.BlockHeader) {

	if e.txNotifier != nil {
		for h := uint32(prevHeight); h > uint32(newHeight); h-- {
			err := e.txNotifier.DisconnectTip(h)
			if err != nil {
				log.Errorf("Failed to disconnect tip at "+
					"height %d: %v", h, err)
			}
		}
	}

	e.handleBlockConnected(newHeight, newHash, newHeader)
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (e *EsploraNotifier) notificationDispatcher() {
	defer e.wg.Done()

	for {
		select {
		case cancelMsg := <-e.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *epochCancel:
				log.Infof("Cancelling epoch notification, "+
					"epoch_id=%v", msg.epochID)

				reg := e.blockEpochClients[msg.epochID]
				if reg != nil {
					reg.epochQueue.Stop()
					close(reg.cancelChan)
					reg.wg.Wait()
					close(reg.epochChan)
					delete(e.blockEpochClients, msg.epochID)
				}
			}

		case registerMsg := <-e.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *blockEpochRegistration:
				log.Infof("New block epoch subscription, "+
					"epoch_id=%v", msg.epochID)

				e.blockEpochClients[msg.epochID] = msg

				if msg.bestBlock != nil {
					e.dispatchMissedBlocks(msg)
				} else {
					e.bestBlockMtx.RLock()
					bestBlock := e.bestBlock
					e.bestBlockMtx.RUnlock()

					e.notifyBlockEpochClient(
						msg, bestBlock.Height,
						bestBlock.Hash,
						bestBlock.BlockHeader,
					)
				}

				msg.errorChan <- nil
			}

		case <-e.quit:
			return
		}
	}
}

// handleHistoricalConfDispatch handles a request to look up historical
// confirmation details for a transaction.
func (e *EsploraNotifier) handleHistoricalConfDispatch(
	dispatch *chainntnfs.HistoricalConfDispatch) {

	defer e.wg.Done()

	confDetails, err := e.historicalConfDetails(
		dispatch.ConfRequest, dispatch.StartHeight, dispatch.EndHeight,
	)
	if err != nil {
		log.Errorf("Failed to get historical conf details for %v: %v",
			dispatch.ConfRequest, err)
		return
	}

	err = e.txNotifier.UpdateConfDetails(dispatch.ConfRequest, confDetails)
	if err != nil {
		log.Errorf("Failed to update conf details for %v: %v",
			dispatch.ConfRequest, err)
	}
}

// handleHistoricalSpendDispatch handles a request to look up historical
// spend details for an outpoint.
func (e *EsploraNotifier) handleHistoricalSpendDispatch(
	dispatch *chainntnfs.HistoricalSpendDispatch) {

	defer e.wg.Done()

	spendDetails, err := e.historicalSpendDetails(
		dispatch.SpendRequest, dispatch.StartHeight, dispatch.EndHeight,
	)
	if err != nil {
		log.Errorf("Failed to get historical spend details for %v: %v",
			dispatch.SpendRequest, err)
		return
	}

	err = e.txNotifier.UpdateSpendDetails(dispatch.SpendRequest, spendDetails)
	if err != nil {
		log.Errorf("Failed to update spend details for %v: %v",
			dispatch.SpendRequest, err)
	}
}

// historicalConfDetails looks up the confirmation details for a transaction
// within the given height range.
func (e *EsploraNotifier) historicalConfDetails(
	confRequest chainntnfs.ConfRequest,
	startHeight, endHeight uint32) (*chainntnfs.TxConfirmation, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If we have a txid, try to get the transaction directly.
	if confRequest.TxID != chainntnfs.ZeroHash {
		txInfo, err := e.client.GetTransaction(ctx, confRequest.TxID.String())
		if err == nil && txInfo != nil && txInfo.Status.Confirmed {
			blockHash, err := chainhash.NewHashFromStr(txInfo.Status.BlockHash)
			if err != nil {
				return nil, fmt.Errorf("invalid block hash: %w", err)
			}

			// Fetch the actual transaction.
			var msgTx *wire.MsgTx
			msgTx, err = e.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
			if err != nil {
				log.Debugf("Failed to fetch raw tx: %v", err)
			}

			// Get the TxIndex.
			txIndex, err := e.client.GetTxIndex(
				ctx, txInfo.Status.BlockHash, txInfo.TxID,
			)
			if err != nil {
				log.Debugf("Failed to get TxIndex: %v", err)
			}

			return &chainntnfs.TxConfirmation{
				BlockHash:   blockHash,
				BlockHeight: uint32(txInfo.Status.BlockHeight),
				TxIndex:     txIndex,
				Tx:          msgTx,
			}, nil
		}

		if err != nil {
			log.Debugf("GetTransaction for %v failed: %v",
				confRequest.TxID, err)
		}
	}

	// If we don't have a pkScript, we can't do scripthash lookup.
	if confRequest.PkScript.Script() == nil ||
		len(confRequest.PkScript.Script()) == 0 {
		return nil, nil
	}

	// Search by scripthash.
	scripthash := esplora.ScripthashFromScript(confRequest.PkScript.Script())

	txs, err := e.client.GetScripthashTxs(ctx, scripthash)
	if err != nil {
		return nil, fmt.Errorf("failed to get scripthash txs: %w", err)
	}

	targetTxID := confRequest.TxID.String()
	for _, txInfo := range txs {
		if !txInfo.Status.Confirmed {
			continue
		}

		if confRequest.TxID != chainntnfs.ZeroHash {
			if txInfo.TxID != targetTxID {
				continue
			}
		} else if uint32(txInfo.Status.BlockHeight) < startHeight ||
			uint32(txInfo.Status.BlockHeight) > endHeight {
			continue
		}

		blockHash, err := chainhash.NewHashFromStr(txInfo.Status.BlockHash)
		if err != nil {
			continue
		}

		log.Debugf("Found confirmed tx %s at height %d via scripthash",
			txInfo.TxID, txInfo.Status.BlockHeight)

		var msgTx *wire.MsgTx
		msgTx, err = e.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
		if err != nil {
			log.Debugf("Failed to fetch raw tx %s: %v", txInfo.TxID, err)
		}

		txIndex, err := e.client.GetTxIndex(
			ctx, txInfo.Status.BlockHash, txInfo.TxID,
		)
		if err != nil {
			log.Debugf("Failed to get TxIndex: %v", err)
		}

		return &chainntnfs.TxConfirmation{
			BlockHash:   blockHash,
			BlockHeight: uint32(txInfo.Status.BlockHeight),
			TxIndex:     txIndex,
			Tx:          msgTx,
		}, nil
	}

	return nil, nil
}

// historicalSpendDetails looks up the spend details for an outpoint within
// the given height range.
func (e *EsploraNotifier) historicalSpendDetails(
	spendRequest chainntnfs.SpendRequest,
	startHeight, endHeight uint32) (*chainntnfs.SpendDetail, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First, check if the output is spent using the outspend endpoint.
	outSpend, err := e.client.GetTxOutSpend(
		ctx, spendRequest.OutPoint.Hash.String(),
		spendRequest.OutPoint.Index,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to check outspend: %w", err)
	}

	if !outSpend.Spent {
		return nil, nil
	}

	// The output is spent, get the spending transaction.
	if !outSpend.Status.Confirmed {
		// Spent but not confirmed yet.
		return nil, nil
	}

	if uint32(outSpend.Status.BlockHeight) < startHeight ||
		uint32(outSpend.Status.BlockHeight) > endHeight {
		return nil, nil
	}

	// Fetch the spending transaction.
	spenderHash, err := chainhash.NewHashFromStr(outSpend.TxID)
	if err != nil {
		return nil, fmt.Errorf("invalid spender txid: %w", err)
	}

	spendingTx, err := e.client.GetRawTransactionMsgTx(ctx, outSpend.TxID)
	if err != nil {
		return nil, fmt.Errorf("failed to get spending tx: %w", err)
	}

	return &chainntnfs.SpendDetail{
		SpentOutPoint:     &spendRequest.OutPoint,
		SpenderTxHash:     spenderHash,
		SpendingTx:        spendingTx,
		SpenderInputIndex: outSpend.Vin,
		SpendingHeight:    int32(outSpend.Status.BlockHeight),
	}, nil
}

// dispatchMissedBlocks sends block epoch notifications for any blocks that
// the client may have missed.
func (e *EsploraNotifier) dispatchMissedBlocks(
	registration *blockEpochRegistration) {

	e.bestBlockMtx.RLock()
	currentHeight := e.bestBlock.Height
	e.bestBlockMtx.RUnlock()

	startHeight := registration.bestBlock.Height + 1

	for height := startHeight; height <= currentHeight; height++ {
		ctx, cancel := context.WithTimeout(
			context.Background(), 30*time.Second,
		)

		hashStr, err := e.client.GetBlockHashByHeight(ctx, int64(height))
		cancel()
		if err != nil {
			log.Errorf("Failed to get block hash at height %d: %v",
				height, err)
			continue
		}

		ctx, cancel = context.WithTimeout(
			context.Background(), 30*time.Second,
		)
		header, err := e.client.GetBlockHeader(ctx, hashStr)
		cancel()
		if err != nil {
			log.Errorf("Failed to get block header at height %d: %v",
				height, err)
			continue
		}

		blockHash, err := chainhash.NewHashFromStr(hashStr)
		if err != nil {
			continue
		}

		e.notifyBlockEpochClient(registration, height, blockHash, header)
	}
}

// notifyBlockEpochClient sends a block epoch notification to a specific client.
func (e *EsploraNotifier) notifyBlockEpochClient(
	registration *blockEpochRegistration, height int32,
	hash *chainhash.Hash, header *wire.BlockHeader) {

	epoch := &chainntnfs.BlockEpoch{
		Height:      height,
		Hash:        hash,
		BlockHeader: header,
	}

	select {
	case registration.epochQueue.ChanIn() <- epoch:
	case <-registration.cancelChan:
	case <-e.quit:
	}
}

// RegisterConfirmationsNtfn registers an intent to be notified once the
// target txid/output script has reached numConfs confirmations on-chain.
func (e *EsploraNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	ntfn, err := e.txNotifier.RegisterConf(
		txid, pkScript, numConfs, heightHint, opts...,
	)
	if err != nil {
		return nil, err
	}

	if ntfn.HistoricalDispatch != nil {
		e.wg.Add(1)
		go e.handleHistoricalConfDispatch(ntfn.HistoricalDispatch)
	}

	return ntfn.Event, nil
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint/output script has been spent by a transaction on-chain.
func (e *EsploraNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	ntfn, err := e.txNotifier.RegisterSpend(outpoint, pkScript, heightHint)
	if err != nil {
		return nil, err
	}

	if ntfn.HistoricalDispatch != nil {
		e.wg.Add(1)
		go e.handleHistoricalSpendDispatch(ntfn.HistoricalDispatch)
	}

	return ntfn.Event, nil
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications of each new block connected to the main
// chain.
func (e *EsploraNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	reg := &blockEpochRegistration{
		epochQueue: queue.NewConcurrentQueue(20),
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&e.epochClientCounter, 1),
		bestBlock:  bestBlock,
		errorChan:  make(chan error, 1),
	}
	reg.epochQueue.Start()

	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()

		for {
			select {
			case item := <-reg.epochQueue.ChanOut():
				epoch := item.(*chainntnfs.BlockEpoch)
				select {
				case reg.epochChan <- epoch:
				case <-reg.cancelChan:
					return
				case <-e.quit:
					return
				}

			case <-reg.cancelChan:
				return

			case <-e.quit:
				return
			}
		}
	}()

	select {
	case e.notificationRegistry <- reg:
		return &chainntnfs.BlockEpochEvent{
			Epochs: reg.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: reg.epochID,
				}

				select {
				case e.notificationCancels <- cancel:
				case <-e.quit:
				}
			},
		}, <-reg.errorChan

	case <-e.quit:
		reg.epochQueue.Stop()
		return nil, ErrEsploraNotifierShuttingDown
	}
}

// GetBlock attempts to retrieve a block from the Esplora API.
func (e *EsploraNotifier) GetBlock(hash chainhash.Hash) (*btcutil.Block,
	error) {

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	return e.client.GetBlock(ctx, &hash)
}

// blockEpochRegistration represents a client's registration for block epoch
// notifications.
type blockEpochRegistration struct {
	epochID    uint64
	epochChan  chan *chainntnfs.BlockEpoch
	epochQueue *queue.ConcurrentQueue
	cancelChan chan struct{}
	bestBlock  *chainntnfs.BlockEpoch
	errorChan  chan error
	wg         sync.WaitGroup
}

// epochCancel is a message sent to cancel a block epoch registration.
type epochCancel struct {
	epochID uint64
}

// parseBlockHeader parses a hex-encoded block header into a wire.BlockHeader.
func parseBlockHeader(hexHeader string) (*wire.BlockHeader, error) {
	headerBytes, err := hex.DecodeString(hexHeader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode header hex: %w", err)
	}

	var header wire.BlockHeader
	err = header.Deserialize(bytes.NewReader(headerBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize header: %w", err)
	}

	return &header, nil
}
