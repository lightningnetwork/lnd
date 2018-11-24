package neutrinonotify

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
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/gcs/builder"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "neutrino"

	// reorgSafetyLimit is the chain depth beyond which it is assumed a block
	// will not be reorganized out of the chain. This is used to determine when
	// to prune old confirmation requests so that reorgs are handled correctly.
	// The coinbase maturity period is a reasonable value to use.
	reorgSafetyLimit = 100
)

var (
	// ErrChainNotifierShuttingDown is used when we are trying to
	// measure a spend notification when notifier is already stopped.
	ErrChainNotifierShuttingDown = errors.New("chainntnfs: system interrupt " +
		"while attempting to register for spend notification.")
)

// NeutrinoNotifier is a version of ChainNotifier that's backed by the neutrino
// Bitcoin light client. Unlike other implementations, this implementation
// speaks directly to the p2p network. As a result, this implementation of the
// ChainNotifier interface is much more light weight that other implementation
// which rely of receiving notification over an RPC interface backed by a
// running full node.
//
// TODO(roasbeef): heavily consolidate with NeutrinoNotifier code
//  * maybe combine into single package?
type NeutrinoNotifier struct {
	confClientCounter  uint64 // To be used atomically.
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	heightMtx  sync.RWMutex
	bestHeight uint32

	p2pNode   *neutrino.ChainService
	chainView *neutrino.Rescan

	chainConn *NeutrinoChainConn

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	txNotifier *chainntnfs.TxNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	rescanErr <-chan error

	chainUpdates *queue.ConcurrentQueue

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

// Ensure NeutrinoNotifier implements the ChainNotifier interface at compile time.
var _ chainntnfs.ChainNotifier = (*NeutrinoNotifier)(nil)

// New creates a new instance of the NeutrinoNotifier concrete implementation
// of the ChainNotifier interface.
//
// NOTE: The passed neutrino node should already be running and active before
// being passed into this function.
func New(node *neutrino.ChainService, spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache) (*NeutrinoNotifier, error) {

	notifier := &NeutrinoNotifier{
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		p2pNode: node,

		rescanErr: make(chan error),

		chainUpdates: queue.NewConcurrentQueue(10),

		spendHintCache:   spendHintCache,
		confirmHintCache: confirmHintCache,

		quit: make(chan struct{}),
	}

	return notifier, nil
}

// Start contacts the running neutrino light client and kicks off an initial
// empty rescan.
func (n *NeutrinoNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&n.started, 1) != 1 {
		return nil
	}

	// First, we'll obtain the latest block height of the p2p node. We'll
	// start the auto-rescan from this point. Once a caller actually wishes
	// to register a chain view, the rescan state will be rewound
	// accordingly.
	startingPoint, err := n.p2pNode.BestBlock()
	if err != nil {
		return err
	}

	n.bestHeight = uint32(startingPoint.Height)

	// Next, we'll create our set of rescan options. Currently it's
	// required that a user MUST set an addr/outpoint/txid when creating a
	// rescan. To get around this, we'll add a "zero" outpoint, that won't
	// actually be matched.
	var zeroInput neutrino.InputWithScript
	rescanOptions := []neutrino.RescanOption{
		neutrino.StartBlock(startingPoint),
		neutrino.QuitChan(n.quit),
		neutrino.NotificationHandlers(
			rpcclient.NotificationHandlers{
				OnFilteredBlockConnected:    n.onFilteredBlockConnected,
				OnFilteredBlockDisconnected: n.onFilteredBlockDisconnected,
			},
		),
		neutrino.WatchInputs(zeroInput),
	}

	n.txNotifier = chainntnfs.NewTxNotifier(
		n.bestHeight, reorgSafetyLimit, n.confirmHintCache,
		n.spendHintCache,
	)

	n.chainConn = &NeutrinoChainConn{n.p2pNode}

	// Finally, we'll create our rescan struct, start it, and launch all
	// the goroutines we need to operate this ChainNotifier instance.
	n.chainView = n.p2pNode.NewRescan(rescanOptions...)
	n.rescanErr = n.chainView.Start()

	n.chainUpdates.Start()

	n.wg.Add(1)
	go n.notificationDispatcher()

	return nil
}

// Stop shuts down the NeutrinoNotifier.
func (n *NeutrinoNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&n.stopped, 1) != 1 {
		return nil
	}

	close(n.quit)
	n.wg.Wait()

	n.chainUpdates.Stop()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, epochClient := range n.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()

		close(epochClient.epochChan)
	}
	n.txNotifier.TearDown()

	return nil
}

// filteredBlock represents a new block which has been connected to the main
// chain. The slice of transactions will only be populated if the block
// includes a transaction that confirmed one of our watched txids, or spends
// one of the outputs currently being watched.
type filteredBlock struct {
	hash   chainhash.Hash
	height uint32
	txns   []*btcutil.Tx

	// connected is true if this update is a new block and false if it is a
	// disconnected block.
	connect bool
}

// rescanFilterUpdate represents a request that will be sent to the
// notificaionRegistry in order to prevent race conditions between the filter
// update and new block notifications.
type rescanFilterUpdate struct {
	updateOptions []neutrino.UpdateOption
	errChan       chan error
}

// onFilteredBlockConnected is a callback which is executed each a new block is
// connected to the end of the main chain.
func (n *NeutrinoNotifier) onFilteredBlockConnected(height int32,
	header *wire.BlockHeader, txns []*btcutil.Tx) {

	// Append this new chain update to the end of the queue of new chain
	// updates.
	n.chainUpdates.ChanIn() <- &filteredBlock{
		hash:    header.BlockHash(),
		height:  uint32(height),
		txns:    txns,
		connect: true,
	}
}

// onFilteredBlockDisconnected is a callback which is executed each time a new
// block has been disconnected from the end of the mainchain due to a re-org.
func (n *NeutrinoNotifier) onFilteredBlockDisconnected(height int32,
	header *wire.BlockHeader) {

	// Append this new chain update to the end of the queue of new chain
	// disconnects.
	n.chainUpdates.ChanIn() <- &filteredBlock{
		hash:    header.BlockHash(),
		height:  uint32(height),
		connect: false,
	}
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (n *NeutrinoNotifier) notificationDispatcher() {
	defer n.wg.Done()
out:
	for {
		select {
		case cancelMsg := <-n.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				// First, we'll lookup the original
				// registration in order to stop the active
				// queue goroutine.
				reg := n.blockEpochClients[msg.epochID]
				reg.epochQueue.Stop()

				// Next, close the cancel channel for this
				// specific client, and wait for the client to
				// exit.
				close(n.blockEpochClients[msg.epochID].cancelChan)
				n.blockEpochClients[msg.epochID].wg.Wait()

				// Once the client has exited, we can then
				// safely close the channel used to send epoch
				// notifications, in order to notify any
				// listeners that the intent has been
				// cancelled.
				close(n.blockEpochClients[msg.epochID].epochChan)
				delete(n.blockEpochClients, msg.epochID)
			}

		case registerMsg := <-n.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *chainntnfs.HistoricalConfDispatch:
				// We'll start a historical rescan chain of the
				// chain asynchronously to prevent blocking
				// potentially long rescans.
				n.wg.Add(1)
				go func() {
					defer n.wg.Done()

					confDetails, err := n.historicalConfDetails(
						msg.TxID, msg.PkScript,
						msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Error(err)
					}

					// If the historical dispatch finished
					// without error, we will invoke
					// UpdateConfDetails even if none were
					// found. This allows the notifier to
					// begin safely updating the height hint
					// cache at tip, since any pending
					// rescans have now completed.
					err = n.txNotifier.UpdateConfDetails(
						*msg.TxID, confDetails,
					)
					if err != nil {
						chainntnfs.Log.Error(err)
					}
				}()

			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")
				n.blockEpochClients[msg.epochID] = msg
				if msg.bestBlock != nil {
					n.heightMtx.Lock()
					bestHeight := int32(n.bestHeight)
					n.heightMtx.Unlock()
					missedBlocks, err :=
						chainntnfs.GetClientMissedBlocks(
							n.chainConn, msg.bestBlock,
							bestHeight, false,
						)
					if err != nil {
						msg.errorChan <- err
						continue
					}
					for _, block := range missedBlocks {
						n.notifyBlockEpochClient(msg,
							block.Height, block.Hash)
					}
				}
				msg.errorChan <- nil

			case *rescanFilterUpdate:
				err := n.chainView.Update(msg.updateOptions...)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to "+
						"update rescan filter: %v", err)
				}
				msg.errChan <- err
			}

		case item := <-n.chainUpdates.ChanOut():
			update := item.(*filteredBlock)
			if update.connect {
				n.heightMtx.Lock()
				// Since neutrino has no way of knowing what
				// height to rewind to in the case of a reorged
				// best known height, there is no point in
				// checking that the previous hash matches the
				// the hash from our best known height the way
				// the other notifiers do when they receive
				// a new connected block. Therefore, we just
				// compare the heights.
				if update.height != n.bestHeight+1 {
					// Handle the case where the notifier
					// missed some blocks from its chain
					// backend
					chainntnfs.Log.Infof("Missed blocks, " +
						"attempting to catch up")
					bestBlock := chainntnfs.BlockEpoch{
						Height: int32(n.bestHeight),
						Hash:   nil,
					}
					_, missedBlocks, err :=
						chainntnfs.HandleMissedBlocks(
							n.chainConn,
							n.txNotifier,
							bestBlock,
							int32(update.height),
							false,
						)
					if err != nil {
						chainntnfs.Log.Error(err)
						n.heightMtx.Unlock()
						continue
					}

					for _, block := range missedBlocks {
						filteredBlock, err :=
							n.getFilteredBlock(block)
						if err != nil {
							chainntnfs.Log.Error(err)
							n.heightMtx.Unlock()
							continue out
						}
						err = n.handleBlockConnected(filteredBlock)
						if err != nil {
							chainntnfs.Log.Error(err)
							n.heightMtx.Unlock()
							continue out
						}
					}

				}

				err := n.handleBlockConnected(update)
				if err != nil {
					chainntnfs.Log.Error(err)
				}
				n.heightMtx.Unlock()
				continue
			}

			n.heightMtx.Lock()
			if update.height != uint32(n.bestHeight) {
				chainntnfs.Log.Infof("Missed disconnected " +
					"blocks, attempting to catch up")
			}

			hash, err := n.p2pNode.GetBlockHash(int64(n.bestHeight))
			if err != nil {
				chainntnfs.Log.Errorf("Unable to fetch block hash "+
					"for height %d: %v", n.bestHeight, err)
				n.heightMtx.Unlock()
				continue
			}

			notifierBestBlock := chainntnfs.BlockEpoch{
				Height: int32(n.bestHeight),
				Hash:   hash,
			}
			newBestBlock, err := chainntnfs.RewindChain(
				n.chainConn, n.txNotifier, notifierBestBlock,
				int32(update.height-1),
			)
			if err != nil {
				chainntnfs.Log.Errorf("Unable to rewind chain "+
					"from height %d to height %d: %v",
					n.bestHeight, update.height-1, err)
			}

			// Set the bestHeight here in case a chain rewind
			// partially completed.
			n.bestHeight = uint32(newBestBlock.Height)
			n.heightMtx.Unlock()

		case err := <-n.rescanErr:
			chainntnfs.Log.Errorf("Error during rescan: %v", err)

		case <-n.quit:
			return

		}
	}
}

// historicalConfDetails looks up whether a transaction is already included in
// a block in the active chain and, if so, returns details about the
// confirmation.
func (n *NeutrinoNotifier) historicalConfDetails(targetHash *chainhash.Hash,
	pkScript []byte,
	startHeight, endHeight uint32) (*chainntnfs.TxConfirmation, error) {

	// Starting from the height hint, we'll walk forwards in the chain to
	// see if this transaction has already been confirmed.
	for scanHeight := startHeight; scanHeight <= endHeight; scanHeight++ {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-n.quit:
			return nil, ErrChainNotifierShuttingDown
		default:
		}

		// First, we'll fetch the block header for this height so we
		// can compute the current block hash.
		blockHash, err := n.p2pNode.GetBlockHash(int64(scanHeight))
		if err != nil {
			return nil, fmt.Errorf("unable to get header for height=%v: %v",
				scanHeight, err)
		}

		// With the hash computed, we can now fetch the basic filter
		// for this height.
		regFilter, err := n.p2pNode.GetCFilter(
			*blockHash, wire.GCSFilterRegular,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve regular filter for "+
				"height=%v: %v", scanHeight, err)
		}

		// If the block has no transactions other than the Coinbase
		// transaction, then the filter may be nil, so we'll continue
		// forward int that case.
		if regFilter == nil {
			continue
		}

		// In the case that the filter exists, we'll attempt to see if
		// any element in it matches our target public key script.
		key := builder.DeriveKey(blockHash)
		match, err := regFilter.Match(key, pkScript)
		if err != nil {
			return nil, fmt.Errorf("unable to query filter: %v", err)
		}

		// If there's no match, then we can continue forward to the
		// next block.
		if !match {
			continue
		}

		// In the case that we do have a match, we'll fetch the block
		// from the network so we can find the positional data required
		// to send the proper response.
		block, err := n.p2pNode.GetBlock(*blockHash)
		if err != nil {
			return nil, fmt.Errorf("unable to get block from network: %v", err)
		}
		for j, tx := range block.Transactions() {
			txHash := tx.Hash()
			if txHash.IsEqual(targetHash) {
				confDetails := chainntnfs.TxConfirmation{
					BlockHash:   blockHash,
					BlockHeight: scanHeight,
					TxIndex:     uint32(j),
				}
				return &confDetails, nil
			}
		}
	}

	return nil, nil
}

// handleBlockConnected applies a chain update for a new block. Any watched
// transactions included this block will processed to either send notifications
// now or after numConfirmations confs.
func (n *NeutrinoNotifier) handleBlockConnected(newBlock *filteredBlock) error {
	// We'll extend the txNotifier's height with the information of this new
	// block, which will handle all of the notification logic for us.
	err := n.txNotifier.ConnectTip(
		&newBlock.hash, newBlock.height, newBlock.txns,
	)
	if err != nil {
		return fmt.Errorf("unable to connect tip: %v", err)
	}

	chainntnfs.Log.Infof("New block: height=%v, sha=%v", newBlock.height,
		newBlock.hash)

	// Now that we've guaranteed the new block extends the txNotifier's
	// current tip, we'll proceed to dispatch notifications to all of our
	// registered clients whom have had notifications fulfilled. Before
	// doing so, we'll make sure update our in memory state in order to
	// satisfy any client requests based upon the new block.
	n.bestHeight = newBlock.height

	n.notifyBlockEpochs(int32(newBlock.height), &newBlock.hash)
	return n.txNotifier.NotifyHeight(newBlock.height)
}

// getFilteredBlock is a utility to retrieve the full filtered block from a block epoch.
func (n *NeutrinoNotifier) getFilteredBlock(epoch chainntnfs.BlockEpoch) (*filteredBlock, error) {
	rawBlock, err := n.p2pNode.GetBlock(*epoch.Hash)
	if err != nil {
		return nil, fmt.Errorf("unable to get block: %v", err)
	}

	txns := rawBlock.Transactions()

	block := &filteredBlock{
		hash:    *epoch.Hash,
		height:  uint32(epoch.Height),
		txns:    txns,
		connect: true,
	}
	return block, nil
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (n *NeutrinoNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	for _, client := range n.blockEpochClients {
		n.notifyBlockEpochClient(client, newHeight, newSha)
	}
}

// notifyBlockEpochClient sends a registered block epoch client a notification
// about a specific block.
func (n *NeutrinoNotifier) notifyBlockEpochClient(epochClient *blockEpochRegistration,
	height int32, sha *chainhash.Hash) {

	epoch := &chainntnfs.BlockEpoch{
		Height: height,
		Hash:   sha,
	}

	select {
	case epochClient.epochQueue.ChanIn() <- epoch:
	case <-epochClient.cancelChan:
	case <-n.quit:
	}
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint has been spent by a transaction on-chain. Once a spend of the
// target outpoint has been detected, the details of the spending event will be
// sent across the 'Spend' channel.
func (n *NeutrinoNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// First, we'll construct a spend notification request and hand it off
	// to the txNotifier.
	spendID := atomic.AddUint64(&n.spendClientCounter, 1)
	cancel := func() {
		n.txNotifier.CancelSpend(*outpoint, spendID)
	}
	ntfn := &chainntnfs.SpendNtfn{
		SpendID:    spendID,
		OutPoint:   *outpoint,
		Event:      chainntnfs.NewSpendEvent(cancel),
		HeightHint: heightHint,
	}

	historicalDispatch, err := n.txNotifier.RegisterSpend(ntfn)
	if err != nil {
		return nil, err
	}

	// If the txNotifier didn't return any details to perform a historical
	// scan of the chain, then we can return early as there's nothing left
	// for us to do.
	if historicalDispatch == nil {
		return ntfn.Event, nil
	}

	// To determine whether this outpoint has been spent on-chain, we'll
	// update our filter to watch for the transaction at tip and we'll also
	// dispatch a historical rescan to determine if it has been spent in the
	// past.
	//
	// We'll update our filter first to ensure we can immediately detect the
	// spend at tip. To do so, we'll map the script into an address
	// type so we can instruct neutrino to match if the transaction
	// containing the script is found in a block.
	inputToWatch := neutrino.InputWithScript{
		OutPoint: *outpoint,
		PkScript: pkScript,
	}
	errChan := make(chan error, 1)
	select {
	case n.notificationRegistry <- &rescanFilterUpdate{
		updateOptions: []neutrino.UpdateOption{
			neutrino.AddInputs(inputToWatch),
			neutrino.Rewind(historicalDispatch.EndHeight),
			neutrino.DisableDisconnectedNtfns(true),
		},
		errChan: errChan,
	}:
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	}

	select {
	case err = <-errChan:
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	}
	if err != nil {
		return nil, fmt.Errorf("unable to update filter: %v", err)
	}

	// With the filter updated, we'll dispatch our historical rescan to
	// ensure we detect the spend if it happened in the past. We'll ensure
	// that neutrino is caught up to the starting height before we attempt
	// to fetch the UTXO from the chain. If we're behind, then we may miss a
	// notification dispatch.
	for {
		n.heightMtx.RLock()
		currentHeight := n.bestHeight
		n.heightMtx.RUnlock()

		if currentHeight >= historicalDispatch.StartHeight {
			break
		}

		time.Sleep(time.Millisecond * 200)
	}

	spendReport, err := n.p2pNode.GetUtxo(
		neutrino.WatchInputs(inputToWatch),
		neutrino.StartBlock(&waddrmgr.BlockStamp{
			Height: int32(historicalDispatch.StartHeight),
		}),
		neutrino.EndBlock(&waddrmgr.BlockStamp{
			Height: int32(historicalDispatch.EndHeight),
		}),
	)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return nil, err
	}

	// If a spend report was returned, and the transaction is present, then
	// this means that the output is already spent.
	var spendDetails *chainntnfs.SpendDetail
	if spendReport != nil && spendReport.SpendingTx != nil {
		spendingTxHash := spendReport.SpendingTx.TxHash()
		spendDetails = &chainntnfs.SpendDetail{
			SpentOutPoint:     outpoint,
			SpenderTxHash:     &spendingTxHash,
			SpendingTx:        spendReport.SpendingTx,
			SpenderInputIndex: spendReport.SpendingInputIndex,
			SpendingHeight:    int32(spendReport.SpendingTxHeight),
		}
	}

	// Finally, no matter whether the rescan found a spend in the past or
	// not, we'll mark our historical rescan as complete to ensure the
	// outpoint's spend hint gets updated upon connected/disconnected
	// blocks.
	err = n.txNotifier.UpdateSpendDetails(*outpoint, spendDetails)
	if err != nil {
		return nil, err
	}

	return ntfn.Event, nil
}

// RegisterConfirmationsNtfn registers a notification with NeutrinoNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (n *NeutrinoNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte,
	numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {

	// Construct a notification request for the transaction and send it to
	// the main event loop.
	ntfn := &chainntnfs.ConfNtfn{
		ConfID:           atomic.AddUint64(&n.confClientCounter, 1),
		TxID:             txid,
		PkScript:         pkScript,
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
	dispatch, err := n.txNotifier.RegisterConf(ntfn)
	if err != nil {
		return nil, err
	}

	if dispatch == nil {
		return ntfn.Event, nil
	}

	// To determine whether this transaction has confirmed on-chain, we'll
	// update our filter to watch for the transaction at tip and we'll also
	// dispatch a historical rescan to determine if it has confirmed in the
	// past.
	//
	// We'll update our filter first to ensure we can immediately detect the
	// confirmation at tip. To do so, we'll map the script into an address
	// type so we can instruct neutrino to match if the transaction
	// containing the script is found in a block.
	params := n.p2pNode.ChainParams()
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, &params)
	if err != nil {
		return nil, fmt.Errorf("unable to extract script: %v", err)
	}

	// We'll send the filter update request to the notifier's main event
	// handler and wait for its response.
	errChan := make(chan error, 1)
	select {
	case n.notificationRegistry <- &rescanFilterUpdate{
		updateOptions: []neutrino.UpdateOption{
			neutrino.AddAddrs(addrs...),
			neutrino.Rewind(dispatch.EndHeight),
			neutrino.DisableDisconnectedNtfns(true),
		},
		errChan: errChan,
	}:
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	}

	select {
	case err = <-errChan:
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	}
	if err != nil {
		return nil, fmt.Errorf("unable to update filter: %v", err)
	}

	// Finally, with the filter updates, we can dispatch the historical
	// rescan to ensure we can detect if the event happened in the past.
	select {
	case n.notificationRegistry <- dispatch:
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	}

	return ntfn.Event, nil
}

// blockEpochRegistration represents a client's intent to receive a
// notification with each newly connected block.
type blockEpochRegistration struct {
	epochID uint64

	epochChan chan *chainntnfs.BlockEpoch

	epochQueue *queue.ConcurrentQueue

	cancelChan chan struct{}

	bestBlock *chainntnfs.BlockEpoch

	errorChan chan error

	wg sync.WaitGroup
}

// epochCancel is a message sent to the NeutrinoNotifier when a client wishes
// to cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications, of each new block connected to the main
// chain. Clients have the option of passing in their best known block, which
// the notifier uses to check if they are behind on blocks and catch them up.
func (n *NeutrinoNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	reg := &blockEpochRegistration{
		epochQueue: queue.NewConcurrentQueue(20),
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&n.epochClientCounter, 1),
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

				case <-n.quit:
					return
				}

			case <-reg.cancelChan:
				return

			case <-n.quit:
				return
			}
		}
	}()

	select {
	case <-n.quit:
		// As we're exiting before the registration could be sent,
		// we'll stop the queue now ourselves.
		reg.epochQueue.Stop()

		return nil, errors.New("chainntnfs: system interrupt while " +
			"attempting to register for block epoch notification.")
	case n.notificationRegistry <- reg:
		return &chainntnfs.BlockEpochEvent{
			Epochs: reg.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: reg.epochID,
				}

				// Submit epoch cancellation to notification dispatcher.
				select {
				case n.notificationCancels <- cancel:
					// Cancellation is being handled, drain the epoch channel until it is
					// closed before yielding to caller.
					for {
						select {
						case _, ok := <-reg.epochChan:
							if !ok {
								return
							}
						case <-n.quit:
							return
						}
					}
				case <-n.quit:
				}
			},
		}, nil
	}
}

// NeutrinoChainConn is a wrapper around neutrino's chain backend in order
// to satisfy the chainntnfs.ChainConn interface.
type NeutrinoChainConn struct {
	p2pNode *neutrino.ChainService
}

// GetBlockHeader returns the block header for a hash.
func (n *NeutrinoChainConn) GetBlockHeader(blockHash *chainhash.Hash) (*wire.BlockHeader, error) {
	return n.p2pNode.GetBlockHeader(blockHash)
}

// GetBlockHeaderVerbose returns a verbose block header result for a hash. This
// result only contains the height with a nil hash.
func (n *NeutrinoChainConn) GetBlockHeaderVerbose(blockHash *chainhash.Hash) (
	*btcjson.GetBlockHeaderVerboseResult, error) {

	height, err := n.p2pNode.GetBlockHeight(blockHash)
	if err != nil {
		return nil, err
	}
	// Since only the height is used from the result, leave the hash nil.
	return &btcjson.GetBlockHeaderVerboseResult{Height: int32(height)}, nil
}

// GetBlockHash returns the hash from a block height.
func (n *NeutrinoChainConn) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return n.p2pNode.GetBlockHash(blockHeight)
}
