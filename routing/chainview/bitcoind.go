package chainview

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roasbeef/btcd/btcjson"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcwallet/wtxmgr"
)

// BitcoindFilteredChainView is an implementation of the FilteredChainView
// interface which is backed by bitcoind.
type BitcoindFilteredChainView struct {
	started int32
	stopped int32

	// bestHeight is the height of the latest block added to the
	// blockQueue from the onFilteredConnectedMethod. It is used to
	// determine up to what height we would need to rescan in case
	// of a filter update.
	bestHeightMtx sync.Mutex
	bestHeight    uint32

	// TODO: Factor out common logic between bitcoind and btcd into a
	// NodeFilteredView interface.
	chainClient *chain.BitcoindClient

	// blockEventQueue is the ordered queue used to keep the order
	// of connected and disconnected blocks sent to the reader of the
	// chainView.
	blockQueue *blockEventQueue

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan filterUpdate

	// chainFilter is the set of utox's that we're currently watching
	// spends for within the chain.
	filterMtx   sync.RWMutex
	chainFilter map[wire.OutPoint]struct{}

	// filterBlockReqs is a channel in which requests to filter select
	// blocks will be sent over.
	filterBlockReqs chan *filterBlockReq

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure BitcoindFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*BitcoindFilteredChainView)(nil)

// NewBitcoindFilteredChainView creates a new instance of a FilteredChainView
// from RPC credentials and a ZMQ socket address for a bitcoind instance.
func NewBitcoindFilteredChainView(config rpcclient.ConnConfig,
	zmqConnect string, params chaincfg.Params) (*BitcoindFilteredChainView,
	error) {
	chainView := &BitcoindFilteredChainView{
		chainFilter:     make(map[wire.OutPoint]struct{}),
		filterUpdates:   make(chan filterUpdate),
		filterBlockReqs: make(chan *filterBlockReq),
		quit:            make(chan struct{}),
	}

	chainConn, err := chain.NewBitcoindClient(&params, config.Host,
		config.User, config.Pass, zmqConnect, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	chainView.chainClient = chainConn

	chainView.blockQueue = newBlockEventQueue()

	return chainView, nil
}

// Start starts all goroutines necessary for normal operation.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BitcoindFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView starting")

	err := b.chainClient.Start()
	if err != nil {
		return err
	}

	err = b.chainClient.NotifyBlocks()
	if err != nil {
		return err
	}

	_, bestHeight, err := b.chainClient.GetBestBlock()
	if err != nil {
		return err
	}

	b.bestHeightMtx.Lock()
	b.bestHeight = uint32(bestHeight)
	b.bestHeightMtx.Unlock()

	b.blockQueue.Start()

	b.wg.Add(1)
	go b.chainFilterer()

	return nil
}

// Stop stops all goroutines which we launched by the prior call to the Start
// method.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BitcoindFilteredChainView) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from bitcoind's
	// zmq socket, and cleans up all related resources.
	b.chainClient.Stop()

	b.blockQueue.Stop()

	log.Infof("FilteredChainView stopping")

	close(b.quit)
	b.wg.Wait()

	return nil
}

// onFilteredBlockConnected is called for each block that's connected to the
// end of the main chain. Based on our current chain filter, the block may or
// may not include any relevant transactions.
func (b *BitcoindFilteredChainView) onFilteredBlockConnected(height int32,
	hash chainhash.Hash, txns []*wtxmgr.TxRecord) {

	mtxs := make([]*wire.MsgTx, len(txns))
	for i, tx := range txns {
		mtxs[i] = &tx.MsgTx

		for _, txIn := range mtxs[i].TxIn {
			// We can delete this outpoint from the chainFilter, as
			// we just received a block where it was spent. In case
			// of a reorg, this outpoint might get "un-spent", but
			// that's okay since it would never be wise to consider
			// the channel open again (since a spending transaction
			// exists on the network).
			b.filterMtx.Lock()
			delete(b.chainFilter, txIn.PreviousOutPoint)
			b.filterMtx.Unlock()
		}

	}

	// We record the height of the last connected block added to the
	// blockQueue such that we can scan up to this height in case of
	// a rescan. It must be protected by a mutex since a filter update
	// might be trying to read it concurrently.
	b.bestHeightMtx.Lock()
	b.bestHeight = uint32(height)
	b.bestHeightMtx.Unlock()

	block := &FilteredBlock{
		Hash:         hash,
		Height:       uint32(height),
		Transactions: mtxs,
	}

	b.blockQueue.Add(&blockEvent{
		eventType: connected,
		block:     block,
	})
}

// onFilteredBlockDisconnected is a callback which is executed once a block is
// disconnected from the end of the main chain.
func (b *BitcoindFilteredChainView) onFilteredBlockDisconnected(height int32,
	hash chainhash.Hash) {

	log.Debugf("got disconnected block at height %d: %v", height,
		hash)

	filteredBlock := &FilteredBlock{
		Hash:   hash,
		Height: uint32(height),
	}

	b.blockQueue.Add(&blockEvent{
		eventType: disconnected,
		block:     filteredBlock,
	})
}

// FilterBlock takes a block hash, and returns a FilteredBlocks which is the
// result of applying the current registered UTXO sub-set on the block
// corresponding to that block hash. If any watched UTOX's are spent by the
// selected lock, then the internal chainFilter will also be updated.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BitcoindFilteredChainView) FilterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
	req := &filterBlockReq{
		blockHash: blockHash,
		resp:      make(chan *FilteredBlock, 1),
		err:       make(chan error, 1),
	}

	select {
	case b.filterBlockReqs <- req:
	case <-b.quit:
		return nil, fmt.Errorf("FilteredChainView shutting down")
	}

	return <-req.resp, <-req.err
}

// chainFilterer is the primary goroutine which: listens for new blocks coming
// and dispatches the relevant FilteredBlock notifications, updates the filter
// due to requests by callers, and finally is able to preform targeted block
// filtration.
//
// TODO(roasbeef): change to use loadfilter RPC's
func (b *BitcoindFilteredChainView) chainFilterer() {
	defer b.wg.Done()

	// filterBlock is a helper function that scans the given block, and
	// notes which transactions spend outputs which are currently being
	// watched. Additionally, the chain filter will also be updated by
	// removing any spent outputs.
	filterBlock := func(blk *wire.MsgBlock) []*wire.MsgTx {
		var filteredTxns []*wire.MsgTx
		for _, tx := range blk.Transactions {
			for _, txIn := range tx.TxIn {
				prevOp := txIn.PreviousOutPoint
				if _, ok := b.chainFilter[prevOp]; ok {
					filteredTxns = append(filteredTxns, tx)

					b.filterMtx.Lock()
					delete(b.chainFilter, prevOp)
					b.filterMtx.Unlock()

					break
				}
			}
		}

		return filteredTxns
	}

	decodeJSONBlock := func(block *btcjson.RescannedBlock,
		height uint32) (*FilteredBlock, error) {
		hash, err := chainhash.NewHashFromStr(block.Hash)
		if err != nil {
			return nil, err

		}
		txs := make([]*wire.MsgTx, 0, len(block.Transactions))
		for _, str := range block.Transactions {
			b, err := hex.DecodeString(str)
			if err != nil {
				return nil, err
			}
			tx := &wire.MsgTx{}
			err = tx.Deserialize(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			txs = append(txs, tx)
		}
		return &FilteredBlock{
			Hash:         *hash,
			Height:       height,
			Transactions: txs,
		}, nil
	}

	for {
		select {
		// The caller has just sent an update to the current chain
		// filter, so we'll apply the update, possibly rewinding our
		// state partially.
		case update := <-b.filterUpdates:

			// First, we'll add all the new UTXO's to the set of
			// watched UTXO's, eliminating any duplicates in the
			// process.
			log.Debugf("Updating chain filter with new UTXO's: %v",
				update.newUtxos)
			for _, newOp := range update.newUtxos {
				b.filterMtx.Lock()
				b.chainFilter[newOp] = struct{}{}
				b.filterMtx.Unlock()
			}

			// Apply the new TX filter to the chain client, which
			// will cause all following notifications from and
			// calls to it return blocks filtered with the new
			// filter.
			b.chainClient.LoadTxFilter(false, []btcutil.Address{},
				update.newUtxos)

			// All blocks gotten after we loaded the filter will
			// have the filter applied, but we will need to rescan
			// the blocks up to the height of the block we last
			// added to the blockQueue.
			b.bestHeightMtx.Lock()
			bestHeight := b.bestHeight
			b.bestHeightMtx.Unlock()

			// If the update height matches our best known height,
			// then we don't need to do any rewinding.
			if update.updateHeight == bestHeight {
				continue
			}

			// Otherwise, we'll rewind the state to ensure the
			// caller doesn't miss any relevant notifications.
			// Starting from the height _after_ the update height,
			// we'll walk forwards, rescanning one block at a time
			// with the chain client applying the newly loaded
			// filter to each block.
			for i := update.updateHeight + 1; i < bestHeight+1; i++ {
				blockHash, err := b.chainClient.GetBlockHash(int64(i))
				if err != nil {
					log.Warnf("Unable to get block hash "+
						"for block at height %d: %v",
						i, err)
					continue
				}

				// To avoid dealing with the case where a reorg
				// is happening while we rescan, we scan one
				// block at a time, skipping blocks that might
				// have gone missing.
				rescanned, err := b.chainClient.RescanBlocks(
					[]chainhash.Hash{*blockHash})
				if err != nil {
					log.Warnf("Unable to rescan block "+
						"with hash %v at height %d: %v",
						blockHash, i, err)
					continue
				}

				// If no block was returned from the rescan, it
				// means no matching transactions were found.
				if len(rescanned) != 1 {
					log.Tracef("rescan of block %v at "+
						"height=%d yielded no "+
						"transactions", blockHash, i)
					continue
				}
				decoded, err := decodeJSONBlock(
					&rescanned[0], i)
				if err != nil {
					log.Errorf("Unable to decode block: %v",
						err)
					continue
				}
				b.blockQueue.Add(&blockEvent{
					eventType: connected,
					block:     decoded,
				})
			}

		// We've received a new request to manually filter a block.
		case req := <-b.filterBlockReqs:
			// First we'll fetch the block itself as well as some
			// additional information including its height.
			block, err := b.chainClient.GetBlock(req.blockHash)
			if err != nil {
				req.err <- err
				req.resp <- nil
				continue
			}
			header, err := b.chainClient.GetBlockHeaderVerbose(
				req.blockHash)
			if err != nil {
				req.err <- err
				req.resp <- nil
				continue
			}

			// Once we have this info, we can directly filter the
			// block and dispatch the proper notification.
			req.resp <- &FilteredBlock{
				Hash:         *req.blockHash,
				Height:       uint32(header.Height),
				Transactions: filterBlock(block),
			}
			req.err <- err

		// We've received a new event from the chain client.
		case event := <-b.chainClient.Notifications():
			switch e := event.(type) {
			case chain.FilteredBlockConnected:
				b.onFilteredBlockConnected(e.Block.Height,
					e.Block.Hash, e.RelevantTxs)
			case chain.BlockDisconnected:
				b.onFilteredBlockDisconnected(e.Height, e.Hash)
			}

		case <-b.quit:
			return
		}
	}
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched.  If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BitcoindFilteredChainView) UpdateFilter(ops []wire.OutPoint, updateHeight uint32) error {
	select {

	case b.filterUpdates <- filterUpdate{
		newUtxos:     ops,
		updateHeight: updateHeight,
	}:
		return nil

	case <-b.quit:
		return fmt.Errorf("chain filter shutting down")
	}
}

// FilteredBlocks returns the channel that filtered blocks are to be sent over.
// Each time a block is connected to the end of a main chain, and appropriate
// FilteredBlock which contains the transactions which mutate our watched UTXO
// set is to be returned.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BitcoindFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return b.blockQueue.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BitcoindFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return b.blockQueue.staleBlocks
}
