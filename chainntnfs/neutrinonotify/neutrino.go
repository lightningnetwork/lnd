package neutrinonotify

import (
	"container/heap"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcutil/gcs/builder"
	"github.com/roasbeef/btcwallet/waddrmgr"
)

const (

	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "neutrino"
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
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	heightMtx  sync.RWMutex
	bestHeight uint32

	p2pNode   *neutrino.ChainService
	chainView neutrino.Rescan

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	spendNotifications map[wire.OutPoint]map[uint64]*spendNotification

	confNotifications map[chainhash.Hash][]*confirmationsNotification
	confHeap          *confirmationHeap

	blockEpochClients map[uint64]*blockEpochRegistration

	rescanErr <-chan error

	newBlocks   *chainntnfs.ConcurrentQueue
	staleBlocks *chainntnfs.ConcurrentQueue

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
func New(node *neutrino.ChainService) (*NeutrinoNotifier, error) {
	notifier := &NeutrinoNotifier{
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendNotifications: make(map[wire.OutPoint]map[uint64]*spendNotification),

		confNotifications: make(map[chainhash.Hash][]*confirmationsNotification),
		confHeap:          newConfirmationHeap(),

		p2pNode: node,

		rescanErr: make(chan error),

		newBlocks: chainntnfs.NewConcurrentQueue(10),

		staleBlocks: chainntnfs.NewConcurrentQueue(10),

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
	bestHeader, bestHeight, err := n.p2pNode.BlockHeaders.ChainTip()
	if err != nil {
		return err
	}
	startingPoint := &waddrmgr.BlockStamp{
		Height: int32(bestHeight),
		Hash:   bestHeader.BlockHash(),
	}
	n.bestHeight = bestHeight

	// Next, we'll create our set of rescan options. Currently it's
	// required that a user MUST set a addr/outpoint/txid when creating a
	// rescan. To get around this, we'll add a "zero" outpoint, that won't
	// actually be matched.
	var zeroHash chainhash.Hash
	rescanOptions := []neutrino.RescanOption{
		neutrino.StartBlock(startingPoint),
		neutrino.QuitChan(n.quit),
		neutrino.NotificationHandlers(
			rpcclient.NotificationHandlers{
				OnFilteredBlockConnected:    n.onFilteredBlockConnected,
				OnFilteredBlockDisconnected: n.onFilteredBlockDisconnected,
			},
		),
		neutrino.WatchTxIDs(zeroHash),
	}

	// Finally, we'll create our rescan struct, start it, and launch all
	// the goroutines we need to operate this ChainNotifier instance.
	n.chainView = n.p2pNode.NewRescan(rescanOptions...)
	n.rescanErr = n.chainView.Start()

	n.newBlocks.Start()
	n.staleBlocks.Start()

	n.wg.Add(1)
	go n.notificationDispatcher()

	return nil
}

// Stop shutsdown the NeutrinoNotifier.
func (n *NeutrinoNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&n.stopped, 1) != 1 {
		return nil
	}

	close(n.quit)
	n.wg.Wait()

	n.newBlocks.Stop()
	n.staleBlocks.Stop()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, spendClients := range n.spendNotifications {
		for _, spendClient := range spendClients {
			close(spendClient.spendChan)
		}
	}
	for _, confClients := range n.confNotifications {
		for _, confClient := range confClients {
			close(confClient.finConf)
			close(confClient.negativeConf)
		}
	}
	for _, epochClient := range n.blockEpochClients {
		close(epochClient.epochChan)
	}

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
}

// onFilteredBlockConnected is a callback which is executed each a new block is
// connected to the end of the main chain.
func (n *NeutrinoNotifier) onFilteredBlockConnected(height int32,
	header *wire.BlockHeader, txns []*btcutil.Tx) {

	// Append this new chain update to the end of the queue of new chain
	// updates.
	n.newBlocks.ChanIn() <- &filteredBlock{
		hash:   header.BlockHash(),
		height: uint32(height),
		txns:   txns,
	}
}

// onFilteredBlockDisconnected is a callback which is executed each time a new
// block has been disconnected from the end of the mainchain due to a re-org.
func (n *NeutrinoNotifier) onFilteredBlockDisconnected(height int32,
	header *wire.BlockHeader) {

	// Append this new chain update to the end of the queue of new chain
	// disconnects.
	n.staleBlocks.ChanIn() <- &filteredBlock{
		hash:   header.BlockHash(),
		height: uint32(height),
	}
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (n *NeutrinoNotifier) notificationDispatcher() {
	defer n.wg.Done()

	for {
		select {
		case cancelMsg := <-n.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *spendCancel:
				chainntnfs.Log.Infof("Cancelling spend "+
					"notification for out_point=%v, "+
					"spend_id=%v", msg.op, msg.spendID)

				// Before we attempt to close the spendChan,
				// ensure that the notification hasn't already
				// yet been dispatched.
				if outPointClients, ok := n.spendNotifications[msg.op]; ok {
					close(outPointClients[msg.spendID].spendChan)
					delete(n.spendNotifications[msg.op], msg.spendID)
				}

			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				// First, close the cancel channel for this
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
			case *spendNotification:
				chainntnfs.Log.Infof("New spend subscription: "+
					"utxo=%v", msg.targetOutpoint)
				op := *msg.targetOutpoint

				if _, ok := n.spendNotifications[op]; !ok {
					n.spendNotifications[op] = make(map[uint64]*spendNotification)
				}
				n.spendNotifications[op][msg.spendID] = msg

			case *confirmationsNotification:
				chainntnfs.Log.Infof("New confirmations "+
					"subscription: txid=%v, numconfs=%v, "+
					"height_hint=%v", *msg.txid,
					msg.numConfirmations, msg.heightHint)

				// If the notification can be partially or
				// fully dispatched, then we can skip the first
				// phase for ntfns.
				n.heightMtx.RLock()
				currentHeight := n.bestHeight
				if n.attemptHistoricalDispatch(msg, currentHeight, msg.heightHint) {
					n.heightMtx.RUnlock()
					continue
				}
				n.heightMtx.RUnlock()

				// If we can't fully dispatch confirmation,
				// then we'll update our filter so we can be
				// notified of its future initial confirmation.
				rescanUpdate := []neutrino.UpdateOption{
					neutrino.AddTxIDs(*msg.txid),
					neutrino.Rewind(currentHeight),
				}
				if err := n.chainView.Update(rescanUpdate...); err != nil {
					chainntnfs.Log.Errorf("unable to update rescan: %v", err)
				}

				txid := *msg.txid
				n.confNotifications[txid] = append(n.confNotifications[txid], msg)

			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")
				n.blockEpochClients[msg.epochID] = msg
			}

		case item := <-n.newBlocks.ChanOut():
			newBlock := item.(*filteredBlock)

			n.heightMtx.Lock()
			n.bestHeight = newBlock.height
			n.heightMtx.Unlock()

			chainntnfs.Log.Infof("New block: height=%v, sha=%v",
				newBlock.height, newBlock.hash)

			// First we'll notify any subscribed clients of the
			// block.
			n.notifyBlockEpochs(int32(newBlock.height), &newBlock.hash)

			// Next, we'll scan over the list of relevant
			// transactions and possibly dispatch notifications for
			// confirmations and spends.
			for _, tx := range newBlock.txns {
				// Check if the inclusion of this transaction
				// within a block by itself triggers a block
				// confirmation threshold, if so send a
				// notification. Otherwise, place the
				// notification on a heap to be triggered in
				// the future once additional confirmations are
				// attained.
				mtx := tx.MsgTx()
				txIndex := tx.Index()
				txSha := mtx.TxHash()
				n.checkConfirmationTrigger(&txSha, newBlock, txIndex)

				for i, txIn := range mtx.TxIn {
					prevOut := txIn.PreviousOutPoint

					// If this transaction indeed does
					// spend an output which we have a
					// registered notification for, then
					// create a spend summary, finally
					// sending off the details to the
					// notification subscriber.
					if clients, ok := n.spendNotifications[prevOut]; ok {
						// TODO(roasbeef): many
						// integration tests expect
						// spend to be notified within
						// the mempool.
						spendDetails := &chainntnfs.SpendDetail{
							SpentOutPoint:     &prevOut,
							SpenderTxHash:     &txSha,
							SpendingTx:        mtx,
							SpenderInputIndex: uint32(i),
							SpendingHeight:    int32(newBlock.height),
						}

						for _, ntfn := range clients {
							chainntnfs.Log.Infof("Dispatching "+
								"spend notification for "+
								"outpoint=%v", ntfn.targetOutpoint)
							ntfn.spendChan <- spendDetails

							// Close spendChan to ensure that any calls to Cancel will not
							// block. This is safe to do since the channel is buffered, and the
							// message can still be read by the receiver.
							close(ntfn.spendChan)
						}

						delete(n.spendNotifications, prevOut)
					}

				}

			}

			// A new block has been connected to the main chain.
			// Send out any N confirmation notifications which may
			// have been triggered by this new block.
			n.notifyConfs(int32(newBlock.height))

		case item := <-n.staleBlocks.ChanOut():
			staleBlock := item.(*filteredBlock)
			chainntnfs.Log.Warnf("Block disconnected from main "+
				"chain: %v", staleBlock.hash)

		case err := <-n.rescanErr:
			chainntnfs.Log.Errorf("Error during rescan: %v", err)

		case <-n.quit:
			return

		}
	}
}

// attemptHistoricalDispatch attempts to consult the historical chain data to
// see if a transaction has already reached full confirmation status at the
// time a notification for it was registered. If it has, then we do an
// immediate dispatch. Otherwise, we'll add the partially confirmed transaction
// to the confirmation heap.
func (n *NeutrinoNotifier) attemptHistoricalDispatch(msg *confirmationsNotification,
	currentHeight, heightHint uint32) bool {

	targetHash := msg.txid

	var (
		confDetails *chainntnfs.TxConfirmation
		scanHeight  uint32
	)

	chainntnfs.Log.Infof("Attempting to trigger dispatch for %v from "+
		"historical chain", msg.txid)

	// Starting from the height hint, we'll walk forwards in the chain to
	// see if this transaction has already been confirmed.
chainScan:
	for scanHeight := heightHint; scanHeight <= currentHeight; scanHeight++ {
		// First, we'll fetch the block header for this height so we
		// can compute the current block hash.
		header, err := n.p2pNode.BlockHeaders.FetchHeaderByHeight(scanHeight)
		if err != nil {
			chainntnfs.Log.Errorf("unable to get header for "+
				"height=%v: %v", scanHeight, err)
			return false
		}
		blockHash := header.BlockHash()

		// With the hash computed, we can now fetch the basic filter
		// for this height.
		regFilter, err := n.p2pNode.GetCFilter(blockHash,
			wire.GCSFilterRegular)
		if err != nil {
			chainntnfs.Log.Errorf("unable to retrieve regular "+
				"filter for height=%v: %v", scanHeight, err)
			return false
		}

		// If the block has no transactions other than the coinbase
		// transaction, then the filter may be nil, so we'll continue
		// forward int that case.
		if regFilter == nil {
			continue
		}

		// In the case that the filter exists, we'll attempt to see if
		// any element in it match our target txid.
		key := builder.DeriveKey(&blockHash)
		match, err := regFilter.Match(key, targetHash[:])
		if err != nil {
			chainntnfs.Log.Errorf("unable to query filter: %v", err)
			return false
		}

		// If there's no match, then we can continue forward to the
		// next block.
		if !match {
			continue
		}

		// In the case that we do have a match, we'll fetch the block
		// from the network so we can find the positional data required
		// to send the proper response.
		block, err := n.p2pNode.GetBlockFromNetwork(blockHash)
		if err != nil {
			chainntnfs.Log.Errorf("unable to get block from "+
				"network: %v", err)
			return false
		}
		for j, tx := range block.Transactions() {
			txHash := tx.Hash()
			if txHash.IsEqual(targetHash) {
				confDetails = &chainntnfs.TxConfirmation{
					BlockHash:   &blockHash,
					BlockHeight: scanHeight,
					TxIndex:     uint32(j),
				}
				break chainScan
			}
		}
	}

	// If it hasn't yet been confirmed, then we can exit early.
	if confDetails == nil {
		return false
	}

	// Otherwise, we'll calculate the number of confirmations that the
	// transaction has so we can decide if it has reached the desired
	// number of confirmations or not.
	txConfs := currentHeight - scanHeight

	// If the transaction has more that enough confirmations, then we can
	// dispatch it immediately after obtaining for information w.r.t
	// exactly *when* if got all its confirmations.
	if uint32(txConfs) >= msg.numConfirmations {
		msg.finConf <- confDetails
		return true
	}

	// Otherwise, the transaction has only been *partially* confirmed, so
	// we need to insert it into the confirmation heap.
	confsLeft := msg.numConfirmations - uint32(txConfs)
	confHeight := uint32(currentHeight) + confsLeft
	heapEntry := &confEntry{
		msg,
		confDetails,
		confHeight,
	}
	heap.Push(n.confHeap, heapEntry)

	return false
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (n *NeutrinoNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	epoch := &chainntnfs.BlockEpoch{
		Height: newHeight,
		Hash:   newSha,
	}

	for _, epochClient := range n.blockEpochClients {
		n.wg.Add(1)
		epochClient.wg.Add(1)
		go func(ntfnChan chan *chainntnfs.BlockEpoch, cancelChan chan struct{},
			clientWg *sync.WaitGroup) {

			defer clientWg.Done()
			defer n.wg.Done()

			select {
			case ntfnChan <- epoch:

			case <-cancelChan:
				return

			case <-n.quit:
				return
			}
		}(epochClient.epochChan, epochClient.cancelChan, &epochClient.wg)
	}
}

// notifyConfs examines the current confirmation heap, sending off any
// notifications which have been triggered by the connection of a new block at
// newBlockHeight.
func (n *NeutrinoNotifier) notifyConfs(newBlockHeight int32) {
	// If the heap is empty, we have nothing to do.
	if n.confHeap.Len() == 0 {
		return
	}

	// Traverse our confirmation heap. The heap is a min-heap, so the
	// confirmation notification which requires the smallest block-height
	// will always be at the top of the heap. If a confirmation
	// notification is eligible for triggering, then fire it off, and check
	// if another is eligible until there are no more eligible entries.
	nextConf := heap.Pop(n.confHeap).(*confEntry)
	for nextConf.triggerHeight <= uint32(newBlockHeight) {

		nextConf.finConf <- nextConf.initialConfDetails

		if n.confHeap.Len() == 0 {
			return
		}

		nextConf = heap.Pop(n.confHeap).(*confEntry)
	}

	heap.Push(n.confHeap, nextConf)
}

// checkConfirmationTrigger determines if the passed txSha included at
// blockHeight triggers any single confirmation notifications. In the event
// that the txid matches, yet needs additional confirmations, it is added to
// the confirmation heap to be triggered at a later time.
func (n *NeutrinoNotifier) checkConfirmationTrigger(txSha *chainhash.Hash,
	newTip *filteredBlock, txIndex int) {

	// If a confirmation notification has been registered for this txid,
	// then either trigger a notification event if only a single
	// confirmation notification was requested, or place the notification
	// on the confirmation heap for future usage.
	if confClients, ok := n.confNotifications[*txSha]; ok {
		// Either all of the registered confirmations will be
		// dispatched due to a single confirmation, or added to the
		// conf head. Therefor we unconditionally delete the registered
		// confirmations from the staging zone.
		defer func() {
			delete(n.confNotifications, *txSha)
		}()

		for _, confClient := range confClients {
			confDetails := &chainntnfs.TxConfirmation{
				BlockHash:   &newTip.hash,
				BlockHeight: uint32(newTip.height),
				TxIndex:     uint32(txIndex),
			}

			if confClient.numConfirmations == 1 {
				chainntnfs.Log.Infof("Dispatching single conf "+
					"notification, sha=%v, height=%v", txSha,
					newTip.height)
				confClient.finConf <- confDetails
				continue
			}

			// The registered notification requires more than one
			// confirmation before triggering. So we create a
			// heapConf entry for this notification.  The heapConf
			// allows us to easily keep track of which
			// notification(s) we should fire off with each
			// incoming block.
			confClient.initialConfirmHeight = uint32(newTip.height)
			finalConfHeight := confClient.initialConfirmHeight + confClient.numConfirmations - 1
			heapEntry := &confEntry{
				confClient,
				confDetails,
				finalConfHeight,
			}
			heap.Push(n.confHeap, heapEntry)
		}
	}
}

// spendNotification couples a target outpoint along with the channel used for
// notifications once a spend of the outpoint has been detected.
type spendNotification struct {
	targetOutpoint *wire.OutPoint

	spendChan chan *chainntnfs.SpendDetail

	spendID uint64
}

// spendCancel is a message sent to the NeutrinoNotifier when a client wishes
// to cancel an outstanding spend notification that has yet to be dispatched.
type spendCancel struct {
	// op is the target outpoint of the notification to be cancelled.
	op wire.OutPoint

	// spendID the ID of the notification to cancel.
	spendID uint64
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint has been spent by a transaction on-chain. Once a spend of the
// target outpoint has been detected, the details of the spending event will be
// sent across the 'Spend' channel.
func (n *NeutrinoNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	n.heightMtx.RLock()
	currentHeight := n.bestHeight
	n.heightMtx.RUnlock()

	chainntnfs.Log.Infof("New spend notification for outpoint=%v, "+
		"height_hint=%v", outpoint, heightHint)

	ntfn := &spendNotification{
		targetOutpoint: outpoint,
		spendChan:      make(chan *chainntnfs.SpendDetail, 1),
		spendID:        atomic.AddUint64(&n.spendClientCounter, 1),
	}
	spendEvent := &chainntnfs.SpendEvent{
		Spend: ntfn.spendChan,
		Cancel: func() {
			cancel := &spendCancel{
				op:      *outpoint,
				spendID: ntfn.spendID,
			}

			// Submit spend cancellation to notification dispatcher.
			select {
			case n.notificationCancels <- cancel:
				// Cancellation is being handled, drain the spend chan until it is
				// closed before yielding to the caller.
				for {
					select {
					case _, ok := <-ntfn.spendChan:
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
	}

	// Ensure that neutrino is caught up to the height hint before we
	// attempt to fetch the utxo fromt the chain. If we're behind, then we
	// may miss a notification dispatch.
	for {
		n.heightMtx.RLock()
		currentHeight := n.bestHeight
		n.heightMtx.RUnlock()

		if currentHeight < heightHint {
			time.Sleep(time.Millisecond * 200)
			continue
		}

		break
	}

	// Before sending off the notification request, we'll attempt to see if
	// this output is still spent or not at this point in the chain.
	spendReport, err := n.p2pNode.GetUtxo(
		neutrino.WatchOutPoints(*outpoint),
		neutrino.StartBlock(&waddrmgr.BlockStamp{
			Height: int32(heightHint),
		}),
	)
	if err != nil {
		return nil, err
	}

	// If a spend report was returned, and the transaction is present, then
	// this means that the output is already spent.
	if spendReport != nil && spendReport.SpendingTx != nil {
		// As a result, we'll launch a goroutine to immediately
		// dispatch the notification with a normal response.
		go func() {
			txSha := spendReport.SpendingTx.TxHash()
			select {
			case ntfn.spendChan <- &chainntnfs.SpendDetail{
				SpentOutPoint:     outpoint,
				SpenderTxHash:     &txSha,
				SpendingTx:        spendReport.SpendingTx,
				SpenderInputIndex: spendReport.SpendingInputIndex,
				SpendingHeight:    int32(spendReport.SpendingTxHeight),
			}:
			case <-n.quit:
				return
			}

		}()

		return spendEvent, nil
	}

	// If the output is still unspent, then we'll update our rescan's
	// filter, and send the request to the dispatcher goroutine.
	rescanUpdate := []neutrino.UpdateOption{
		neutrino.AddOutPoints(*outpoint),
		neutrino.Rewind(currentHeight),
	}

	if err := n.chainView.Update(rescanUpdate...); err != nil {
		return nil, err
	}

	select {
	case n.notificationRegistry <- ntfn:
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	}

	return spendEvent, nil
}

// confirmationNotification represents a client's intent to receive a
// notification once the target txid reaches numConfirmations confirmations.
type confirmationsNotification struct {
	txid *chainhash.Hash

	heightHint uint32

	initialConfirmHeight uint32
	numConfirmations     uint32

	finConf      chan *chainntnfs.TxConfirmation
	negativeConf chan int32 // TODO(roasbeef): re-org funny business
}

// RegisterConfirmationsNtfn registers a notification with NeutrinoNotifier
// which will be triggered once the txid reaches numConfs number of
// confirmations.
func (n *NeutrinoNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {

	ntfn := &confirmationsNotification{
		txid:             txid,
		heightHint:       heightHint,
		numConfirmations: numConfs,
		finConf:          make(chan *chainntnfs.TxConfirmation, 1),
		negativeConf:     make(chan int32, 1),
	}

	select {
	case <-n.quit:
		return nil, ErrChainNotifierShuttingDown
	case n.notificationRegistry <- ntfn:
		return &chainntnfs.ConfirmationEvent{
			Confirmed:    ntfn.finConf,
			NegativeConf: ntfn.negativeConf,
		}, nil
	}
}

// blockEpochRegistration represents a client's intent to receive a
// notification with each newly connected block.
type blockEpochRegistration struct {
	epochID uint64

	epochChan chan *chainntnfs.BlockEpoch

	cancelChan chan struct{}

	wg sync.WaitGroup
}

// epochCancel is a message sent to the NeutrinoNotifier when a client wishes
// to cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the caller
// to receive notifications, of each new block connected to the main chain.
func (n *NeutrinoNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	registration := &blockEpochRegistration{
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&n.epochClientCounter, 1),
	}

	select {
	case <-n.quit:
		return nil, errors.New("chainntnfs: system interrupt while " +
			"attempting to register for block epoch notification.")
	case n.notificationRegistry <- registration:
		return &chainntnfs.BlockEpochEvent{
			Epochs: registration.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: registration.epochID,
				}

				// Submit epoch cancellation to notification dispatcher.
				select {
				case n.notificationCancels <- cancel:
					// Cancellation is being handled, drain the epoch channel until it is
					// closed before yielding to caller.
					for {
						select {
						case _, ok := <-registration.epochChan:
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
