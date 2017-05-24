package chainview

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/waddrmgr"
	"github.com/lightninglabs/neutrino"
)

// CfFilteredChainView is an implementation of the FilteredChainView interface
// which is supported by an underlying Bitcoin light client which supports
// client side filtering of Golomb Coded Sets. Rather than fetching all the
// blocks, the light client is able to query fitlers locally, to test if an
// item in  ablock modifies any of our watched set of UTXOs/
type CfFilteredChainView struct {
	started int32
	stopped int32

	// p2pNode is a pointer to the running GCS-filter supported Bitcoin
	// light clientl
	p2pNode *neutrino.ChainService

	// chainView is the active rescan which only watches our specified
	// sub-set of the UTXO set.
	chainView neutrino.Rescan

	// rescanErrChan is the channel that any errors encountered during the
	// rescan will be sent over.
	rescanErrChan <-chan error

	// newBlocks is the channel in which new filtered blocks are sent over.
	newBlocks chan *FilteredBlock

	// staleBlocks is the channel in which blocks that have been
	// disconnected from the mainchain are sent over.
	staleBlocks chan *FilteredBlock

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan filterUpdate

	// chainFilter is the
	filterMtx   sync.RWMutex
	chainFilter map[wire.OutPoint]struct{}

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure CfFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*CfFilteredChainView)(nil)

// NewCfFilteredChainView creates a new instance of the CfFilteredChainView
// which is connected to an active neutrino node.
//
// NOTE: The node should already be running an syncing before being passed into
// this function.
func NewCfFilteredChainView(node *neutrino.ChainService) (*CfFilteredChainView, error) {
	return &CfFilteredChainView{
		newBlocks:     make(chan *FilteredBlock),
		staleBlocks:   make(chan *FilteredBlock),
		quit:          make(chan struct{}),
		rescanErrChan: make(chan error),
		filterUpdates: make(chan filterUpdate),
		chainFilter:   make(map[wire.OutPoint]struct{}),
		p2pNode:       node,
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
	bestHeader, bestHeight, err := c.p2pNode.LatestBlock()
	if err != nil {
		return err
	}
	startingPoint := &waddrmgr.BlockStamp{
		Height: int32(bestHeight),
		Hash:   bestHeader.BlockHash(),
	}

	// Next, we'll create our set of rescan options. Currently it's
	// required that a user MUST set a addr/outpoint/txid when creating a
	// rescan. To get around this, we'll add a "zero" outpoint, that won't
	// actually be matched.
	var zeroPoint wire.OutPoint
	rescanOptions := []neutrino.RescanOption{
		neutrino.StartBlock(startingPoint),
		neutrino.QuitChan(c.quit),
		neutrino.NotificationHandlers(
			btcrpcclient.NotificationHandlers{
				OnFilteredBlockConnected:    c.onFilteredBlockConnected,
				OnFilteredBlockDisconnected: c.onFilteredBlockDisconnected,
			},
		),
		neutrino.WatchOutPoints(zeroPoint),
	}

	// Finally, we'll create our rescan struct, start it, and launch all
	// the goroutines we need to operate this FilteredChainView instance.
	c.chainView = c.p2pNode.NewRescan(rescanOptions...)
	c.rescanErrChan = c.chainView.Start()

	c.wg.Add(1)
	go c.chainFilterer()

	return nil
}

// Stop signals all active goroutines for a graceful shutdown.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&c.stopped, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView stopping")

	close(c.quit)
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

	go func() {
		c.newBlocks <- &FilteredBlock{
			Hash:         header.BlockHash(),
			Height:       uint32(height),
			Transactions: mtxs,
		}
	}()
}

// onFilteredBlockDisconnected is a callback which is executed once a block is
// disconnected from the end of the main chain.
func (c *CfFilteredChainView) onFilteredBlockDisconnected(height int32,
	header *wire.BlockHeader) {

	go func() {
		c.staleBlocks <- &FilteredBlock{
			Hash:   header.BlockHash(),
			Height: uint32(height),
		}
	}()
}

// chainFilterer is the primary coordination goroutine within the
// CfFilteredChainView. This goroutine handles errors from the running rescan,
// and also filter updates.
func (c *CfFilteredChainView) chainFilterer() {
	defer c.wg.Done()

	for {
		select {

		case err := <-c.rescanErrChan:
			log.Errorf("Error encountered during rescan: %v", err)

		// We've received a new update to the filter from the caller to
		// mutate their established chain view.
		case update := <-c.filterUpdates:
			log.Debugf("Updating chain filter with new UTXO's: %v",
				update.newUtxos)

			// First, we'll update the current chain view, by
			// adding any new UTXO's, ignoring duplicates int he
			// process.
			c.filterMtx.Lock()
			for _, op := range update.newUtxos {
				c.chainFilter[op] = struct{}{}
			}
			c.filterMtx.Unlock()

			// With our internal chain view update, we'll craft a
			// new update to the chainView which includes our new
			// UTXO's, and current update height.
			rescanUpdate := []neutrino.UpdateOption{
				neutrino.AddOutPoints(update.newUtxos...),
				neutrino.Rewind(update.updateHeight),
			}
			err := c.chainView.Update(rescanUpdate...)
			if err != nil {
				log.Errorf("unable to update rescan: %v", err)
			}

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
	// As we need to manually filter a block, we'll first fetch the block
	// form the network.
	block, err := c.p2pNode.GetBlockFromNetwork(*blockHash)
	if err != nil {
		return nil, err
	}

	// Along with the block, we'll also need the height of the block in
	// order to complete the notification.
	_, blockHeight, err := c.p2pNode.GetBlockByHash(*blockHash)
	if err != nil {
		return nil, err
	}

	// Finally, we'll step through the block, input by input, to see if any
	// transactions spend any outputs from our watched sub-set of the UTXO
	// set.
	var filteredTxns []*wire.MsgTx
	for _, tx := range block.Transactions() {
		for _, txIn := range tx.MsgTx().TxIn {
			prevOp := txIn.PreviousOutPoint
			if _, ok := c.chainFilter[prevOp]; ok {
				filteredTxns = append(filteredTxns, tx.MsgTx())

				c.filterMtx.Lock()
				delete(c.chainFilter, prevOp)
				c.filterMtx.Unlock()

				break
			}
		}
	}

	return &FilteredBlock{
		Hash:         *blockHash,
		Height:       blockHeight,
		Transactions: filteredTxns,
	}, nil
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched.  If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) UpdateFilter(ops []wire.OutPoint, updateHeight uint32) error {
	select {

	case c.filterUpdates <- filterUpdate{
		newUtxos:     ops,
		updateHeight: updateHeight,
	}:
		return nil

	case <-c.quit:
		return fmt.Errorf("chain filter shutting down")
	}
}

// FilteredBlocks returns the channel that filtered blocks are to be sent over.
// Each time a block is connected to the end of a main chain, and appropriate
// FilteredBlock which contains the transactions which mutate our watched UTXO
// set is to be returned.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return c.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *CfFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return c.staleBlocks
}
