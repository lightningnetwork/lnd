package chainview

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/esplora"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
)

// EsploraFilteredChainView is an implementation of the FilteredChainView
// interface which is backed by an Esplora HTTP API connection. It uses
// scripthash queries to monitor for spends of watched outputs.
type EsploraFilteredChainView struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// bestHeight is the height of the latest block added to the
	// blockQueue. It is used to determine up to what height we would
	// need to rescan in case of a filter update.
	bestHeightMtx sync.Mutex
	bestHeight    uint32

	// client is the Esplora client used for all API operations.
	client *esplora.Client

	// subscriptionID is the ID of our block notification subscription.
	subscriptionID uint64

	// blockEventQueue is the ordered queue used to keep the order of
	// connected and disconnected blocks sent to the reader of the
	// chainView.
	blockQueue *blockEventQueue

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan esploraFilterUpdate

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

// A compile time check to ensure EsploraFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*EsploraFilteredChainView)(nil)

// esploraFilterUpdate is a message sent to the chainFilterer to update the
// current chainFilter state.
type esploraFilterUpdate struct {
	newUtxos     []graphdb.EdgePoint
	updateHeight uint32
}

// NewEsploraFilteredChainView creates a new instance of the
// EsploraFilteredChainView which is connected to an active Esplora client.
//
// NOTE: The client should already be started and connected before being
// passed into this function.
func NewEsploraFilteredChainView(
	client *esplora.Client) (*EsploraFilteredChainView, error) {

	return &EsploraFilteredChainView{
		client:               client,
		blockQueue:           newBlockEventQueue(),
		filterUpdates:        make(chan esploraFilterUpdate),
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
func (e *EsploraFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&e.started, 1) != 1 {
		return nil
	}

	log.Infof("EsploraFilteredChainView starting")

	// Ensure the Esplora client is connected.
	if !e.client.IsConnected() {
		return fmt.Errorf("esplora client not connected")
	}

	// Get the current best block height.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tipHeight, err := e.client.GetTipHeight(ctx)
	if err != nil {
		return fmt.Errorf("unable to get tip height: %w", err)
	}

	e.bestHeightMtx.Lock()
	e.bestHeight = uint32(tipHeight)
	e.bestHeightMtx.Unlock()

	log.Debugf("EsploraFilteredChainView initial height: %d", tipHeight)

	e.blockQueue.Start()

	// Start the main goroutines.
	e.wg.Add(2)
	go e.blockNotificationHandler()
	go e.chainFilterer()

	return nil
}

// Stop stops all goroutines which we launched by the prior call to the Start
// method.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *EsploraFilteredChainView) Stop() error {
	log.Debug("EsploraFilteredChainView stopping")
	defer log.Debug("EsploraFilteredChainView stopped")

	// Already shutting down?
	if atomic.AddInt32(&e.stopped, 1) != 1 {
		return nil
	}

	e.blockQueue.Stop()

	close(e.quit)
	e.wg.Wait()

	return nil
}

// blockNotificationHandler handles incoming block notifications from
// the Esplora client and dispatches appropriate events.
func (e *EsploraFilteredChainView) blockNotificationHandler() {
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

			e.handleBlockConnected(blockInfo)

		case <-e.quit:
			return
		}
	}
}

// handleBlockConnected processes a new block notification, filters
// for relevant transactions, and dispatches the filtered block event.
func (e *EsploraFilteredChainView) handleBlockConnected(
	blockInfo *esplora.BlockInfo) {

	blockHeight := uint32(blockInfo.Height)

	e.bestHeightMtx.Lock()
	prevBestHeight := e.bestHeight
	e.bestHeightMtx.Unlock()

	// Check for reorg - if the new height is less than or equal to what
	// we've seen, we may have a reorg situation.
	if blockHeight <= prevBestHeight && blockHeight > 0 {
		e.handlePotentialReorg(blockHeight, prevBestHeight)
	}

	// Parse block hash.
	blockHash, err := chainhash.NewHashFromStr(blockInfo.ID)
	if err != nil {
		log.Errorf("Failed to parse block hash %s: %v",
			blockInfo.ID, err)
		return
	}

	// Filter the block for transactions that spend our watched outputs.
	filteredTxns := e.filterBlockTransactions(blockHeight)

	// Update best height.
	e.bestHeightMtx.Lock()
	e.bestHeight = blockHeight
	e.bestHeightMtx.Unlock()

	// Create and dispatch the filtered block.
	filteredBlock := &FilteredBlock{
		Hash:         *blockHash,
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
func (e *EsploraFilteredChainView) handlePotentialReorg(newHeight,
	prevHeight uint32) {

	log.Debugf("Potential reorg detected: new height %d, prev height %d",
		newHeight, prevHeight)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Send disconnected events for blocks from prevHeight down to
	// newHeight.
	for h := prevHeight; h >= newHeight; h-- {
		hashStr, err := e.client.GetBlockHashByHeight(ctx, int64(h))
		if err != nil {
			log.Warnf("Failed to get hash for disconnected "+
				"block %d: %v", h, err)
			continue
		}

		blockHash, err := chainhash.NewHashFromStr(hashStr)
		if err != nil {
			log.Warnf("Failed to parse block hash: %v", err)
			continue
		}

		disconnectedBlock := &FilteredBlock{
			Hash:   *blockHash,
			Height: h,
		}

		e.blockQueue.Add(&blockEvent{
			eventType: disconnected,
			block:     disconnectedBlock,
		})
	}
}

// filterBlockTransactions scans the watched outputs to find any that were
// spent in the given block height.
func (e *EsploraFilteredChainView) filterBlockTransactions(
	blockHeight uint32) []*wire.MsgTx {

	e.filterMtx.RLock()
	if len(e.chainFilter) == 0 {
		e.filterMtx.RUnlock()
		return nil
	}

	// Copy the current filter to avoid holding the lock during API calls.
	watchedOutpoints := make(map[wire.OutPoint][]byte)
	for op, script := range e.chainFilter {
		watchedOutpoints[op] = script
	}
	e.filterMtx.RUnlock()

	var filteredTxns []*wire.MsgTx
	spentOutpoints := make([]wire.OutPoint, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// For each watched outpoint, check if it was spent using the outspend
	// endpoint.
	for outpoint := range watchedOutpoints {
		outSpend, err := e.client.GetTxOutSpend(
			ctx, outpoint.Hash.String(), outpoint.Index,
		)
		if err != nil {
			log.Debugf("Failed to check outspend for %v: %v",
				outpoint, err)
			continue
		}

		if !outSpend.Spent {
			continue
		}

		// Check if the spend is confirmed and at this block height.
		if !outSpend.Status.Confirmed {
			continue
		}

		if uint32(outSpend.Status.BlockHeight) != blockHeight {
			continue
		}

		// Fetch the spending transaction.
		tx, err := e.client.GetRawTransactionMsgTx(ctx, outSpend.TxID)
		if err != nil {
			log.Debugf("Failed to get spending tx %s: %v",
				outSpend.TxID, err)
			continue
		}

		filteredTxns = append(filteredTxns, tx)
		spentOutpoints = append(spentOutpoints, outpoint)
	}

	// Remove spent outpoints from the filter.
	if len(spentOutpoints) > 0 {
		e.filterMtx.Lock()
		for _, op := range spentOutpoints {
			pkScript := e.chainFilter[op]
			delete(e.chainFilter, op)

			// Also remove from scripthash mapping.
			if pkScript != nil {
				sh := esplora.ScripthashFromScript(pkScript)
				delete(e.scripthashToOutpoint, sh)
			}
		}
		e.filterMtx.Unlock()
	}

	return filteredTxns
}

// chainFilterer is the primary goroutine which handles filter updates and
// block filtering requests.
func (e *EsploraFilteredChainView) chainFilterer() {
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
func (e *EsploraFilteredChainView) handleFilterUpdate(
	update esploraFilterUpdate) {

	log.Tracef("Updating chain filter with %d new UTXO's",
		len(update.newUtxos))

	// Add new outpoints to the filter.
	e.filterMtx.Lock()
	for _, edgePoint := range update.newUtxos {
		e.chainFilter[edgePoint.OutPoint] = edgePoint.FundingPkScript

		// Also add to scripthash mapping.
		sh := esplora.ScripthashFromScript(edgePoint.FundingPkScript)
		e.scripthashToOutpoint[sh] = edgePoint.OutPoint
	}
	e.filterMtx.Unlock()

	// Check if we need to rescan for spends we might have missed.
	e.bestHeightMtx.Lock()
	bestHeight := e.bestHeight
	e.bestHeightMtx.Unlock()

	if update.updateHeight < bestHeight {
		log.Debugf("Rescanning for filter update from height %d to %d",
			update.updateHeight, bestHeight)

		ctx, cancel := context.WithTimeout(
			context.Background(), 120*time.Second,
		)
		defer cancel()

		// Check each new outpoint to see if it was already spent.
		for _, edgePoint := range update.newUtxos {
			outSpend, err := e.client.GetTxOutSpend(
				ctx, edgePoint.OutPoint.Hash.String(),
				edgePoint.OutPoint.Index,
			)
			if err != nil {
				log.Debugf("Failed to check outspend: %v", err)
				continue
			}

			if !outSpend.Spent || !outSpend.Status.Confirmed {
				continue
			}

			spendHeight := uint32(outSpend.Status.BlockHeight)
			if spendHeight < update.updateHeight ||
				spendHeight > bestHeight {
				continue
			}

			// Fetch the spending transaction.
			tx, err := e.client.GetRawTransactionMsgTx(
				ctx, outSpend.TxID,
			)
			if err != nil {
				log.Debugf("Failed to get tx: %v", err)
				continue
			}

			// Get the block hash for this height.
			blockHash, err := e.client.GetBlockHashByHeight(
				ctx, int64(spendHeight),
			)
			if err != nil {
				log.Debugf("Failed to get block hash: %v", err)
				continue
			}

			hash, err := chainhash.NewHashFromStr(blockHash)
			if err != nil {
				continue
			}

			// Send a filtered block for this spend.
			filteredBlock := &FilteredBlock{
				Hash:         *hash,
				Height:       spendHeight,
				Transactions: []*wire.MsgTx{tx},
			}

			e.blockQueue.Add(&blockEvent{
				eventType: connected,
				block:     filteredBlock,
			})

			// Remove from filter.
			e.filterMtx.Lock()
			delete(e.chainFilter, edgePoint.OutPoint)
			sh := esplora.ScripthashFromScript(edgePoint.FundingPkScript)
			delete(e.scripthashToOutpoint, sh)
			e.filterMtx.Unlock()
		}
	}
}

// handleFilterBlockReq handles a request to filter a specific block.
func (e *EsploraFilteredChainView) handleFilterBlockReq(req *filterBlockReq) {
	blockHash := req.blockHash

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Get block info to find the height.
	blockInfo, err := e.client.GetBlockInfo(ctx, blockHash.String())
	if err != nil {
		req.err <- fmt.Errorf("failed to get block info: %w", err)
		return
	}

	// Filter transactions at this block height.
	filteredTxns := e.filterBlockTransactions(uint32(blockInfo.Height))

	filteredBlock := &FilteredBlock{
		Hash:         *blockHash,
		Height:       uint32(blockInfo.Height),
		Transactions: filteredTxns,
	}

	req.resp <- filteredBlock
}

// FilterBlock takes a block hash and returns a FilteredBlock with any
// transactions that spend watched outputs.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *EsploraFilteredChainView) FilterBlock(
	blockHash *chainhash.Hash) (*FilteredBlock, error) {

	req := &filterBlockReq{
		blockHash: blockHash,
		resp:      make(chan *FilteredBlock, 1),
		err:       make(chan error, 1),
	}

	select {
	case e.filterBlockReqs <- req:
	case <-e.quit:
		return nil, fmt.Errorf("esplora chain view shutting down")
	}

	select {
	case filteredBlock := <-req.resp:
		return filteredBlock, nil

	case err := <-req.err:
		return nil, err

	case <-e.quit:
		return nil, fmt.Errorf("esplora chain view shutting down")
	}
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *EsploraFilteredChainView) UpdateFilter(ops []graphdb.EdgePoint,
	updateHeight uint32) error {

	select {
	case e.filterUpdates <- esploraFilterUpdate{
		newUtxos:     ops,
		updateHeight: updateHeight,
	}:
		return nil

	case <-e.quit:
		return fmt.Errorf("esplora chain view shutting down")
	}
}

// FilteredBlocks returns the channel that filtered blocks are to be sent
// over. Each time a block is connected to the end of a main chain, and
// passes the filter previously set via UpdateFilter(), a struct over the
// returned channel will be sent.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *EsploraFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return e.blockQueue.newBlocks
}

// DisconnectedBlocks returns the channel that filtered blocks are to be sent
// over. Each time a block is disconnected from the end of the main chain, a
// struct over the returned channel will be sent.
//
// NOTE: This is part of the FilteredChainView interface.
func (e *EsploraFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return e.blockQueue.staleBlocks
}
