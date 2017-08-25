package chainview

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
)

// BtcdFilteredChainView is an implementation of the FilteredChainView
// interface which is backed by an active websockets connection to btcd.
type BtcdFilteredChainView struct {
	started int32
	stopped int32

	// bestHash is the hash of the latest block in the main chain.
	bestHash chainhash.Hash

	// bestHeight is the height of the latest block in the main chain.
	bestHeight int32

	btcdConn *rpcclient.Client

	// newBlocks is the channel in which new filtered blocks are sent over.
	newBlocks chan *FilteredBlock

	// staleBlocks is the channel in which blocks that have been
	// disconnected from the mainchain are sent over.
	staleBlocks chan *FilteredBlock

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan filterUpdate

	// The three field below are used to implement a synchronized queue
	// that lets use instantly handle sent notifications without blocking
	// the main websockets notification loop.
	chainUpdates      []*chainUpdate
	chainUpdateSignal chan struct{}
	chainUpdateMtx    sync.Mutex

	// chainFilter is the set of utox's that we're currently watching
	// spends for within the chain.
	chainFilter map[wire.OutPoint]struct{}

	// filterBlockReqs is a channel in which requests to filter select
	// blocks will be sent over.
	filterBlockReqs chan *filterBlockReq

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure BtcdFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*BtcdFilteredChainView)(nil)

// NewBtcdFilteredChainView creates a new instance of a FilteredChainView from
// RPC credentials for an active btcd instance.
func NewBtcdFilteredChainView(config rpcclient.ConnConfig) (*BtcdFilteredChainView, error) {
	chainView := &BtcdFilteredChainView{
		newBlocks:         make(chan *FilteredBlock),
		staleBlocks:       make(chan *FilteredBlock),
		chainUpdateSignal: make(chan struct{}),
		chainFilter:       make(map[wire.OutPoint]struct{}),
		filterUpdates:     make(chan filterUpdate),
		filterBlockReqs:   make(chan *filterBlockReq),
		quit:              make(chan struct{}),
	}

	ntfnCallbacks := &rpcclient.NotificationHandlers{
		OnBlockConnected:    chainView.onBlockConnected,
		OnBlockDisconnected: chainView.onBlockDisconnected,
	}

	// Disable connecting to btcd within the rpcclient.New method. We
	// defer establishing the connection to our .Start() method.
	config.DisableConnectOnNew = true
	config.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(&config, ntfnCallbacks)
	if err != nil {
		return nil, err
	}
	chainView.btcdConn = chainConn

	return chainView, nil
}

// Start starts all goroutines necessary for normal operation.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BtcdFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView starting")

	// Connect to btcd, and register for notifications on connected, and
	// disconnected blocks.
	if err := b.btcdConn.Connect(20); err != nil {
		return err
	}
	if err := b.btcdConn.NotifyBlocks(); err != nil {
		return err
	}

	bestHash, bestHeight, err := b.btcdConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.bestHash, b.bestHeight = *bestHash, bestHeight

	b.wg.Add(1)
	go b.chainFilterer()

	return nil
}

// Stop stops all goroutines which we launched by the prior call to the Start
// method.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BtcdFilteredChainView) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from btcd, and
	// cleans up all related resources.
	b.btcdConn.Shutdown()

	log.Infof("FilteredChainView stopping")

	close(b.quit)
	b.wg.Wait()

	return nil
}

// chainUpdate encapsulates an update to the current main chain. This struct is
// used as an element within an unbounded queue in order to avoid blocking the
// main rpc dispatch rule.
type chainUpdate struct {
	blockHash   *chainhash.Hash
	blockHeight int32
}

// onBlockConnected implements on OnBlockConnected callback for rpcclient.
// Ingesting a block updates the wallet's internal utxo state based on the
// outputs created and destroyed within each block.
func (b *BtcdFilteredChainView) onBlockConnected(hash *chainhash.Hash,
	height int32, t time.Time) {

	// Append this new chain update to the end of the queue of new chain
	// updates.
	b.chainUpdateMtx.Lock()
	b.chainUpdates = append(b.chainUpdates, &chainUpdate{hash, height})
	b.chainUpdateMtx.Unlock()

	// Launch a goroutine to signal the notification dispatcher that a new
	// block update is available. We do this in a new goroutine in order to
	// avoid blocking the main loop of the rpc client.
	go func() {
		b.chainUpdateSignal <- struct{}{}
	}()
}

// onBlockDisconnected implements on OnBlockDisconnected callback for rpcclient.
func (b *BtcdFilteredChainView) onBlockDisconnected(hash *chainhash.Hash,
	height int32, t time.Time) {

	// TODO(roasbeef): impl
}

// filterBlockReq houses a request to manually filter a block specified by
// block hash.
type filterBlockReq struct {
	blockHash *chainhash.Hash
	resp      chan *FilteredBlock
	err       chan error
}

// FilterBlock takes a block hash, and returns a FilteredBlocks which is the
// result of applying the current registered UTXO sub-set on the block
// corresponding to that block hash. If any watched UTOX's are spent by the
// selected lock, then the internal chainFilter will also be updated.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BtcdFilteredChainView) FilterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
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
// and dispatches the relevent FilteredBlock notifications, updates the filter
// due to requests by callers, and finally is able to preform targeted block
// filtration.
//
// TODO(roasbeef): change to use loadfilter RPC's
func (b *BtcdFilteredChainView) chainFilterer() {
	defer b.wg.Done()

	// filterBlock is a helper funciton that scans the given block, and
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

					delete(b.chainFilter, prevOp)

					break
				}
			}
		}

		return filteredTxns
	}

	for {
		select {

		// A new block has been connected to the end of the main chain.
		// So we'll need to dispatch a new FilteredBlock notification.
		case <-b.chainUpdateSignal:
			// A new update is available, so pop the new chain
			// update from the front of the update queue.
			b.chainUpdateMtx.Lock()
			update := b.chainUpdates[0]
			b.chainUpdates[0] = nil // Set to nil to prevent GC leak.
			b.chainUpdates = b.chainUpdates[1:]
			b.chainUpdateMtx.Unlock()

			// Now that we have the new block has, fetch the new
			// block itself.
			newBlock, err := b.btcdConn.GetBlock(update.blockHash)
			if err != nil {
				log.Errorf("Unable to get block: %v", err)
				continue
			}
			b.bestHash, b.bestHeight = *update.blockHash, update.blockHeight

			// Next, we'll scan this block to see if it modified
			// any of the UTXO set that we're watching.
			filteredTxns := filterBlock(newBlock)

			// Finally, launch a goroutine to dispatch this
			// filtered block notification.
			go func() {
				b.newBlocks <- &FilteredBlock{
					Hash:         *update.blockHash,
					Height:       uint32(update.blockHeight),
					Transactions: filteredTxns,
				}
			}()

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
				b.chainFilter[newOp] = struct{}{}
			}

			// If the update height matches our best known height,
			// then we don't need to do any rewinding.
			if update.updateHeight == uint32(b.bestHeight) {
				continue
			}

			// Otherwise, we'll rewind the state to ensure the
			// caller doesn't miss any relevant notifications.
			// Starting from the height _after_ the update height,
			// we'll walk forwards, manually filtering blocks.
			for i := int32(update.updateHeight) + 1; i < b.bestHeight+1; i++ {
				blockHash, err := b.btcdConn.GetBlockHash(int64(i))
				if err != nil {
					log.Errorf("Unable to get block hash: %v", err)
					continue
				}
				block, err := b.btcdConn.GetBlock(blockHash)
				if err != nil {
					log.Errorf("Unable to get block: %v", err)
					continue
				}

				filteredTxns := filterBlock(block)

				go func(height uint32) {
					b.newBlocks <- &FilteredBlock{
						Hash:         *blockHash,
						Height:       height,
						Transactions: filteredTxns,
					}
				}(uint32(i))
			}

		// We've received a new request to manually filter a block.
		case req := <-b.filterBlockReqs:
			// First we'll fetch the block itself as well as some
			// additional information including its height.
			block, err := b.btcdConn.GetBlock(req.blockHash)
			if err != nil {
				req.err <- err
				req.resp <- nil
				continue
			}
			header, err := b.btcdConn.GetBlockHeaderVerbose(req.blockHash)
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

		case <-b.quit:
			return
		}
	}
}

// filterUpdate is a message sent to the chainFilterer to update the current
// chainFilter state.
type filterUpdate struct {
	newUtxos     []wire.OutPoint
	updateHeight uint32
	done         chan struct{}
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched.  If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BtcdFilteredChainView) UpdateFilter(ops []wire.OutPoint, updateHeight uint32) error {
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
func (b *BtcdFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return b.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (b *BtcdFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return b.staleBlocks
}
