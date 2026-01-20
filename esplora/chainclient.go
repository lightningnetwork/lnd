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

	// progress logging for long rescans/sync.
	progressMtx        sync.Mutex
	lastProgressLog    time.Time
	lastProgressHeight int64

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
		start := time.Now()
		hashStr, err = c.client.GetBlockHashByHeight(ctx, height)
		cancel()

		if err == nil {
			c.maybeLogProgress(height)
			if dur := time.Since(start); dur > 2*time.Second {
				log.Warnf("Slow GetBlockHash height=%d took %v", height, dur)
			}
			if i > 0 {
				log.Debugf("Successfully got block hash at height %d after %d retries",
					height, i)
			}
			break
		}

		if dur := time.Since(start); dur > 2*time.Second {
			log.Warnf("Slow GetBlockHash height=%d failed after %v: %v",
				height, dur, err)
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
		start := time.Now()
		header, err = c.client.GetBlockHeader(ctx, hash.String())
		cancel()

		if err == nil {
			if dur := time.Since(start); dur > 2*time.Second {
				log.Warnf("Slow GetBlockHeader hash=%s took %v", hash.String(), dur)
			}
			break
		}

		if dur := time.Since(start); dur > 2*time.Second {
			log.Warnf("Slow GetBlockHeader hash=%s failed after %v: %v",
				hash.String(), dur, err)
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

// maybeLogProgress logs periodic progress during long scans.
func (c *ChainClient) maybeLogProgress(height int64) {
	const (
		progressEvery    = int64(500)
		progressInterval = 30 * time.Second
	)

	now := time.Now()

	c.progressMtx.Lock()
	defer c.progressMtx.Unlock()

	if c.lastProgressLog.IsZero() {
		c.lastProgressLog = now
		c.lastProgressHeight = height
		return
	}

	heightDelta := height - c.lastProgressHeight
	timeDelta := now.Sub(c.lastProgressLog)
	if heightDelta < 0 {
		// Reset baseline if height moves backward (e.g. birthday search).
		c.lastProgressLog = now
		c.lastProgressHeight = height
		return
	}
	if heightDelta < progressEvery && timeDelta < progressInterval {
		return
	}

	rate := float64(heightDelta) / timeDelta.Seconds()
	log.Infof("Esplora sync progress: height=%d (+%d in %s, %.2f blk/s)",
		height, heightDelta, timeDelta.Round(time.Second), rate)

	c.lastProgressLog = now
	c.lastProgressHeight = height
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

// filterBlocksAddressThreshold is the number of addresses above which we switch
// from per-address API queries to block-based scanning. Block-based scanning
// fetches each block's transactions and scans them locally, which is much more
// efficient when there are many addresses to check.
const filterBlocksAddressThreshold = 500

// FilterBlocks scans the blocks contained in the FilterBlocksRequest for any
// addresses of interest.
func (c *ChainClient) FilterBlocks(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	totalAddrs := len(req.ExternalAddrs) + len(req.InternalAddrs)

	log.Tracef("FilterBlocks called: %d external addrs, %d internal addrs, %d blocks",
		len(req.ExternalAddrs), len(req.InternalAddrs), len(req.Blocks))

	// Use block-based scanning for large address sets (e.g., during wallet recovery).
	// This is much more efficient than querying each address individually.
	if totalAddrs > filterBlocksAddressThreshold {
		log.Infof("FilterBlocks: using block-based scanning for %d addresses across %d blocks",
			totalAddrs, len(req.Blocks))
		return c.filterBlocksByScanning(req)
	}

	// For small address sets, use per-address queries.
	return c.filterBlocksByAddress(req)
}

// filterBlocksByAddress filters blocks by querying each address individually.
// This is efficient for small address sets but slow for large ones.
func (c *ChainClient) filterBlocksByAddress(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var (
		relevantTxns       []*wire.MsgTx
		batchIndex         uint32
		foundRelevant      bool
		foundExternalAddrs = make(map[waddrmgr.KeyScope]map[uint32]struct{})
		foundInternalAddrs = make(map[waddrmgr.KeyScope]map[uint32]struct{})
		foundOutPoints     = make(map[wire.OutPoint]btcutil.Address)
	)

	// Check each watched external address for activity in the requested blocks.
	for scopedIdx, addr := range req.ExternalAddrs {
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

			if foundExternalAddrs[scopedIdx.Scope] == nil {
				foundExternalAddrs[scopedIdx.Scope] = make(map[uint32]struct{})
			}
			foundExternalAddrs[scopedIdx.Scope][scopedIdx.Index] = struct{}{}

			for _, tx := range txns {
				for i, txOut := range tx.TxOut {
					_, addrs, _, err := txscript.ExtractPkScriptAddrs(
						txOut.PkScript, c.chainParams,
					)
					if err != nil {
						continue
					}
					for _, a := range addrs {
						if a.EncodeAddress() == addr.EncodeAddress() {
							op := wire.OutPoint{
								Hash:  tx.TxHash(),
								Index: uint32(i),
							}
							foundOutPoints[op] = addr
						}
					}
				}
			}

			log.Tracef("FilterBlocks: found %d txs for external addr %s (scope=%v, index=%d)",
				len(txns), addr.EncodeAddress(), scopedIdx.Scope, scopedIdx.Index)
		}
	}

	// Check each watched internal address for activity in the requested blocks.
	for scopedIdx, addr := range req.InternalAddrs {
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

			if foundInternalAddrs[scopedIdx.Scope] == nil {
				foundInternalAddrs[scopedIdx.Scope] = make(map[uint32]struct{})
			}
			foundInternalAddrs[scopedIdx.Scope][scopedIdx.Index] = struct{}{}

			for _, tx := range txns {
				for i, txOut := range tx.TxOut {
					_, addrs, _, err := txscript.ExtractPkScriptAddrs(
						txOut.PkScript, c.chainParams,
					)
					if err != nil {
						continue
					}
					for _, a := range addrs {
						if a.EncodeAddress() == addr.EncodeAddress() {
							op := wire.OutPoint{
								Hash:  tx.TxHash(),
								Index: uint32(i),
							}
							foundOutPoints[op] = addr
						}
					}
				}
			}

			log.Tracef("FilterBlocks: found %d txs for internal addr %s (scope=%v, index=%d)",
				len(txns), addr.EncodeAddress(), scopedIdx.Scope, scopedIdx.Index)
		}
	}

	if !foundRelevant {
		return nil, nil
	}

	log.Debugf("FilterBlocks: found %d relevant txns at block height %d",
		len(relevantTxns), req.Blocks[batchIndex].Height)

	return &chain.FilterBlocksResponse{
		BatchIndex:         batchIndex,
		BlockMeta:          req.Blocks[batchIndex],
		FoundExternalAddrs: foundExternalAddrs,
		FoundInternalAddrs: foundInternalAddrs,
		FoundOutPoints:     foundOutPoints,
		RelevantTxns:       relevantTxns,
	}, nil
}

// maxConcurrentBlockFetches is the maximum number of concurrent block fetches.
const maxConcurrentBlockFetches = 5

// filterBlocksByScanning filters blocks by fetching each block's transactions
// and scanning them locally against the watched address set. This is much more
// efficient than per-address queries when there are many addresses.
func (c *ChainClient) filterBlocksByScanning(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	// Use a longer timeout for block scanning since we may need to fetch many blocks.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Build address lookup maps for O(1) matching.
	// Map from address string to ScopedIndex for quick lookup.
	externalAddrMap := make(map[string]waddrmgr.ScopedIndex)
	for scopedIdx, addr := range req.ExternalAddrs {
		externalAddrMap[addr.EncodeAddress()] = scopedIdx
	}

	internalAddrMap := make(map[string]waddrmgr.ScopedIndex)
	for scopedIdx, addr := range req.InternalAddrs {
		internalAddrMap[addr.EncodeAddress()] = scopedIdx
	}

	// Pre-fetch all block transactions in parallel for better performance.
	type blockTxsResult struct {
		blockIdx int
		txs      []*wire.MsgTx
		err      error
	}

	log.Infof("FilterBlocks: pre-fetching transactions for %d blocks...", len(req.Blocks))

	blockTxsChan := make(chan blockTxsResult, len(req.Blocks))
	blockSemaphore := make(chan struct{}, maxConcurrentBlockFetches)

	var fetchWg sync.WaitGroup
	for i, blockMeta := range req.Blocks {
		fetchWg.Add(1)
		go func(idx int, meta wtxmgr.BlockMeta) {
			defer fetchWg.Done()

			// Acquire semaphore.
			select {
			case blockSemaphore <- struct{}{}:
				defer func() { <-blockSemaphore }()
			case <-ctx.Done():
				blockTxsChan <- blockTxsResult{blockIdx: idx, err: ctx.Err()}
				return
			}

			txs, err := c.getBlockTransactions(ctx, &meta.Hash)
			blockTxsChan <- blockTxsResult{blockIdx: idx, txs: txs, err: err}

			// Log progress for long operations.
			if (idx+1)%100 == 0 {
				log.Infof("FilterBlocks: fetched %d/%d blocks", idx+1, len(req.Blocks))
			}
		}(i, blockMeta)
	}

	go func() {
		fetchWg.Wait()
		close(blockTxsChan)
	}()

	// Collect all block transactions.
	allBlockTxs := make(map[int][]*wire.MsgTx)
	for result := range blockTxsChan {
		if result.err != nil {
			log.Warnf("FilterBlocks: failed to get transactions for block %d: %v",
				result.blockIdx, result.err)
			continue
		}
		allBlockTxs[result.blockIdx] = result.txs
	}

	log.Infof("FilterBlocks: finished fetching transactions, scanning %d blocks...", len(allBlockTxs))

	var (
		relevantTxns       []*wire.MsgTx
		batchIndex         uint32
		foundRelevant      bool
		foundExternalAddrs = make(map[waddrmgr.KeyScope]map[uint32]struct{})
		foundInternalAddrs = make(map[waddrmgr.KeyScope]map[uint32]struct{})
		foundOutPoints     = make(map[wire.OutPoint]btcutil.Address)
		seenTxs            = make(map[chainhash.Hash]struct{})
	)

	// Process blocks sequentially (order matters for finding earliest match).
	for blockIdx, blockMeta := range req.Blocks {
		blockTxs, ok := allBlockTxs[blockIdx]
		if !ok {
			continue
		}
		// Scan each transaction for watched addresses and spent outpoints.
		for _, tx := range blockTxs {
			txHash := tx.TxHash()
			txIsRelevant := false

			// First, check inputs to see if they spend any watched outpoints.
			// This detects when our UTXOs are spent.
			for _, txIn := range tx.TxIn {
				// Check if this input spends a watched outpoint.
				if addr, ok := req.WatchedOutPoints[txIn.PreviousOutPoint]; ok {
					txIsRelevant = true
					log.Infof("FilterBlocks: found spend of watched outpoint %v (addr=%s) in block %d",
						txIn.PreviousOutPoint, addr.EncodeAddress(), blockMeta.Height)
				}
				// Check if this input spends an outpoint we found in this scan.
				if addr, ok := foundOutPoints[txIn.PreviousOutPoint]; ok {
					txIsRelevant = true
					log.Infof("FilterBlocks: found spend of found outpoint %v (addr=%s) in block %d",
						txIn.PreviousOutPoint, addr.EncodeAddress(), blockMeta.Height)
				}
			}

			// Check outputs for watched addresses.
			for i, txOut := range tx.TxOut {
				_, addrs, _, err := txscript.ExtractPkScriptAddrs(
					txOut.PkScript, c.chainParams,
				)
				if err != nil {
					continue
				}

				for _, addr := range addrs {
					addrStr := addr.EncodeAddress()

					// Check external addresses.
					if scopedIdx, ok := externalAddrMap[addrStr]; ok {
						txIsRelevant = true

						if foundExternalAddrs[scopedIdx.Scope] == nil {
							foundExternalAddrs[scopedIdx.Scope] = make(map[uint32]struct{})
						}
						foundExternalAddrs[scopedIdx.Scope][scopedIdx.Index] = struct{}{}

						op := wire.OutPoint{Hash: txHash, Index: uint32(i)}
						foundOutPoints[op] = req.ExternalAddrs[scopedIdx]

						log.Infof("FilterBlocks: found output for external addr %s (scope=%v, index=%d) in block %d, value=%d",
							addrStr, scopedIdx.Scope, scopedIdx.Index, blockMeta.Height, txOut.Value)
					}

					// Check internal addresses.
					if scopedIdx, ok := internalAddrMap[addrStr]; ok {
						txIsRelevant = true

						if foundInternalAddrs[scopedIdx.Scope] == nil {
							foundInternalAddrs[scopedIdx.Scope] = make(map[uint32]struct{})
						}
						foundInternalAddrs[scopedIdx.Scope][scopedIdx.Index] = struct{}{}

						op := wire.OutPoint{Hash: txHash, Index: uint32(i)}
						foundOutPoints[op] = req.InternalAddrs[scopedIdx]

						log.Infof("FilterBlocks: found output for internal addr %s (scope=%v, index=%d) in block %d, value=%d",
							addrStr, scopedIdx.Scope, scopedIdx.Index, blockMeta.Height, txOut.Value)
					}
				}
			}

			// If this transaction is relevant, add it to results.
			if txIsRelevant {
				if _, seen := seenTxs[txHash]; !seen {
					relevantTxns = append(relevantTxns, tx)
					seenTxs[txHash] = struct{}{}
				}

				if !foundRelevant || uint32(blockIdx) < batchIndex {
					batchIndex = uint32(blockIdx)
				}
				foundRelevant = true
			}
		}

		// Log progress every 50 blocks.
		if (blockIdx+1)%50 == 0 || blockIdx == len(req.Blocks)-1 {
			log.Infof("FilterBlocks: scanned %d/%d blocks, found %d relevant txns",
				blockIdx+1, len(req.Blocks), len(relevantTxns))
		}
	}

	if !foundRelevant {
		log.Infof("FilterBlocks: no relevant transactions found in %d blocks",
			len(req.Blocks))
		return nil, nil
	}

	log.Infof("FilterBlocks: found %d relevant txns, earliest at block height %d",
		len(relevantTxns), req.Blocks[batchIndex].Height)

	return &chain.FilterBlocksResponse{
		BatchIndex:         batchIndex,
		BlockMeta:          req.Blocks[batchIndex],
		FoundExternalAddrs: foundExternalAddrs,
		FoundInternalAddrs: foundInternalAddrs,
		FoundOutPoints:     foundOutPoints,
		RelevantTxns:       relevantTxns,
	}, nil
}

// maxConcurrentTxFetches is the maximum number of concurrent transaction fetches.
const maxConcurrentTxFetches = 10

// getBlockTransactions fetches all transactions for a block using parallel fetching.
func (c *ChainClient) getBlockTransactions(ctx context.Context,
	blockHash *chainhash.Hash) ([]*wire.MsgTx, error) {

	// Get transaction IDs for the block.
	txids, err := c.client.GetBlockTxIDs(ctx, blockHash.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get block txids: %w", err)
	}

	if len(txids) == 0 {
		return nil, nil
	}

	// For small blocks, fetch sequentially to avoid overhead.
	if len(txids) <= 2 {
		txs := make([]*wire.MsgTx, 0, len(txids))
		for _, txid := range txids {
			tx, err := c.client.GetRawTransactionMsgTx(ctx, txid)
			if err != nil {
				log.Warnf("Failed to get tx %s: %v", txid, err)
				continue
			}
			txs = append(txs, tx)
		}
		return txs, nil
	}

	// For larger blocks, fetch transactions in parallel.
	type txResult struct {
		index int
		tx    *wire.MsgTx
		err   error
	}

	results := make(chan txResult, len(txids))
	semaphore := make(chan struct{}, maxConcurrentTxFetches)

	var wg sync.WaitGroup
	for i, txid := range txids {
		wg.Add(1)
		go func(idx int, id string) {
			defer wg.Done()

			// Acquire semaphore.
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- txResult{index: idx, err: ctx.Err()}
				return
			}

			tx, err := c.client.GetRawTransactionMsgTx(ctx, id)
			results <- txResult{index: idx, tx: tx, err: err}
		}(i, txid)
	}

	// Wait for all goroutines to complete.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results maintaining order.
	txsByIndex := make(map[int]*wire.MsgTx)
	for result := range results {
		if result.err != nil {
			log.Warnf("Failed to get tx at index %d: %v", result.index, result.err)
			continue
		}
		if result.tx != nil {
			txsByIndex[result.index] = result.tx
		}
	}

	// Build ordered slice.
	txs := make([]*wire.MsgTx, 0, len(txsByIndex))
	for i := 0; i < len(txids); i++ {
		if tx, ok := txsByIndex[i]; ok {
			txs = append(txs, tx)
		}
	}

	return txs, nil
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

	// Build a map of block heights for quick lookup
	blockHeights := make(map[int32]int)
	for i, block := range blocks {
		blockHeights[block.Height] = i
	}

	for _, txInfo := range txs {
		if !txInfo.Status.Confirmed {
			continue
		}

		// Check if this height falls within any of our blocks.
		if idx, ok := blockHeights[int32(txInfo.Status.BlockHeight)]; ok {
			// Fetch the full transaction.
			tx, err := c.client.GetRawTransactionMsgTx(ctx, txInfo.TxID)
			if err != nil {
				continue
			}

			relevantTxns = append(relevantTxns, tx)
			if uint32(idx) < batchIdx {
				batchIdx = uint32(idx)
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
