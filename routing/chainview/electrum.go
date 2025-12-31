package chainview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
)

// ElectrumClient is the interface that wraps the methods needed from an
// Electrum client for the filtered chain view. This interface allows us to
// avoid import cycles and enables easier testing.
type ElectrumClient interface {
	// IsConnected returns true if the client is currently connected to
	// the Electrum server.
	IsConnected() bool

	// SubscribeHeaders subscribes to new block header notifications and
	// returns a channel that will receive header updates.
	SubscribeHeaders(ctx context.Context) (<-chan *HeaderResult, error)

	// GetBlockHeader retrieves the block header at the given height.
	GetBlockHeader(ctx context.Context, height uint32) (*wire.BlockHeader,
		error)

	// GetHistory retrieves the transaction history for a scripthash.
	GetHistory(ctx context.Context,
		scripthash string) ([]*HistoryResult, error)

	// GetTransactionMsgTx retrieves a transaction and returns it as a
	// wire.MsgTx.
	GetTransactionMsgTx(ctx context.Context,
		txHash *chainhash.Hash) (*wire.MsgTx, error)
}

// HeaderResult represents a block header notification from an Electrum server.
type HeaderResult struct {
	Height int32
}

// HistoryResult represents a transaction in the history of a scripthash.
type HistoryResult struct {
	TxHash string
	Height int32
}

// ElectrumFilteredChainView is an implementation of the FilteredChainView
// interface which is backed by an Electrum server connection. It uses
// scripthash subscriptions to monitor for spends of watched outputs.
type ElectrumFilteredChainView struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// bestHeight is the height of the latest block added to the
	// blockQueue. It is used to determine up to what height we would
	// need to rescan in case of a filter update.
	bestHeightMtx sync.Mutex
	bestHeight    uint32

	// client is the Electrum client used for all RPC operations.
	client ElectrumClient

	// blockEventQueue is the ordered queue used to keep the order of
	// connected and disconnected blocks sent to the reader of the
	// chainView.
	blockQueue *blockEventQueue

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan electrumFilterUpdate

	// chainFilter is the set of utxo's that we're currently watching
	// spends for within the chain. Maps outpoint to funding pkScript.
	filterMtx   sync.RWMutex
	chainFilter map[wire.OutPoint][]byte

	// scripthashToOutpoint maps scripthashes to their corresponding
	// outpoints for efficient lookup when we receive notifications.
	scripthashToOutpoint map[string]wire.OutPoint

	// filterBlockReqs is a channel in which requests to filter select
	// blocks will be sent over.
	filterBlockReqs chan *filterBlockReq

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure ElectrumFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*ElectrumFilteredChainView)(nil)

// electrumFilterUpdate is a message sent to the chainFilterer to update the
// current chainFilter state. Unlike the btcd version, this includes the full
// EdgePoint with pkScript for scripthash conversion.
type electrumFilterUpdate struct {
	newUtxos     []graphdb.EdgePoint
	updateHeight uint32
}

// NewElectrumFilteredChainView creates a new instance of the
// ElectrumFilteredChainView which is connected to an active Electrum client.
//
// NOTE: The client should already be started and connected before being
// passed into this function.
func NewElectrumFilteredChainView(
	client ElectrumClient) (*ElectrumFilteredChainView, error) {

	return &ElectrumFilteredChainView{
		client:               client,
		blockQueue:           newBlockEventQueue(),
		filterUpdates:        make(chan electrumFilterUpdate),
		chainFilter:          make(map[wire.OutPoint][]byte),
		scripthashToOutpoint: make(map[string]wire.OutPoint),
		filterBlockReqs:      make(chan *filterBlockReq),
		quit:                 make(chan struct{}),
	}, nil
}

// Start kicks off the FilteredChainView implementation. This function must be
// called before any calls to UpdateFilter can be processed.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *ElectrumFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&e.started, 1) != 1 {
		return nil
	}

	log.Infof("ElectrumFilteredChainView starting")

	// Ensure the Electrum client is connected.
	if !e.client.IsConnected() {
		return fmt.Errorf("electrum client not connected")
	}

	// Get the current best block height.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	headerChan, err := e.client.SubscribeHeaders(ctx)
	if err != nil {
		return fmt.Errorf("unable to subscribe to headers: %w", err)
	}

	// Get the initial header to set best height.
	select {
	case header := <-headerChan:
		e.bestHeightMtx.Lock()
		e.bestHeight = uint32(header.Height)
		e.bestHeightMtx.Unlock()

		log.Debugf("ElectrumFilteredChainView initial height: %d",
			header.Height)

	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout waiting for initial header")

	case <-e.quit:
		return fmt.Errorf("chain view shutting down")
	}

	e.blockQueue.Start()

	// Start the main goroutines.
	e.wg.Add(2)
	go e.blockSubscriptionHandler(headerChan)
	go e.chainFilterer()

	return nil
}

// Stop stops all goroutines which we launched by the prior call to the Start
// method.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *ElectrumFilteredChainView) Stop() error {
	log.Debug("ElectrumFilteredChainView stopping")
	defer log.Debug("ElectrumFilteredChainView stopped")

	// Already shutting down?
	if atomic.AddInt32(&e.stopped, 1) != 1 {
		return nil
	}

	e.blockQueue.Stop()

	close(e.quit)
	e.wg.Wait()

	return nil
}

// blockSubscriptionHandler handles incoming block header notifications from
// the Electrum server and dispatches appropriate events.
func (e *ElectrumFilteredChainView) blockSubscriptionHandler(
	headerChan <-chan *HeaderResult) {

	defer e.wg.Done()

	for {
		select {
		case header, ok := <-headerChan:
			if !ok {
				log.Warn("Header subscription channel closed")
				return
			}

			e.handleBlockConnected(header)

		case <-e.quit:
			return
		}
	}
}

// handleBlockConnected processes a new block header notification, filters
// for relevant transactions, and dispatches the filtered block event.
func (e *ElectrumFilteredChainView) handleBlockConnected(
	header *HeaderResult) {

	blockHeight := uint32(header.Height)

	e.bestHeightMtx.Lock()
	prevBestHeight := e.bestHeight
	e.bestHeightMtx.Unlock()

	// Check for reorg - if the new height is less than or equal to what
	// we've seen, we may have a reorg situation.
	if blockHeight <= prevBestHeight && blockHeight > 0 {
		e.handlePotentialReorg(blockHeight, prevBestHeight)
	}

	// Get the block header to retrieve the hash.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	blockHeader, err := e.client.GetBlockHeader(ctx, blockHeight)
	if err != nil {
		log.Errorf("Failed to get block header at height %d: %v",
			blockHeight, err)
		return
	}

	blockHash := blockHeader.BlockHash()

	// Filter the block for transactions that spend our watched outputs.
	filteredTxns := e.filterBlockTransactions(blockHeight)

	// Update best height.
	e.bestHeightMtx.Lock()
	e.bestHeight = blockHeight
	e.bestHeightMtx.Unlock()

	// Create and dispatch the filtered block.
	filteredBlock := &FilteredBlock{
		Hash:         blockHash,
		Height:       blockHeight,
		Transactions: filteredTxns,
	}

	e.blockQueue.Add(&blockEvent{
		eventType: connected,
		block:     filteredBlock,
	})
}

// handlePotentialReorg handles potential chain reorganizations by sending
// disconnected block events for blocks that are no longer on the main chain.
func (e *ElectrumFilteredChainView) handlePotentialReorg(newHeight,
	prevHeight uint32) {

	log.Debugf("Potential reorg detected: new height %d, prev height %d",
		newHeight, prevHeight)

	// Send disconnected events for blocks from prevHeight down to
	// newHeight.
	for h := prevHeight; h >= newHeight; h-- {
		ctx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		blockHeader, err := e.client.GetBlockHeader(ctx, h)
		cancel()

		if err != nil {
			log.Warnf("Failed to get header for disconnected "+
				"block %d: %v", h, err)
			continue
		}

		blockHash := blockHeader.BlockHash()
		disconnectedBlock := &FilteredBlock{
			Hash:   blockHash,
			Height: h,
		}

		e.blockQueue.Add(&blockEvent{
			eventType: disconnected,
			block:     disconnectedBlock,
		})
	}
}

// scripthashFromScript converts a pkScript (output script) to an Electrum
// scripthash. The scripthash is the SHA256 hash of the script with the bytes
// reversed (displayed in little-endian order).
func scripthashFromScript(pkScript []byte) string {
	hash := sha256.Sum256(pkScript)

	// Reverse the hash bytes for Electrum's format.
	reversed := make([]byte, len(hash))
	for i := 0; i < len(hash); i++ {
		reversed[i] = hash[len(hash)-1-i]
	}

	return hex.EncodeToString(reversed)
}

// filterBlockTransactions scans the watched outputs to find any that were
// spent in the given block height.
func (e *ElectrumFilteredChainView) filterBlockTransactions(
	blockHeight uint32) []*wire.MsgTx {

	e.filterMtx.RLock()
	if len(e.chainFilter) == 0 {
		e.filterMtx.RUnlock()
		return nil
	}

	// Copy the current filter to avoid holding the lock during RPC calls.
	watchedOutpoints := make(map[wire.OutPoint][]byte)
	for op, script := range e.chainFilter {
		watchedOutpoints[op] = script
	}
	e.filterMtx.RUnlock()

	var filteredTxns []*wire.MsgTx
	spentOutpoints := make([]wire.OutPoint, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// For each watched outpoint, check if it was spent.
	for outpoint, pkScript := range watchedOutpoints {
		scripthash := scripthashFromScript(pkScript)

		// Get the history for this scripthash.
		history, err := e.client.GetHistory(ctx, scripthash)
		if err != nil {
			log.Warnf("Failed to get history for scripthash: %v",
				err)
			continue
		}

		// Look for transactions that might spend our outpoint.
		for _, histItem := range history {
			// Skip unconfirmed transactions.
			if histItem.Height <= 0 {
				continue
			}

			// Only check transactions at or before this block.
			if uint32(histItem.Height) > blockHeight {
				continue
			}

			// Fetch and check the transaction.
			txHash, err := chainhash.NewHashFromStr(histItem.TxHash)
			if err != nil {
				continue
			}

			tx, err := e.client.GetTransactionMsgTx(ctx, txHash)
			if err != nil {
				log.Debugf("Failed to get tx %s: %v",
					histItem.TxHash, err)
				continue
			}

			// Check if this transaction spends our outpoint.
			for _, txIn := range tx.TxIn {
				if txIn.PreviousOutPoint == outpoint {
					filteredTxns = append(
						filteredTxns, tx.Copy(),
					)
					spentOutpoints = append(
						spentOutpoints, outpoint,
					)
					break
				}
			}
		}
	}

	// Remove spent outpoints from the filter.
	if len(spentOutpoints) > 0 {
		e.filterMtx.Lock()
		for _, op := range spentOutpoints {
			delete(e.chainFilter, op)

			// Also remove from scripthash mapping.
			for sh, mappedOp := range e.scripthashToOutpoint {
				if mappedOp == op {
					delete(e.scripthashToOutpoint, sh)
					break
				}
			}
		}
		e.filterMtx.Unlock()
	}

	return filteredTxns
}

// chainFilterer is the primary goroutine which handles filter updates and
// block filtering requests.
func (e *ElectrumFilteredChainView) chainFilterer() {
	defer e.wg.Done()

	for {
		select {
		case update := <-e.filterUpdates:
			e.handleFilterUpdate(update)

		case req := <-e.filterBlockReqs:
			e.handleFilterBlockReq(req)

		case <-e.quit:
			return
		}
	}
}

// handleFilterUpdate processes a filter update by adding new outpoints to
// watch and rescanning if necessary.
func (e *ElectrumFilteredChainView) handleFilterUpdate(
	update electrumFilterUpdate) {

	log.Tracef("Updating chain filter with %d new UTXO's",
		len(update.newUtxos))

	// Add new outpoints to the filter.
	e.filterMtx.Lock()
	for _, op := range update.newUtxos {
		e.chainFilter[op.OutPoint] = op.FundingPkScript

		// Add to scripthash mapping for efficient lookup.
		scripthash := scripthashFromScript(op.FundingPkScript)
		e.scripthashToOutpoint[scripthash] = op.OutPoint
	}
	e.filterMtx.Unlock()

	// Get the current best height.
	e.bestHeightMtx.Lock()
	bestHeight := e.bestHeight
	e.bestHeightMtx.Unlock()

	// If the update height matches our best known height, no rescan is
	// needed.
	if update.updateHeight >= bestHeight {
		return
	}

	// Rescan blocks from updateHeight+1 to bestHeight.
	log.Debugf("Rescanning blocks from %d to %d",
		update.updateHeight+1, bestHeight)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for height := update.updateHeight + 1; height <= bestHeight; height++ {
		// Get the block header for this height.
		blockHeader, err := e.client.GetBlockHeader(ctx, height)
		if err != nil {
			log.Warnf("Failed to get block header at height %d: %v",
				height, err)
			continue
		}

		blockHash := blockHeader.BlockHash()

		// Filter the block.
		filteredTxns := e.filterBlockTransactions(height)

		// Dispatch the filtered block.
		filteredBlock := &FilteredBlock{
			Hash:         blockHash,
			Height:       height,
			Transactions: filteredTxns,
		}

		e.blockQueue.Add(&blockEvent{
			eventType: connected,
			block:     filteredBlock,
		})
	}
}

// handleFilterBlockReq processes a request to filter a specific block.
func (e *ElectrumFilteredChainView) handleFilterBlockReq(req *filterBlockReq) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get the block height from the hash. Electrum doesn't have a direct
	// method, so we need to look it up through the block headers.
	blockHeight, err := e.getBlockHeightByHash(ctx, req.blockHash)
	if err != nil {
		req.err <- fmt.Errorf("failed to get block height: %w", err)
		req.resp <- nil
		return
	}

	// Filter the block for relevant transactions.
	filteredTxns := e.filterBlockTransactions(blockHeight)

	req.resp <- &FilteredBlock{
		Hash:         *req.blockHash,
		Height:       blockHeight,
		Transactions: filteredTxns,
	}
	req.err <- nil
}

// getBlockHeightByHash retrieves the height of a block given its hash. This
// requires searching through recent blocks since Electrum doesn't have a
// direct hash-to-height lookup.
func (e *ElectrumFilteredChainView) getBlockHeightByHash(ctx context.Context,
	blockHash *chainhash.Hash) (uint32, error) {

	e.bestHeightMtx.Lock()
	currentHeight := e.bestHeight
	e.bestHeightMtx.Unlock()

	// Search backwards from the current height. We limit the search to
	// avoid excessive queries.
	const maxSearchDepth = 1000

	startHeight := uint32(0)
	if currentHeight > maxSearchDepth {
		startHeight = currentHeight - maxSearchDepth
	}

	for height := currentHeight; height >= startHeight; height-- {
		header, err := e.client.GetBlockHeader(ctx, height)
		if err != nil {
			continue
		}

		hash := header.BlockHash()
		if hash.IsEqual(blockHash) {
			return height, nil
		}

		// Avoid infinite loop.
		if height == 0 {
			break
		}
	}

	return 0, fmt.Errorf("block hash %s not found in recent %d blocks",
		blockHash.String(), maxSearchDepth)
}

// FilterBlock takes a block hash, and returns a FilteredBlocks which is the
// result of applying the current registered UTXO sub-set on the block
// corresponding to that block hash. If any watched UTXO's are spent by the
// selected block, then the internal chainFilter will also be updated.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *ElectrumFilteredChainView) FilterBlock(
	blockHash *chainhash.Hash) (*FilteredBlock, error) {

	req := &filterBlockReq{
		blockHash: blockHash,
		resp:      make(chan *FilteredBlock, 1),
		err:       make(chan error, 1),
	}

	select {
	case e.filterBlockReqs <- req:
	case <-e.quit:
		return nil, fmt.Errorf("chain view shutting down")
	}

	select {
	case resp := <-req.resp:
		err := <-req.err
		return resp, err

	case <-e.quit:
		return nil, fmt.Errorf("chain view shutting down")
	}
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched. If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *ElectrumFilteredChainView) UpdateFilter(ops []graphdb.EdgePoint,
	updateHeight uint32) error {

	select {
	case e.filterUpdates <- electrumFilterUpdate{
		newUtxos:     ops,
		updateHeight: updateHeight,
	}:
		return nil

	case <-e.quit:
		return fmt.Errorf("chain filter shutting down")
	}
}

// FilteredBlocks returns the channel that filtered blocks are to be sent over.
// Each time a block is connected to the end of a main chain, and appropriate
// FilteredBlock which contains the transactions which mutate our watched UTXO
// set is to be returned.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *ElectrumFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return e.blockQueue.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *ElectrumFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return e.blockQueue.staleBlocks
}
