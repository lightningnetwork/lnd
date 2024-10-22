package chainview

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lntypes"
)

// CfFilteredChainView is an implementation of the FilteredChainView interface
// which is supported by an underlying Bitcoin light client which supports
// client side filtering of Golomb Coded Sets. Rather than fetching all the
// blocks, the light client is able to query filters locally, to test if an
// item in a block modifies any of our watched set of UTXOs.
type CfFilteredChainView struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// p2pNode is a pointer to the running GCS-filter supported Bitcoin
	// light clientl
	p2pNode *neutrino.ChainService

	// chainView is the active rescan which only watches our specified
	// sub-set of the UTXO set.
	chainView *neutrino.Rescan

	// rescanErrChan is the channel that any errors encountered during the
	// rescan will be sent over.
	rescanErrChan <-chan error

	// blockEventQueue is the ordered queue used to keep the order
	// of connected and disconnected blocks sent to the reader of the
	// chainView.
	blockQueue *blockEventQueue

	// blockCache is an LRU block cache.
	blockCache *blockcache.BlockCache

	// chainFilter is the
	filterMtx   sync.RWMutex
	chainFilter map[wire.OutPoint][]byte

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure CfFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*CfFilteredChainView)(nil)

// NewCfFilteredChainView creates a new instance of the CfFilteredChainView
// which is connected to an active neutrino node.
//
// NOTE: The node should already be running and syncing before being passed into
// this function.
func NewCfFilteredChainView(node *neutrino.ChainService,
	blockCache *blockcache.BlockCache) (*CfFilteredChainView, error) {

	return &CfFilteredChainView{
		blockQueue:    newBlockEventQueue(),
		quit:          make(chan struct{}),
		rescanErrChan: make(chan error),
		chainFilter:   make(map[wire.OutPoint][]byte),
		p2pNode:       node,
		blockCache:    blockCache,
	}, nil
}

// Start kicks off the FilteredChainView implementation. This function must be
// called before any calls to UpdateFilter can be processed.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&c.started, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView starting")

	// First, we'll obtain the latest block height of the p2p node. We'll
	// start the auto-rescan from this point. Once a caller actually wishes
	// to register a chain view, the rescan state will be rewound
	// accordingly.
	startingPoint, err := c.p2pNode.BestBlock()
	if err != nil {
		return err
	}

	// Next, we'll create our set of rescan options. Currently it's
	// required that an user MUST set a addr/outpoint/txid when creating a
	// rescan. To get around this, we'll add a "zero" outpoint, that won't
	// actually be matched.
	var zeroPoint neutrino.InputWithScript
	rescanOptions := []neutrino.RescanOption{
		neutrino.StartBlock(startingPoint),
		neutrino.QuitChan(c.quit),
		neutrino.NotificationHandlers(
			rpcclient.NotificationHandlers{
				OnFilteredBlockConnected:    c.onFilteredBlockConnected,
				OnFilteredBlockDisconnected: c.onFilteredBlockDisconnected,
			},
		),
		neutrino.WatchInputs(zeroPoint),
	}

	// Finally, we'll create our rescan struct, start it, and launch all
	// the goroutines we need to operate this FilteredChainView instance.
	c.chainView = neutrino.NewRescan(
		&neutrino.RescanChainSource{
			ChainService: c.p2pNode,
		},
		rescanOptions...,
	)
	c.rescanErrChan = c.chainView.Start()

	c.blockQueue.Start()

	c.wg.Add(1)
	go c.chainFilterer()

	return nil
}

// Stop signals all active goroutines for a graceful shutdown.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) Stop() error {
	log.Debug("CfFilteredChainView stopping")
	defer log.Debug("CfFilteredChainView stopped")

	// Already shutting down?
	if atomic.AddInt32(&c.stopped, 1) != 1 {
		return nil
	}

	close(c.quit)
	c.blockQueue.Stop()
	c.wg.Wait()

	return nil
}

// onFilteredBlockConnected is called for each block that's connected to the
// end of the main chain. Based on our current chain filter, the block may or
// may not include any relevant transactions.
func (c *CfFilteredChainView) onFilteredBlockConnected(height int32,
	header *wire.BlockHeader, txns []*btcutil.Tx) {

	mtxs := make([]*wire.MsgTx, len(txns))
	for i, tx := range txns {
		mtx := tx.MsgTx()
		mtxs[i] = mtx

		for _, txIn := range mtx.TxIn {
			c.filterMtx.Lock()
			delete(c.chainFilter, txIn.PreviousOutPoint)
			c.filterMtx.Unlock()
		}

	}

	block := &FilteredBlock{
		Hash:         header.BlockHash(),
		Height:       uint32(height),
		Transactions: mtxs,
	}

	c.blockQueue.Add(&blockEvent{
		eventType: connected,
		block:     block,
	})
}

// onFilteredBlockDisconnected is a callback which is executed once a block is
// disconnected from the end of the main chain.
func (c *CfFilteredChainView) onFilteredBlockDisconnected(height int32,
	header *wire.BlockHeader) {

	log.Debugf("got disconnected block at height %d: %v", height,
		header.BlockHash())

	filteredBlock := &FilteredBlock{
		Hash:   header.BlockHash(),
		Height: uint32(height),
	}

	c.blockQueue.Add(&blockEvent{
		eventType: disconnected,
		block:     filteredBlock,
	})
}

// chainFilterer is the primary coordination goroutine within the
// CfFilteredChainView. This goroutine handles errors from the running rescan.
func (c *CfFilteredChainView) chainFilterer() {
	defer c.wg.Done()

	for {
		select {
		case err := <-c.rescanErrChan:
			log.Errorf("Error encountered during rescan: %v", err)
		case <-c.quit:
			return
		}
	}
}

// FilterBlock takes a block hash, and returns a FilteredBlocks which is the
// result of applying the current registered UTXO sub-set on the block
// corresponding to that block hash. If any watched UTXO's are spent by the
// selected lock, then the internal chainFilter will also be updated.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) FilterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
	// First, we'll fetch the block header itself so we can obtain the
	// height which is part of our return value.
	blockHeight, err := c.p2pNode.GetBlockHeight(blockHash)
	if err != nil {
		return nil, err
	}

	filteredBlock := &FilteredBlock{
		Hash:   *blockHash,
		Height: uint32(blockHeight),
	}

	// If we don't have any items within our current chain filter, then we
	// can exit early as we don't need to fetch the filter.
	c.filterMtx.RLock()
	if len(c.chainFilter) == 0 {
		c.filterMtx.RUnlock()
		return filteredBlock, nil
	}
	c.filterMtx.RUnlock()

	// Next, using the block, hash, we'll fetch the compact filter for this
	// block. We only require the regular filter as we're just looking for
	// outpoint that have been spent.
	filter, err := c.p2pNode.GetCFilter(*blockHash, wire.GCSFilterRegular)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch filter: %w", err)
	}

	// Before we can match the filter, we'll need to map each item in our
	// chain filter to the representation that included in the compact
	// filters.
	c.filterMtx.RLock()
	relevantPoints := make([][]byte, 0, len(c.chainFilter))
	for _, filterEntry := range c.chainFilter {
		relevantPoints = append(relevantPoints, filterEntry)
	}
	c.filterMtx.RUnlock()

	// With our relevant points constructed, we can finally match against
	// the retrieved filter.
	matched, err := filter.MatchAny(builder.DeriveKey(blockHash),
		relevantPoints)
	if err != nil {
		return nil, err
	}

	// If there wasn't a match, then we'll return the filtered block as is
	// (void of any transactions).
	if !matched {
		return filteredBlock, nil
	}

	// If we reach this point, then there was a match, so we'll need to
	// fetch the block itself so we can scan it for any actual matches (as
	// there's a fp rate).
	block, err := c.GetBlock(*blockHash)
	if err != nil {
		return nil, err
	}

	// Finally, we'll step through the block, input by input, to see if any
	// transactions spend any outputs from our watched sub-set of the UTXO
	// set.
	for _, tx := range block.Transactions() {
		for _, txIn := range tx.MsgTx().TxIn {
			prevOp := txIn.PreviousOutPoint

			c.filterMtx.RLock()
			_, ok := c.chainFilter[prevOp]
			c.filterMtx.RUnlock()

			if ok {
				filteredBlock.Transactions = append(
					filteredBlock.Transactions,
					tx.MsgTx().Copy(),
				)

				c.filterMtx.Lock()
				delete(c.chainFilter, prevOp)
				c.filterMtx.Unlock()

				break
			}
		}
	}

	return filteredBlock, nil
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched.  If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) UpdateFilter(ops []graphdb.EdgePoint,
	updateHeight uint32) error {

	log.Tracef("Updating chain filter with new UTXO's: %v", ops)

	// First, we'll update the current chain view, by adding any new
	// UTXO's, ignoring duplicates in the process.
	c.filterMtx.Lock()
	for _, op := range ops {
		c.chainFilter[op.OutPoint] = op.FundingPkScript
	}
	c.filterMtx.Unlock()

	inputs := make([]neutrino.InputWithScript, len(ops))
	for i, op := range ops {
		inputs[i] = neutrino.InputWithScript{
			PkScript: op.FundingPkScript,
			OutPoint: op.OutPoint,
		}
	}

	// With our internal chain view update, we'll craft a new update to the
	// chainView which includes our new UTXO's, and current update height.
	rescanUpdate := []neutrino.UpdateOption{
		neutrino.AddInputs(inputs...),
		neutrino.Rewind(updateHeight),
		neutrino.DisableDisconnectedNtfns(true),
	}
	err := c.chainView.Update(rescanUpdate...)
	if err != nil {
		return fmt.Errorf("unable to update rescan: %w", err)
	}
	return nil
}

// FilteredBlocks returns the channel that filtered blocks are to be sent over.
// Each time a block is connected to the end of a main chain, and appropriate
// FilteredBlock which contains the transactions which mutate our watched UTXO
// set is to be returned.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return c.blockQueue.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return c.blockQueue.staleBlocks
}

// GetBlock is used to retrieve the block with the given hash. Since the block
// cache used by neutrino will be the same as that used by LND (since it is
// passed to neutrino on initialisation), the neutrino GetBlock method can be
// called directly since it already uses the block cache. However, neutrino
// does not lock the block cache mutex for the given block hash and so that is
// done here.
func (c *CfFilteredChainView) GetBlock(hash chainhash.Hash) (
	*btcutil.Block, error) {

	c.blockCache.HashMutex.Lock(lntypes.Hash(hash))
	defer c.blockCache.HashMutex.Unlock(lntypes.Hash(hash))

	return c.p2pNode.GetBlock(hash)
}
