package electrumnotify

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
	"github.com/lightningnetwork/lnd/electrum"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "electrum"
)

var (
	// ErrElectrumNotifierShuttingDown is returned when the notifier is
	// shutting down.
	ErrElectrumNotifierShuttingDown = errors.New(
		"electrum notifier is shutting down",
	)
)

// ElectrumNotifier implements the ChainNotifier interface using an Electrum
// server as the chain backend. This provides a lightweight way to receive
// chain notifications without running a full node.
//
// NOTE: Electrum servers do not serve full blocks, so this implementation has
// limitations compared to full-node backends. Confirmation and spend tracking
// is done via scripthash-based queries.
type ElectrumNotifier struct {
	epochClientCounter uint64 // To be used atomically.

	start   sync.Once
	active  int32 // To be used atomically.
	stopped int32 // To be used atomically.

	bestBlockMtx sync.RWMutex
	bestBlock    chainntnfs.BlockEpoch

	// client is the Electrum client used to communicate with the server.
	client *electrum.Client

	// restClient is an optional REST API client for mempool/electrs.
	// Used to fetch TxIndex for channel validation.
	restClient *electrum.RESTClient

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

// Ensure ElectrumNotifier implements the ChainNotifier interface at compile
// time.
var _ chainntnfs.ChainNotifier = (*ElectrumNotifier)(nil)

// New creates a new instance of the ElectrumNotifier. The Electrum client
// should already be started and connected before being passed to this
// function. If restURL is provided, the notifier will use the mempool/electrs
// REST API to fetch TxIndex for proper channel validation.
func New(client *electrum.Client, chainParams *chaincfg.Params,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache,
	blockCache *blockcache.BlockCache,
	restURL string) *ElectrumNotifier {

	var restClient *electrum.RESTClient
	if restURL != "" {
		restClient = electrum.NewRESTClient(restURL)
		log.Infof("Electrum notifier REST API enabled: %s", restURL)
	}

	return &ElectrumNotifier{
		client:      client,
		restClient:  restClient,
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

// Start establishes the connection to the Electrum server and begins
// processing block notifications.
func (e *ElectrumNotifier) Start() error {
	var startErr error
	e.start.Do(func() {
		startErr = e.startNotifier()
	})
	return startErr
}

// startNotifier is the internal method that performs the actual startup.
func (e *ElectrumNotifier) startNotifier() error {
	log.Info("Electrum notifier starting...")

	// Ensure the client is connected.
	if !e.client.IsConnected() {
		return errors.New("electrum client is not connected")
	}

	// Get the current best block from the Electrum server by subscribing
	// to headers.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	headersChan, err := e.client.SubscribeHeaders(ctx)
	if err != nil {
		return fmt.Errorf("failed to subscribe to headers: %w", err)
	}

	// The first message on the headers channel is the current tip.
	select {
	case headerResult := <-headersChan:
		if headerResult == nil {
			return errors.New("received nil header result")
		}

		blockHeader, err := parseBlockHeader(headerResult.Hex)
		if err != nil {
			return fmt.Errorf("failed to parse block header: %w",
				err)
		}

		blockHash := blockHeader.BlockHash()

		e.bestBlockMtx.Lock()
		e.bestBlock = chainntnfs.BlockEpoch{
			Height:      int32(headerResult.Height),
			Hash:        &blockHash,
			BlockHeader: blockHeader,
		}
		e.bestBlockMtx.Unlock()

		log.Infof("Electrum notifier started at height %d, hash %s",
			headerResult.Height, blockHash.String())

	case <-time.After(30 * time.Second):
		return errors.New("timeout waiting for initial block header")

	case <-e.quit:
		return ErrElectrumNotifierShuttingDown
	}

	// Initialize the transaction notifier with the current best height.
	e.bestBlockMtx.RLock()
	currentHeight := uint32(e.bestBlock.Height)
	e.bestBlockMtx.RUnlock()

	e.txNotifier = chainntnfs.NewTxNotifier(
		currentHeight, chainntnfs.ReorgSafetyLimit,
		e.confirmHintCache, e.spendHintCache,
	)

	// Start the notification dispatcher goroutine.
	e.wg.Add(1)
	go e.notificationDispatcher()

	// Start the block subscription handler.
	e.wg.Add(1)
	go e.blockSubscriptionHandler(headersChan)

	// Mark the notifier as active.
	atomic.StoreInt32(&e.active, 1)

	log.Debug("Electrum notifier started successfully")

	return nil
}

// Stop shuts down the ElectrumNotifier.
func (e *ElectrumNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&e.stopped, 1) != 1 {
		return nil
	}

	log.Info("Electrum notifier shutting down...")
	defer log.Debug("Electrum notifier shutdown complete")

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
func (e *ElectrumNotifier) Started() bool {
	return atomic.LoadInt32(&e.active) != 0
}

// blockSubscriptionHandler handles incoming block header notifications from
// the Electrum server.
func (e *ElectrumNotifier) blockSubscriptionHandler(
	headersChan <-chan *electrum.SubscribeHeadersResult) {

	defer e.wg.Done()

	for {
		select {
		case headerResult, ok := <-headersChan:
			if !ok {
				log.Warn("Headers subscription channel closed")
				return
			}

			if headerResult == nil {
				continue
			}

			blockHeader, err := parseBlockHeader(headerResult.Hex)
			if err != nil {
				log.Errorf("Failed to parse block header: %v",
					err)
				continue
			}

			blockHash := blockHeader.BlockHash()
			newHeight := int32(headerResult.Height)

			// Check if this is a new block or a reorg.
			e.bestBlockMtx.RLock()
			prevHeight := e.bestBlock.Height
			prevHash := e.bestBlock.Hash
			e.bestBlockMtx.RUnlock()

			// Handle the new block.
			if newHeight > prevHeight {
				// New block connected.
				e.handleBlockConnected(
					newHeight, &blockHash, blockHeader,
				)
			} else if newHeight <= prevHeight &&
				!blockHash.IsEqual(prevHash) {

				// Potential reorg detected.
				log.Warnf("Potential reorg detected: "+
					"prev_height=%d, new_height=%d",
					prevHeight, newHeight)

				e.handleReorg(prevHeight, newHeight, &blockHash,
					blockHeader)
			}

		case <-e.quit:
			return
		}
	}
}

// handleBlockConnected processes a newly connected block.
func (e *ElectrumNotifier) handleBlockConnected(height int32,
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

	// Update the txNotifier's height. Since we don't have full block data
	// from Electrum, we use NotifyHeight instead of ConnectTip.
	if e.txNotifier != nil {
		// First update the height so currentHeight is correct when we
		// check for pending confirmations/spends.
		err := e.txNotifier.NotifyHeight(uint32(height))
		if err != nil {
			log.Errorf("Failed to notify height: %v", err)
		}

		// Check pending confirmations and spends in parallel AFTER
		// notifying height. This ensures currentHeight is updated so
		// UpdateConfDetails/UpdateSpendDetails can properly dispatch.
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

// checkPendingConfirmations queries the Electrum server to check if any
// pending confirmation requests have been satisfied.
func (e *ElectrumNotifier) checkPendingConfirmations(currentHeight uint32) {
	unconfirmed := e.txNotifier.UnconfirmedRequests()
	if len(unconfirmed) == 0 {
		return
	}

	log.Debugf("Checking %d pending confirmation requests at height %d",
		len(unconfirmed), currentHeight)

	for _, confRequest := range unconfirmed {
		// Try to get confirmation details for this request.
		confDetails, err := e.historicalConfDetails(
			confRequest, 0, currentHeight,
		)
		if err != nil {
			log.Debugf("Error checking confirmation for %v: %v",
				confRequest, err)
			continue
		}

		if confDetails == nil {
			// Still unconfirmed.
			continue
		}

		log.Infof("Found confirmation for pending request %v at "+
			"height %d", confRequest, confDetails.BlockHeight)

		// Update the txNotifier with the confirmation details.
		err = e.txNotifier.UpdateConfDetails(confRequest, confDetails)
		if err != nil {
			log.Errorf("Failed to update conf details for %v: %v",
				confRequest, err)
		}
	}
}

// checkPendingSpends queries the Electrum server to check if any pending
// spend requests have been satisfied. This is critical for proper channel
// close detection.
func (e *ElectrumNotifier) checkPendingSpends(currentHeight uint32) {
	unspent := e.txNotifier.UnspentRequests()
	if len(unspent) == 0 {
		return
	}

	log.Debugf("Checking %d pending spend requests at height %d",
		len(unspent), currentHeight)

	for _, spendRequest := range unspent {
		// Try to get spend details for this request.
		spendDetails, err := e.historicalSpendDetails(
			spendRequest, 0, currentHeight,
		)
		if err != nil {
			log.Debugf("Error checking spend for %v: %v",
				spendRequest, err)
			continue
		}

		if spendDetails == nil {
			// Still unspent.
			continue
		}

		log.Infof("Found spend for pending request %v at height %d",
			spendRequest, spendDetails.SpendingHeight)

		// Update the txNotifier with the spend details.
		err = e.txNotifier.UpdateSpendDetails(spendRequest, spendDetails)
		if err != nil {
			log.Errorf("Failed to update spend details for %v: %v",
				spendRequest, err)
		}
	}
}

// handleReorg handles a chain reorganization.
func (e *ElectrumNotifier) handleReorg(prevHeight, newHeight int32,
	newHash *chainhash.Hash, newHeader *wire.BlockHeader) {

	// For reorgs, we need to disconnect blocks and reconnect at the new
	// height. Since we don't have full block data, we do our best by
	// updating the txNotifier.
	if e.txNotifier != nil {
		// Disconnect blocks from prevHeight down to newHeight.
		for h := uint32(prevHeight); h > uint32(newHeight); h-- {
			err := e.txNotifier.DisconnectTip(h)
			if err != nil {
				log.Errorf("Failed to disconnect tip at "+
					"height %d: %v", h, err)
			}
		}
	}

	// Now handle the new block at the reorg height.
	e.handleBlockConnected(newHeight, newHash, newHeader)
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (e *ElectrumNotifier) notificationDispatcher() {
	defer e.wg.Done()

	for {
		select {
		case cancelMsg := <-e.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *epochCancel:
				log.Infof("Cancelling epoch notification, "+
					"epoch_id=%v", msg.epochID)

				// Look up the original registration to stop
				// the active queue goroutine.
				reg := e.blockEpochClients[msg.epochID]
				if reg != nil {
					reg.epochQueue.Stop()

					// Close the cancel channel and wait for
					// the client to exit.
					close(reg.cancelChan)
					reg.wg.Wait()

					// Close the epoch channel to notify
					// listeners.
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

				// If the client specified a best block, check
				// if they're behind the current tip.
				if msg.bestBlock != nil {
					e.dispatchMissedBlocks(msg)
				} else {
					// Send the current best block.
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
func (e *ElectrumNotifier) handleHistoricalConfDispatch(
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
func (e *ElectrumNotifier) handleHistoricalSpendDispatch(
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
func (e *ElectrumNotifier) historicalConfDetails(
	confRequest chainntnfs.ConfRequest,
	startHeight, endHeight uint32) (*chainntnfs.TxConfirmation, error) {

	// If we have a txid, try to get the transaction directly.
	// First, try to get the transaction directly by txid if we have one.
	if confRequest.TxID != chainntnfs.ZeroHash {
		ctx, cancel := context.WithTimeout(
			context.Background(), 30*time.Second,
		)
		defer cancel()

		txResult, err := e.client.GetTransaction(
			ctx, confRequest.TxID.String(),
		)
		if err == nil && txResult != nil && txResult.Confirmations > 0 {
			// Transaction is confirmed.
			blockHash, err := chainhash.NewHashFromStr(
				txResult.Blockhash,
			)
			if err != nil {
				return nil, fmt.Errorf("invalid block hash: %w",
					err)
			}

			// Calculate block height from confirmations.
			e.bestBlockMtx.RLock()
			currentHeight := e.bestBlock.Height
			e.bestBlockMtx.RUnlock()

			blockHeight := uint32(currentHeight) -
				uint32(txResult.Confirmations) + 1

			// Fetch the actual transaction to include in the
			// confirmation details.
			var msgTx *wire.MsgTx
			txHex := txResult.Hex
			if txHex != "" {
				txBytes, decErr := hex.DecodeString(txHex)
				if decErr == nil {
					msgTx = &wire.MsgTx{}
					if parseErr := msgTx.Deserialize(
						bytes.NewReader(txBytes),
					); parseErr != nil {
						log.Debugf("Failed to parse tx: %v",
							parseErr)
						msgTx = nil
					}
				}
			}

			// Try to get the actual TxIndex via REST API if available.
			var txIndex uint32
			if e.restClient != nil {
				txIdx, _, err := e.restClient.GetTxIndexByHeight(
					ctx, int64(blockHeight),
					confRequest.TxID.String(),
				)
				if err != nil {
					log.Debugf("Failed to get TxIndex via REST: %v", err)
				} else {
					txIndex = txIdx
					log.Debugf("Got TxIndex %d for tx %s via REST",
						txIndex, confRequest.TxID)
				}
			}

			return &chainntnfs.TxConfirmation{
				BlockHash:   blockHash,
				BlockHeight: blockHeight,
				TxIndex:     txIndex,
				Tx:          msgTx,
			}, nil
		}

		// If GetTransaction failed or tx is unconfirmed, log and fall
		// through to try scripthash lookup if we have a pkScript.
		if err != nil {
			log.Debugf("GetTransaction for %v failed: %v, trying "+
				"scripthash lookup", confRequest.TxID, err)
		} else {
			log.Debugf("Transaction %v not confirmed yet, trying "+
				"scripthash lookup", confRequest.TxID)
		}
	}

	// If we don't have a pkScript, we can't do scripthash lookup.
	if confRequest.PkScript.Script() == nil ||
		len(confRequest.PkScript.Script()) == 0 {

		return nil, nil
	}

	// Search by scripthash (address history).
	scripthash := electrum.ScripthashFromScript(confRequest.PkScript.Script())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	history, err := e.client.GetHistory(ctx, scripthash)
	if err != nil {
		return nil, fmt.Errorf("failed to get history: %w", err)
	}

	// Search through history for our target transaction.
	targetTxID := confRequest.TxID.String()
	for _, tx := range history {
		if tx.Height <= 0 {
			// Unconfirmed transaction.
			continue
		}

		// If we have a txid, only match that specific transaction.
		// Otherwise, match any confirmed transaction in the range.
		if confRequest.TxID != chainntnfs.ZeroHash {
			if tx.Hash != targetTxID {
				continue
			}
		} else if uint32(tx.Height) < startHeight ||
			uint32(tx.Height) > endHeight {
			continue
		}

		// Get the block header for this height.
		header, err := e.client.GetBlockHeader(
			ctx, uint32(tx.Height),
		)
		if err != nil {
			log.Debugf("Failed to get block header at height %d: %v",
				tx.Height, err)
			continue
		}

		blockHash := header.BlockHash()

		log.Debugf("Found confirmed tx %s at height %d via scripthash",
			tx.Hash, tx.Height)

		// Fetch the actual transaction to include in the confirmation
		// details. This is needed for channel funding validation.
		var msgTx *wire.MsgTx
		txHex, txErr := e.client.GetRawTransaction(ctx, tx.Hash)
		if txErr == nil && txHex != "" {
			txBytes, decErr := hex.DecodeString(txHex)
			if decErr == nil {
				msgTx = &wire.MsgTx{}
				if parseErr := msgTx.Deserialize(
					bytes.NewReader(txBytes),
				); parseErr != nil {
					log.Debugf("Failed to parse tx %s: %v",
						tx.Hash, parseErr)
					msgTx = nil
				}
			}
		} else if txErr != nil {
			log.Debugf("Failed to fetch raw tx %s: %v",
				tx.Hash, txErr)
		}

		// Try to get the actual TxIndex via REST API if available.
		var txIndex uint32
		if e.restClient != nil {
			blockHashStr := blockHash.String()
			txIdx, err := e.restClient.GetTxIndex(
				ctx, blockHashStr, tx.Hash,
			)
			if err != nil {
				log.Debugf("Failed to get TxIndex via REST: %v", err)
			} else {
				txIndex = txIdx
				log.Debugf("Got TxIndex %d for tx %s via REST",
					txIndex, tx.Hash)
			}
		}

		return &chainntnfs.TxConfirmation{
			BlockHash:   &blockHash,
			BlockHeight: uint32(tx.Height),
			TxIndex:     txIndex,
			Tx:          msgTx,
		}, nil
	}

	return nil, nil
}

// historicalSpendDetails looks up the spend details for an outpoint within
// the given height range.
func (e *ElectrumNotifier) historicalSpendDetails(
	spendRequest chainntnfs.SpendRequest,
	startHeight, endHeight uint32) (*chainntnfs.SpendDetail, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// For taproot outputs, the PkScript is ZeroTaprootPkScript because we
	// can't derive the script from the witness. We need to fetch the
	// funding transaction to get the actual output script.
	pkScript := spendRequest.PkScript.Script()
	if spendRequest.PkScript == chainntnfs.ZeroTaprootPkScript {
		// Fetch the funding transaction to get the actual pkScript.
		fundingTx, err := e.client.GetTransactionMsgTx(
			ctx, &spendRequest.OutPoint.Hash,
		)
		if err != nil {
			log.Debugf("Failed to get funding tx for taproot "+
				"spend lookup %v: %v", spendRequest.OutPoint, err)
			return nil, nil
		}

		if int(spendRequest.OutPoint.Index) >= len(fundingTx.TxOut) {
			log.Debugf("Invalid output index %d for funding tx %v",
				spendRequest.OutPoint.Index,
				spendRequest.OutPoint.Hash)
			return nil, nil
		}

		pkScript = fundingTx.TxOut[spendRequest.OutPoint.Index].PkScript
		log.Debugf("Fetched taproot pkScript for %v: %x",
			spendRequest.OutPoint, pkScript)
	}

	// Convert the output script to a scripthash for Electrum queries.
	scripthash := electrum.ScripthashFromScript(pkScript)

	// Get the transaction history for this scripthash.
	history, err := e.client.GetHistory(ctx, scripthash)
	if err != nil {
		return nil, fmt.Errorf("failed to get history: %w", err)
	}

	// Look for a transaction that spends the outpoint.
	for _, histTx := range history {
		if histTx.Height <= 0 {
			// Skip unconfirmed transactions for historical lookups.
			continue
		}

		if uint32(histTx.Height) < startHeight ||
			uint32(histTx.Height) > endHeight {
			continue
		}

		txHash, err := chainhash.NewHashFromStr(histTx.Hash)
		if err != nil {
			continue
		}

		// Get the full transaction to check inputs.
		tx, err := e.client.GetTransactionMsgTx(ctx, txHash)
		if err != nil {
			log.Debugf("Failed to get transaction %s: %v",
				histTx.Hash, err)
			continue
		}

		// Check if this transaction spends our outpoint.
		for inputIdx, txIn := range tx.TxIn {
			if txIn.PreviousOutPoint == spendRequest.OutPoint {
				spenderHash := tx.TxHash()

				return &chainntnfs.SpendDetail{
					SpentOutPoint:     &spendRequest.OutPoint,
					SpenderTxHash:     &spenderHash,
					SpendingTx:        tx,
					SpenderInputIndex: uint32(inputIdx),
					SpendingHeight:    histTx.Height,
				}, nil
			}
		}
	}

	return nil, nil
}

// dispatchMissedBlocks sends block epoch notifications for any blocks that
// the client may have missed.
func (e *ElectrumNotifier) dispatchMissedBlocks(
	registration *blockEpochRegistration) {

	e.bestBlockMtx.RLock()
	currentHeight := e.bestBlock.Height
	e.bestBlockMtx.RUnlock()

	startHeight := registration.bestBlock.Height + 1

	for height := startHeight; height <= currentHeight; height++ {
		ctx, cancel := context.WithTimeout(
			context.Background(), 30*time.Second,
		)

		header, err := e.client.GetBlockHeader(ctx, uint32(height))
		cancel()

		if err != nil {
			log.Errorf("Failed to get block header at height %d: %v",
				height, err)
			continue
		}

		blockHash := header.BlockHash()
		e.notifyBlockEpochClient(registration, height, &blockHash, header)
	}
}

// notifyBlockEpochClient sends a block epoch notification to a specific client.
func (e *ElectrumNotifier) notifyBlockEpochClient(
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
func (e *ElectrumNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	// Register the conf notification with the TxNotifier.
	ntfn, err := e.txNotifier.RegisterConf(
		txid, pkScript, numConfs, heightHint, opts...,
	)
	if err != nil {
		return nil, err
	}

	// If we need to perform a historical scan, dispatch it.
	if ntfn.HistoricalDispatch != nil {
		e.wg.Add(1)
		go e.handleHistoricalConfDispatch(ntfn.HistoricalDispatch)
	}

	return ntfn.Event, nil
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint/output script has been spent by a transaction on-chain.
func (e *ElectrumNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// Register the spend notification with the TxNotifier.
	ntfn, err := e.txNotifier.RegisterSpend(outpoint, pkScript, heightHint)
	if err != nil {
		return nil, err
	}

	// If we need to perform a historical scan, dispatch it.
	if ntfn.HistoricalDispatch != nil {
		e.wg.Add(1)
		go e.handleHistoricalSpendDispatch(ntfn.HistoricalDispatch)
	}

	return ntfn.Event, nil
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications of each new block connected to the main
// chain.
func (e *ElectrumNotifier) RegisterBlockEpochNtfn(
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

	// Start a goroutine to forward epochs from the queue to the channel.
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
		return nil, ErrElectrumNotifierShuttingDown
	}
}

// GetBlock attempts to retrieve a block from the cache or the Electrum server.
// NOTE: Electrum servers do not serve full blocks, so this will return an
// error. This method is provided for interface compatibility.
func (e *ElectrumNotifier) GetBlock(hash chainhash.Hash) (*btcutil.Block,
	error) {

	return nil, errors.New("electrum backend does not support full block " +
		"retrieval")
}

// filteredBlock represents a block with optional transaction data.
type filteredBlock struct {
	header  *wire.BlockHeader
	hash    chainhash.Hash
	height  uint32
	txns    []*btcutil.Tx
	connect bool
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
