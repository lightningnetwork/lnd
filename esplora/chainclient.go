package esplora

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

const (
	// esploraBackendName is the name of the Esplora backend.
	esploraBackendName = "esplora"

	// defaultRequestTimeout is the default timeout for Esplora requests.
	defaultRequestTimeout = 30 * time.Second
)

var (
	// ErrChainClientNotStarted is returned when operations are attempted
	// before the chain client is started.
	ErrChainClientNotStarted = errors.New("chain client not started")

	// ErrOutputSpent is returned when the requested output has been spent.
	ErrOutputSpent = errors.New("output has been spent")

	// ErrOutputNotFound is returned when the requested output cannot be
	// found.
	ErrOutputNotFound = errors.New("output not found")
)

// ChainClient is an implementation of chain.Interface that uses an Esplora
// HTTP API as its backend.
type ChainClient struct {
	started int32
	stopped int32

	client         *Client
	chainParams    *chaincfg.Params
	subscriptionID uint64

	// bestBlock tracks the current chain tip.
	bestBlockMtx sync.RWMutex
	bestBlock    waddrmgr.BlockStamp

	// lastProcessedHeight tracks the last block height we sent to the wallet.
	// This is used to ensure we don't skip any blocks.
	lastProcessedHeight int32

	// headerCache caches block headers by hash.
	headerCacheMtx sync.RWMutex
	headerCache    map[chainhash.Hash]*wire.BlockHeader

	// heightToHash maps block heights to hashes.
	heightToHashMtx sync.RWMutex
	heightToHash    map[int32]*chainhash.Hash

	// notificationChan is used to send notifications to the wallet.
	notificationChan chan interface{}

	// notifyBlocks indicates if block notifications are enabled.
	notifyBlocks atomic.Bool

	// watchedAddrs tracks addresses being watched.
	watchedAddrsMtx sync.RWMutex
	watchedAddrs    map[string]btcutil.Address

	// watchedOutpoints tracks outpoints being watched.
	watchedOutpointsMtx sync.RWMutex
	watchedOutpoints    map[wire.OutPoint]btcutil.Address

	quit chan struct{}
	wg   sync.WaitGroup
}

// Compile time check to ensure ChainClient implements chain.Interface.
var _ chain.Interface = (*ChainClient)(nil)

// NewChainClient creates a new Esplora chain client.
func NewChainClient(client *Client, chainParams *chaincfg.Params) *ChainClient {
	return &ChainClient{
		client:           client,
		chainParams:      chainParams,
		headerCache:      make(map[chainhash.Hash]*wire.BlockHeader),
		heightToHash:     make(map[int32]*chainhash.Hash),
		notificationChan: make(chan interface{}, 100),
		watchedAddrs:     make(map[string]btcutil.Address),
		watchedOutpoints: make(map[wire.OutPoint]btcutil.Address),
		quit:             make(chan struct{}),
	}
}

// Start initializes the chain client and begins processing notifications.
func (c *ChainClient) Start() error {
	if atomic.AddInt32(&c.started, 1) != 1 {
		return nil
	}

	log.Info("Starting Esplora chain client")

	// Ensure the underlying client is connected.
	if !c.client.IsConnected() {
		return ErrNotConnected
	}

	// Get initial best block.
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	tipHeight, err := c.client.GetTipHeight(ctx)
	if err != nil {
		return fmt.Errorf("failed to get tip height: %w", err)
	}

	tipHash, err := c.client.GetTipHash(ctx)
	if err != nil {
		return fmt.Errorf("failed to get tip hash: %w", err)
	}

	header, err := c.client.GetBlockHeader(ctx, tipHash)
	if err != nil {
		return fmt.Errorf("failed to get tip header: %w", err)
	}

	hash, err := chainhash.NewHashFromStr(tipHash)
	if err != nil {
		return fmt.Errorf("failed to parse tip hash: %w", err)
	}

	c.bestBlockMtx.Lock()
	c.bestBlock = waddrmgr.BlockStamp{
		Height:    int32(tipHeight),
		Hash:      *hash,
		Timestamp: header.Timestamp,
	}
	// Initialize lastProcessedHeight to current tip - we'll start processing
	// new blocks from here.
	c.lastProcessedHeight = int32(tipHeight)
	c.bestBlockMtx.Unlock()

	// Cache the header.
	c.cacheHeader(int32(tipHeight), hash, header)

	// Start the notification handler.
	c.wg.Add(1)
	go c.notificationHandler()

	// Send ClientConnected notification to trigger wallet sync.
	log.Infof("Sending ClientConnected notification to trigger wallet sync")
	c.notificationChan <- chain.ClientConnected{}

	// Send initial rescan finished notification.
	c.bestBlockMtx.RLock()
	bestBlock := c.bestBlock
	c.bestBlockMtx.RUnlock()

	c.notificationChan <- &chain.RescanFinished{
		Hash:   &bestBlock.Hash,
		Height: bestBlock.Height,
		Time:   bestBlock.Timestamp,
	}

	return nil
}

// Stop shuts down the chain client.
func (c *ChainClient) Stop() {
	if atomic.AddInt32(&c.stopped, 1) != 1 {
		return
	}

	log.Info("Stopping Esplora chain client")

	close(c.quit)
	c.wg.Wait()

	close(c.notificationChan)
}

// WaitForShutdown blocks until the client has finished shutting down.
func (c *ChainClient) WaitForShutdown() {
	c.wg.Wait()
}

// GetBestBlock returns the hash and height of the best known block.
func (c *ChainClient) GetBestBlock() (*chainhash.Hash, int32, error) {
	c.bestBlockMtx.RLock()
	defer c.bestBlockMtx.RUnlock()

	hash := c.bestBlock.Hash
	return &hash, c.bestBlock.Height, nil
}

// GetBlock returns the raw block from the server given its hash.
func (c *ChainClient) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	block, err := c.client.GetBlock(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch block: %w", err)
	}

	return block.MsgBlock(), nil
}

// GetTxIndex returns the index of a transaction within a block at the given height.
func (c *ChainClient) GetTxIndex(height int64, txid string) (uint32, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	return c.client.GetTxIndexByHeight(ctx, height, txid)
}

// GetBlockHash returns the hash of the block at the given height.
func (c *ChainClient) GetBlockHash(height int64) (*chainhash.Hash, error) {
	// Check cache first.
	c.heightToHashMtx.RLock()
	if hash, ok := c.heightToHash[int32(height)]; ok {
		c.heightToHashMtx.RUnlock()
		return hash, nil
	}
	c.heightToHashMtx.RUnlock()

	// Retry logic to handle race condition where esplora hasn't indexed
	// the block yet. This can happen when we receive a block notification
	// but the intermediate blocks haven't been indexed.
	const maxRetries = 5
	const retryDelay = 500 * time.Millisecond

	var hashStr string
	var err error

	for i := 0; i < maxRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
		hashStr, err = c.client.GetBlockHashByHeight(ctx, height)
		cancel()

		if err == nil {
			if i > 0 {
				log.Debugf("Successfully got block hash at height %d after %d retries",
					height, i)
			}
			break
		}

		log.Debugf("GetBlockHash attempt %d/%d failed for height %d: %v",
			i+1, maxRetries, height, err)

		// If this isn't the last retry, wait before trying again.
		if i < maxRetries-1 {
			log.Debugf("Retrying GetBlockHash for height %d in %v",
				height, retryDelay)
			time.Sleep(retryDelay)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get block hash at height %d after %d retries: %w",
			height, maxRetries, err)
	}

	hash, err := chainhash.NewHashFromStr(hashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse block hash: %w", err)
	}

	// Cache the result.
	c.heightToHashMtx.Lock()
	c.heightToHash[int32(height)] = hash
	c.heightToHashMtx.Unlock()

	return hash, nil
}

// GetBlockHeader returns the block header for the given hash.
func (c *ChainClient) GetBlockHeader(hash *chainhash.Hash) (*wire.BlockHeader, error) {
	// Check cache first.
	c.headerCacheMtx.RLock()
	if header, ok := c.headerCache[*hash]; ok {
		c.headerCacheMtx.RUnlock()
		return header, nil
	}
	c.headerCacheMtx.RUnlock()

	// Retry logic to handle race condition where esplora hasn't indexed
	// the block yet.
	const maxRetries = 5
	const retryDelay = 500 * time.Millisecond

	var header *wire.BlockHeader
	var err error

	for i := 0; i < maxRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
		header, err = c.client.GetBlockHeader(ctx, hash.String())
		cancel()

		if err == nil {
			break
		}

		// If this isn't the last retry, wait before trying again.
		if i < maxRetries-1 {
			log.Debugf("Block header not found for %s, retrying in %v (attempt %d/%d)",
				hash.String(), retryDelay, i+1, maxRetries)
			time.Sleep(retryDelay)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get block header after %d retries: %w",
			maxRetries, err)
	}

	// Cache the header.
	c.headerCacheMtx.Lock()
	c.headerCache[*hash] = header
	c.headerCacheMtx.Unlock()

	return header, nil
}

// IsCurrent returns true if the chain client believes it is synced with the
// network.
func (c *ChainClient) IsCurrent() bool {
	bestHash, _, err := c.GetBestBlock()
	if err != nil {
		return false
	}

	bestHeader, err := c.GetBlockHeader(bestHash)
	if err != nil {
		return false
	}

	// Consider ourselves current if the best block is within 2 hours.
	return time.Since(bestHeader.Timestamp) < 2*time.Hour
}

// FilterBlocks scans the blocks contained in the FilterBlocksRequest for any
// addresses of interest.
func (c *ChainClient) FilterBlocks(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var (
		relevantTxns  []*wire.MsgTx
		batchIndex    uint32
		foundRelevant bool
	)

	// Check each watched address for activity in the requested blocks.
	for _, addr := range req.ExternalAddrs {
		txns, idx, err := c.filterAddressInBlocks(ctx, addr, req.Blocks)
		if err != nil {
			log.Warnf("Failed to filter address %s: %v", addr, err)
			continue
		}

		if len(txns) > 0 {
			relevantTxns = append(relevantTxns, txns...)
			if !foundRelevant || idx < batchIndex {
				batchIndex = idx
			}
			foundRelevant = true
		}
	}

	for _, addr := range req.InternalAddrs {
		txns, idx, err := c.filterAddressInBlocks(ctx, addr, req.Blocks)
		if err != nil {
			log.Warnf("Failed to filter address %s: %v", addr, err)
			continue
		}

		if len(txns) > 0 {
			relevantTxns = append(relevantTxns, txns...)
			if !foundRelevant || idx < batchIndex {
				batchIndex = idx
			}
			foundRelevant = true
		}
	}

	if !foundRelevant {
		return nil, nil
	}

	return &chain.FilterBlocksResponse{
		BatchIndex:   batchIndex,
		BlockMeta:    req.Blocks[batchIndex],
		RelevantTxns: relevantTxns,
	}, nil
}

// filterAddressInBlocks checks if an address has any activity in the given blocks.
func (c *ChainClient) filterAddressInBlocks(ctx context.Context,
	addr btcutil.Address,
	blocks []wtxmgr.BlockMeta) ([]*wire.MsgTx, uint32, error) {

	addrStr := addr.EncodeAddress()

	txs, err := c.client.GetAddressTxs(ctx, addrStr)
	if err != nil {
		return nil, 0, err
	}

	var (
		relevantTxns []*wire.MsgTx
		batchIdx     uint32 = ^uint32(0)
	)

	for _, txInfo := range txs {
		if !txInfo.Status.Confirmed {
			continue
		}

		// Check if this height falls within any of our blocks.
		for i, block := range blocks {
			if txInfo.Status.BlockHeight == int64(block.Height) {
				// Fetch the full transaction.
				tx, err := c.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
				if err != nil {
					continue
				}

				relevantTxns = append(relevantTxns, tx)
				if uint32(i) < batchIdx {
					batchIdx = uint32(i)
				}
				break
			}
		}
	}

	return relevantTxns, batchIdx, nil
}

// BlockStamp returns the latest block notified by the client.
func (c *ChainClient) BlockStamp() (*waddrmgr.BlockStamp, error) {
	c.bestBlockMtx.RLock()
	defer c.bestBlockMtx.RUnlock()

	return &c.bestBlock, nil
}

// SendRawTransaction submits the encoded transaction to the server.
func (c *ChainClient) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	return c.client.BroadcastTx(ctx, tx)
}

// GetUtxo returns the transaction output identified by the given outpoint.
func (c *ChainClient) GetUtxo(op *wire.OutPoint, pkScript []byte,
	heightHint uint32, cancel <-chan struct{}) (*wire.TxOut, error) {

	ctx, ctxCancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer ctxCancel()

	// Check if the output is spent.
	outSpend, err := c.client.GetTxOutSpend(ctx, op.Hash.String(), op.Index)
	if err != nil {
		return nil, fmt.Errorf("failed to check output spend status: %w", err)
	}

	if outSpend.Spent {
		return nil, ErrOutputSpent
	}

	// Fetch the transaction to get the output value.
	tx, err := c.client.GetTransaction(ctx, op.Hash.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction: %w", err)
	}

	if int(op.Index) >= len(tx.Vout) {
		return nil, ErrOutputNotFound
	}

	vout := tx.Vout[op.Index]

	return &wire.TxOut{
		Value:    vout.Value,
		PkScript: pkScript,
	}, nil
}

// Rescan rescans from the specified height for addresses.
func (c *ChainClient) Rescan(blockHash *chainhash.Hash, addrs []btcutil.Address,
	outpoints map[wire.OutPoint]btcutil.Address) error {

	log.Infof("Rescan called for %d addresses, %d outpoints",
		len(addrs), len(outpoints))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Get the starting height.
	var startHeight int32
	if blockHash != nil {
		// Find height from hash.
		c.headerCacheMtx.RLock()
		for h, cachedHash := range c.heightToHash {
			if cachedHash.IsEqual(blockHash) {
				startHeight = h
				break
			}
		}
		c.headerCacheMtx.RUnlock()
	}

	// Scan each address for historical transactions.
	for _, addr := range addrs {
		if err := c.scanAddressHistory(ctx, addr, startHeight); err != nil {
			log.Warnf("Failed to scan address %s: %v", addr, err)
		}
	}

	// Add addresses to watch list for future monitoring.
	c.watchedAddrsMtx.Lock()
	for _, addr := range addrs {
		c.watchedAddrs[addr.EncodeAddress()] = addr
	}
	c.watchedAddrsMtx.Unlock()

	// Add outpoints to watch list.
	c.watchedOutpointsMtx.Lock()
	for op, addr := range outpoints {
		c.watchedOutpoints[op] = addr
	}
	c.watchedOutpointsMtx.Unlock()

	// Send rescan finished notification.
	c.bestBlockMtx.RLock()
	bestBlock := c.bestBlock
	c.bestBlockMtx.RUnlock()

	c.notificationChan <- &chain.RescanFinished{
		Hash:   &bestBlock.Hash,
		Height: bestBlock.Height,
		Time:   bestBlock.Timestamp,
	}

	return nil
}

// scanAddressHistory scans an address for historical transactions.
func (c *ChainClient) scanAddressHistory(ctx context.Context,
	addr btcutil.Address, startHeight int32) error {

	addrStr := addr.EncodeAddress()

	txs, err := c.client.GetAddressTxs(ctx, addrStr)
	if err != nil {
		return fmt.Errorf("failed to get address history: %w", err)
	}

	log.Debugf("Found %d transactions for address %s", len(txs), addrStr)

	for _, txInfo := range txs {
		if !txInfo.Status.Confirmed {
			continue
		}

		if int32(txInfo.Status.BlockHeight) < startHeight {
			continue
		}

		// Fetch the full transaction.
		tx, err := c.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
		if err != nil {
			log.Warnf("Failed to fetch tx %s: %v", txInfo.TxID, err)
			continue
		}

		blockHash, err := chainhash.NewHashFromStr(txInfo.Status.BlockHash)
		if err != nil {
			continue
		}

		// Send relevant transaction notification.
		c.notificationChan <- chain.RelevantTx{
			TxRecord: &wtxmgr.TxRecord{
				MsgTx:        *tx,
				Hash:         tx.TxHash(),
				Received:     time.Unix(txInfo.Status.BlockTime, 0),
				SerializedTx: nil,
			},
			Block: &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{
					Hash:   *blockHash,
					Height: int32(txInfo.Status.BlockHeight),
				},
				Time: time.Unix(txInfo.Status.BlockTime, 0),
			},
		}
	}

	return nil
}

// NotifyReceived marks an address for transaction notifications.
func (c *ChainClient) NotifyReceived(addrs []btcutil.Address) error {
	log.Infof("NotifyReceived called with %d addresses", len(addrs))

	c.watchedAddrsMtx.Lock()
	for _, addr := range addrs {
		c.watchedAddrs[addr.EncodeAddress()] = addr
	}
	c.watchedAddrsMtx.Unlock()

	// Scan addresses for existing transactions in the background.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		for _, addr := range addrs {
			if err := c.scanAddressForExistingTxs(ctx, addr); err != nil {
				log.Debugf("Error scanning address %s: %v",
					addr.EncodeAddress(), err)
			}
		}
	}()

	return nil
}

// scanAddressForExistingTxs scans an address for existing transactions.
func (c *ChainClient) scanAddressForExistingTxs(ctx context.Context,
	addr btcutil.Address) error {

	addrStr := addr.EncodeAddress()

	txs, err := c.client.GetAddressTxs(ctx, addrStr)
	if err != nil {
		return fmt.Errorf("failed to get address transactions: %w", err)
	}

	if len(txs) == 0 {
		return nil
	}

	log.Debugf("Found %d existing transactions for address %s",
		len(txs), addrStr)

	for _, txInfo := range txs {
		// Fetch the full transaction.
		tx, err := c.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
		if err != nil {
			log.Warnf("Failed to fetch tx %s: %v", txInfo.TxID, err)
			continue
		}

		rec := &wtxmgr.TxRecord{
			MsgTx:    *tx,
			Hash:     tx.TxHash(),
			Received: time.Now(),
		}

		var blockMeta *wtxmgr.BlockMeta
		if txInfo.Status.Confirmed {
			blockHash, err := chainhash.NewHashFromStr(txInfo.Status.BlockHash)
			if err == nil {
				blockMeta = &wtxmgr.BlockMeta{
					Block: wtxmgr.Block{
						Hash:   *blockHash,
						Height: int32(txInfo.Status.BlockHeight),
					},
					Time: time.Unix(txInfo.Status.BlockTime, 0),
				}
			}
		}

		c.notificationChan <- chain.RelevantTx{
			TxRecord: rec,
			Block:    blockMeta,
		}
	}

	return nil
}

// NotifyBlocks enables block notifications.
func (c *ChainClient) NotifyBlocks() error {
	c.notifyBlocks.Store(true)
	return nil
}

// Notifications returns a channel of notifications from the chain client.
func (c *ChainClient) Notifications() <-chan interface{} {
	return c.notificationChan
}

// BackEnd returns the name of the driver.
func (c *ChainClient) BackEnd() string {
	return esploraBackendName
}

// TestMempoolAccept is not supported by Esplora.
func (c *ChainClient) TestMempoolAccept(txns []*wire.MsgTx,
	maxFeeRate float64) ([]*btcjson.TestMempoolAcceptResult, error) {

	// Esplora doesn't support mempool acceptance testing.
	// Return ErrBackendVersion to trigger the fallback to direct publish.
	return nil, rpcclient.ErrBackendVersion
}

// MapRPCErr maps errors from the RPC client to equivalent errors in the
// btcjson package.
func (c *ChainClient) MapRPCErr(err error) error {
	return err
}

// notificationHandler processes block notifications and dispatches them.
func (c *ChainClient) notificationHandler() {
	defer c.wg.Done()

	blockNotifs, subID := c.client.Subscribe()
	c.subscriptionID = subID

	defer c.client.Unsubscribe(subID)

	for {
		select {
		case <-c.quit:
			return

		case blockInfo, ok := <-blockNotifs:
			if !ok {
				return
			}
			c.handleNewBlock(blockInfo)
		}
	}
}

// handleNewBlock processes a new block notification.
// It ensures all blocks are processed sequentially by fetching any missing
// intermediate blocks before processing the new one.
func (c *ChainClient) handleNewBlock(blockInfo *BlockInfo) {
	newHeight := int32(blockInfo.Height)

	// Get the last processed height.
	c.bestBlockMtx.RLock()
	lastHeight := c.lastProcessedHeight
	c.bestBlockMtx.RUnlock()

	// If we're behind, we need to catch up by processing each block sequentially.
	// This ensures btcwallet receives all blocks in order.
	if newHeight > lastHeight+1 {
		log.Debugf("Catching up from height %d to %d", lastHeight+1, newHeight)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		for h := lastHeight + 1; h < newHeight; h++ {
			if err := c.processBlockAtHeight(ctx, h); err != nil {
				log.Errorf("Failed to process block at height %d: %v", h, err)
				// Continue anyway - the next poll will try again
				return
			}
		}
	}

	// Now process the actual block we received.
	hash, err := chainhash.NewHashFromStr(blockInfo.ID)
	if err != nil {
		log.Errorf("Failed to parse block hash: %v", err)
		return
	}

	// Update best block and last processed height.
	c.bestBlockMtx.Lock()
	c.bestBlock = waddrmgr.BlockStamp{
		Height:    newHeight,
		Hash:      *hash,
		Timestamp: time.Unix(blockInfo.Timestamp, 0),
	}
	c.lastProcessedHeight = newHeight
	c.bestBlockMtx.Unlock()

	// Cache height to hash mapping.
	c.heightToHashMtx.Lock()
	c.heightToHash[newHeight] = hash
	c.heightToHashMtx.Unlock()

	log.Debugf("New block: height=%d hash=%s", blockInfo.Height, blockInfo.ID)

	// Send block connected notification if enabled.
	if c.notifyBlocks.Load() {
		c.notificationChan <- chain.BlockConnected{
			Block: wtxmgr.Block{
				Hash:   *hash,
				Height: newHeight,
			},
			Time: time.Unix(blockInfo.Timestamp, 0),
		}
	}

	// Check watched addresses for new activity.
	c.checkWatchedAddresses(newHeight)
}

// processBlockAtHeight fetches and processes a block at the given height.
func (c *ChainClient) processBlockAtHeight(ctx context.Context, height int32) error {
	hashStr, err := c.client.GetBlockHashByHeight(ctx, int64(height))
	if err != nil {
		return fmt.Errorf("failed to get block hash: %w", err)
	}

	hash, err := chainhash.NewHashFromStr(hashStr)
	if err != nil {
		return fmt.Errorf("failed to parse block hash: %w", err)
	}

	header, err := c.client.GetBlockHeader(ctx, hashStr)
	if err != nil {
		return fmt.Errorf("failed to get block header: %w", err)
	}

	// Update state.
	c.bestBlockMtx.Lock()
	c.bestBlock = waddrmgr.BlockStamp{
		Height:    height,
		Hash:      *hash,
		Timestamp: header.Timestamp,
	}
	c.lastProcessedHeight = height
	c.bestBlockMtx.Unlock()

	// Cache the header and height mapping.
	c.cacheHeader(height, hash, header)

	log.Debugf("Processed intermediate block: height=%d hash=%s", height, hashStr)

	// Send block connected notification if enabled.
	if c.notifyBlocks.Load() {
		c.notificationChan <- chain.BlockConnected{
			Block: wtxmgr.Block{
				Hash:   *hash,
				Height: height,
			},
			Time: header.Timestamp,
		}
	}

	// Check watched addresses for new activity.
	c.checkWatchedAddresses(height)

	return nil
}

// checkWatchedAddresses checks if any watched addresses have new activity.
func (c *ChainClient) checkWatchedAddresses(height int32) {
	c.watchedAddrsMtx.RLock()
	addrs := make([]btcutil.Address, 0, len(c.watchedAddrs))
	for _, addr := range c.watchedAddrs {
		addrs = append(addrs, addr)
	}
	c.watchedAddrsMtx.RUnlock()

	if len(addrs) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	for _, addr := range addrs {
		txs, err := c.client.GetAddressTxs(ctx, addr.EncodeAddress())
		if err != nil {
			continue
		}

		for _, txInfo := range txs {
			if !txInfo.Status.Confirmed {
				continue
			}
			if txInfo.Status.BlockHeight != int64(height) {
				continue
			}

			// New transaction at this height.
			tx, err := c.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
			if err != nil {
				continue
			}

			blockHash, err := chainhash.NewHashFromStr(txInfo.Status.BlockHash)
			if err != nil {
				continue
			}

			c.notificationChan <- chain.RelevantTx{
				TxRecord: &wtxmgr.TxRecord{
					MsgTx:    *tx,
					Hash:     tx.TxHash(),
					Received: time.Unix(txInfo.Status.BlockTime, 0),
				},
				Block: &wtxmgr.BlockMeta{
					Block: wtxmgr.Block{
						Hash:   *blockHash,
						Height: int32(txInfo.Status.BlockHeight),
					},
					Time: time.Unix(txInfo.Status.BlockTime, 0),
				},
			}
		}
	}
}

// cacheHeader caches a block header.
func (c *ChainClient) cacheHeader(height int32, hash *chainhash.Hash,
	header *wire.BlockHeader) {

	c.headerCacheMtx.Lock()
	c.headerCache[*hash] = header
	c.headerCacheMtx.Unlock()

	c.heightToHashMtx.Lock()
	c.heightToHash[height] = hash
	c.heightToHashMtx.Unlock()
}

// scriptFromAddress creates a pkScript from an address.
func scriptFromAddress(addr btcutil.Address, params *chaincfg.Params) ([]byte, error) {
	return txscript.PayToAddrScript(addr)
}
