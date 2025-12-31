package electrum

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
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

const (
	// electrumBackendName is the name of the Electrum backend.
	electrumBackendName = "electrum"

	// defaultRequestTimeout is the default timeout for Electrum requests.
	defaultRequestTimeout = 30 * time.Second
)

var (
	// ErrBlockNotFound is returned when a block cannot be found.
	ErrBlockNotFound = errors.New("block not found")

	// ErrFullBlocksNotSupported is returned when full block retrieval is
	// attempted but not supported by Electrum.
	ErrFullBlocksNotSupported = errors.New("electrum does not support " +
		"full block retrieval")

	// ErrNotImplemented is returned for operations not supported by
	// Electrum.
	ErrNotImplemented = errors.New("operation not implemented for " +
		"electrum backend")

	// ErrOutputSpent is returned when the requested output has been spent.
	ErrOutputSpent = errors.New("output has been spent")

	// ErrOutputNotFound is returned when the requested output cannot be
	// found.
	ErrOutputNotFound = errors.New("output not found")
)

// ChainClient is an implementation of chain.Interface that uses an Electrum
// server as its backend. Note that Electrum servers have limitations compared
// to full nodes - notably they cannot serve full block data.
type ChainClient struct {
	started int32
	stopped int32

	client *Client

	chainParams *chaincfg.Params

	// bestBlockMtx protects bestBlock.
	bestBlockMtx sync.RWMutex
	bestBlock    waddrmgr.BlockStamp

	// headerCache caches block headers by hash for efficient lookups.
	headerCacheMtx sync.RWMutex
	headerCache    map[chainhash.Hash]*wire.BlockHeader

	// heightToHash maps block heights to hashes.
	heightToHashMtx sync.RWMutex
	heightToHash    map[int32]*chainhash.Hash

	// notificationChan is used to send notifications to the wallet.
	notificationChan chan interface{}

	// notifyBlocks indicates whether we should send block notifications.
	notifyBlocks atomic.Bool

	// watchedAddresses contains addresses we're watching for activity.
	watchedAddrsMtx sync.RWMutex
	watchedAddrs    map[string]btcutil.Address

	// watchedOutpoints contains outpoints we're watching for spends.
	watchedOutpointsMtx sync.RWMutex
	watchedOutpoints    map[wire.OutPoint]btcutil.Address

	quit chan struct{}
	wg   sync.WaitGroup
}

// Compile time check to ensure ChainClient implements chain.Interface.
var _ chain.Interface = (*ChainClient)(nil)

// NewChainClient creates a new Electrum chain client.
func NewChainClient(client *Client,
	chainParams *chaincfg.Params) *ChainClient {

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
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) Start() error {
	if atomic.AddInt32(&c.started, 1) != 1 {
		return nil
	}

	log.Info("Starting Electrum chain client")

	// Ensure the underlying client is connected.
	if !c.client.IsConnected() {
		return ErrNotConnected
	}

	// Subscribe to headers using a background context that won't be
	// cancelled when Start() returns. The subscription needs to live for
	// the lifetime of the client.
	headerChan, err := c.client.SubscribeHeaders(context.Background())
	if err != nil {
		return fmt.Errorf("failed to subscribe to headers: %w", err)
	}

	// Get initial header with a timeout.
	select {
	case header := <-headerChan:
		ctx, cancel := context.WithTimeout(
			context.Background(), defaultRequestTimeout,
		)
		blockHeader, err := c.client.GetBlockHeader(
			ctx, uint32(header.Height),
		)
		cancel()
		if err != nil {
			return fmt.Errorf("failed to get initial header: %w",
				err)
		}

		hash := blockHeader.BlockHash()
		c.bestBlockMtx.Lock()
		c.bestBlock = waddrmgr.BlockStamp{
			Height:    int32(header.Height),
			Hash:      hash,
			Timestamp: blockHeader.Timestamp,
		}
		c.bestBlockMtx.Unlock()

		// Cache the header.
		c.cacheHeader(int32(header.Height), &hash, blockHeader)

	case <-time.After(defaultRequestTimeout):
		return errors.New("timeout waiting for initial header")
	}

	// Start the notification handler.
	c.wg.Add(1)
	go c.notificationHandler(headerChan)

	// Send ClientConnected notification first. This triggers the wallet to
	// start the sync process by calling syncWithChain.
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
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) Stop() {
	if atomic.AddInt32(&c.stopped, 1) != 1 {
		return
	}

	log.Info("Stopping Electrum chain client")

	close(c.quit)
	c.wg.Wait()

	close(c.notificationChan)
}

// WaitForShutdown blocks until the client has finished shutting down.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) WaitForShutdown() {
	c.wg.Wait()
}

// GetBestBlock returns the hash and height of the best known block.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) GetBestBlock() (*chainhash.Hash, int32, error) {
	c.bestBlockMtx.RLock()
	defer c.bestBlockMtx.RUnlock()

	hash := c.bestBlock.Hash
	return &hash, c.bestBlock.Height, nil
}

// GetBlock returns the raw block from the server given its hash.
//
// NOTE: Electrum servers do not serve full blocks. This method will return
// an error. Use GetBlockHeader for header-only queries.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	// Electrum servers cannot serve full blocks. This is a fundamental
	// limitation of the protocol.
	return nil, ErrFullBlocksNotSupported
}

// GetBlockHash returns the hash of the block at the given height.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) GetBlockHash(height int64) (*chainhash.Hash, error) {
	// Check cache first.
	c.heightToHashMtx.RLock()
	if hash, ok := c.heightToHash[int32(height)]; ok {
		c.heightToHashMtx.RUnlock()
		return hash, nil
	}
	c.heightToHashMtx.RUnlock()

	// Fetch from server.
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	header, err := c.client.GetBlockHeader(ctx, uint32(height))
	if err != nil {
		return nil, fmt.Errorf("failed to get block header at "+
			"height %d: %w", height, err)
	}

	hash := header.BlockHash()

	// Cache the result.
	c.cacheHeader(int32(height), &hash, header)

	return &hash, nil
}

// GetBlockHeader returns the block header for the given hash.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) GetBlockHeader(
	hash *chainhash.Hash) (*wire.BlockHeader, error) {

	// Check cache first.
	c.headerCacheMtx.RLock()
	if header, ok := c.headerCache[*hash]; ok {
		c.headerCacheMtx.RUnlock()
		return header, nil
	}
	c.headerCacheMtx.RUnlock()

	// We need to find the height for this hash. Search backwards from
	// best block.
	c.bestBlockMtx.RLock()
	bestHeight := c.bestBlock.Height
	c.bestBlockMtx.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Search for the block by iterating through recent heights.
	const maxSearchDepth = 1000
	startHeight := bestHeight
	if startHeight > maxSearchDepth {
		startHeight = bestHeight - maxSearchDepth
	} else {
		startHeight = 0
	}

	for height := bestHeight; height >= startHeight; height-- {
		header, err := c.client.GetBlockHeader(ctx, uint32(height))
		if err != nil {
			continue
		}

		headerHash := header.BlockHash()
		c.cacheHeader(height, &headerHash, header)

		if headerHash.IsEqual(hash) {
			return header, nil
		}

		if height == 0 {
			break
		}
	}

	return nil, ErrBlockNotFound
}

// IsCurrent returns true if the chain client believes it is synced with the
// network.
//
// NOTE: This is part of the chain.Interface interface.
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
// addresses of interest. For each requested block, the corresponding compact
// filter will first be checked for matches, skipping those that do not report
// anything. If the filter returns a positive match, the full block will be
// fetched and filtered for addresses using a block filterer.
//
// NOTE: For Electrum, we use scripthash queries instead of compact filters.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) FilterBlocks(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	// For Electrum, we can't scan full blocks. Instead, we query the
	// history for each watched address and check if any transactions
	// appeared in the requested block range.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var (
		relevantTxns  []*wire.MsgTx
		batchIndex    uint32
		foundRelevant bool
	)

	// Check each watched address for activity in the requested blocks.
	for _, addr := range req.ExternalAddrs {
		txns, idx, err := c.filterAddressInBlocks(
			ctx, addr, req.Blocks,
		)
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
		txns, idx, err := c.filterAddressInBlocks(
			ctx, addr, req.Blocks,
		)
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

// filterAddressInBlocks checks if an address has any activity in the given
// blocks.
func (c *ChainClient) filterAddressInBlocks(ctx context.Context,
	addr btcutil.Address,
	blocks []wtxmgr.BlockMeta) ([]*wire.MsgTx, uint32, error) {

	pkScript, err := scriptFromAddress(addr, c.chainParams)
	if err != nil {
		return nil, 0, err
	}

	scripthash := ScripthashFromScript(pkScript)

	history, err := c.client.GetHistory(ctx, scripthash)
	if err != nil {
		return nil, 0, err
	}

	var (
		relevantTxns []*wire.MsgTx
		batchIdx     uint32 = ^uint32(0)
	)

	for _, histItem := range history {
		if histItem.Height <= 0 {
			continue
		}

		// Check if this height falls within any of our blocks.
		for i, block := range blocks {
			if int32(histItem.Height) == block.Height {
				txHash, err := chainhash.NewHashFromStr(
					histItem.Hash,
				)
				if err != nil {
					continue
				}

				// Fetch the transaction.
				tx, err := c.client.GetTransactionMsgTx(
					ctx, txHash,
				)
				if err != nil {
					log.Warnf("Failed to get tx %s: %v",
						histItem.Hash, err)
					continue
				}

				relevantTxns = append(relevantTxns, tx)

				if uint32(i) < batchIdx {
					batchIdx = uint32(i)
				}
			}
		}
	}

	return relevantTxns, batchIdx, nil
}

// BlockStamp returns the latest block notified by the client.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) BlockStamp() (*waddrmgr.BlockStamp, error) {
	c.bestBlockMtx.RLock()
	defer c.bestBlockMtx.RUnlock()

	stamp := c.bestBlock
	return &stamp, nil
}

// SendRawTransaction submits a raw transaction to the server which will then
// relay it to the Bitcoin network.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	return c.client.BroadcastTx(ctx, tx)
}

// GetUtxo returns the original output referenced by the passed outpoint if it
// is still unspent. This uses Electrum's listunspent RPC to check if the
// output exists.
func (c *ChainClient) GetUtxo(op *wire.OutPoint, pkScript []byte,
	heightHint uint32, cancel <-chan struct{}) (*wire.TxOut, error) {

	// Convert the pkScript to a scripthash for Electrum query.
	scripthash := ScripthashFromScript(pkScript)

	ctx, ctxCancel := context.WithTimeout(
		context.Background(), defaultRequestTimeout,
	)
	defer ctxCancel()

	// Query unspent outputs for this scripthash.
	unspent, err := c.client.ListUnspent(ctx, scripthash)
	if err != nil {
		return nil, fmt.Errorf("failed to list unspent: %w", err)
	}

	// Search for our specific outpoint in the unspent list.
	for _, utxo := range unspent {
		if utxo.Hash == op.Hash.String() &&
			utxo.Position == op.Index {

			// Found the UTXO - it's unspent.
			return &wire.TxOut{
				Value:    int64(utxo.Value),
				PkScript: pkScript,
			}, nil
		}
	}

	// Not found in unspent list. Check if it exists at all by looking at
	// the transaction history.
	history, err := c.client.GetHistory(ctx, scripthash)
	if err != nil {
		return nil, fmt.Errorf("failed to get history: %w", err)
	}

	// Check if any transaction in history matches our outpoint's tx.
	for _, histItem := range history {
		if histItem.Hash == op.Hash.String() {
			// The transaction exists but the output is not in the
			// unspent list, meaning it has been spent.
			return nil, ErrOutputSpent
		}
	}

	// Output was never found.
	return nil, ErrOutputNotFound
}

// Rescan rescans the chain for transactions paying to the given addresses.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) Rescan(startHash *chainhash.Hash,
	addrs []btcutil.Address,
	outpoints map[wire.OutPoint]btcutil.Address) error {

	log.Infof("Starting rescan from block %s with %d addresses and "+
		"%d outpoints", startHash, len(addrs), len(outpoints))

	// Log all addresses being watched for debugging.
	for i, addr := range addrs {
		log.Debugf("Rescan address %d: %s", i, addr.EncodeAddress())
	}

	// Store watched addresses and outpoints.
	c.watchedAddrsMtx.Lock()
	for _, addr := range addrs {
		c.watchedAddrs[addr.EncodeAddress()] = addr
	}
	c.watchedAddrsMtx.Unlock()

	c.watchedOutpointsMtx.Lock()
	for op, addr := range outpoints {
		c.watchedOutpoints[op] = addr
	}
	c.watchedOutpointsMtx.Unlock()

	// Get the start height from the hash.
	startHeader, err := c.GetBlockHeader(startHash)
	if err != nil {
		return fmt.Errorf("failed to get start block header: %w", err)
	}

	// Get start height by searching for the hash.
	startHeight := int32(0)
	c.heightToHashMtx.RLock()
	for height, hash := range c.heightToHash {
		if hash.IsEqual(startHash) {
			startHeight = height
			break
		}
	}
	c.heightToHashMtx.RUnlock()

	// If we didn't find it, estimate from timestamp.
	if startHeight == 0 && startHeader != nil {
		c.bestBlockMtx.RLock()
		bestHeight := c.bestBlock.Height
		bestTime := c.bestBlock.Timestamp
		c.bestBlockMtx.RUnlock()

		// Rough estimate: 10 minutes per block.
		timeDiff := bestTime.Sub(startHeader.Timestamp)
		blockDiff := int32(timeDiff.Minutes() / 10)
		startHeight = bestHeight - blockDiff
		if startHeight < 0 {
			startHeight = 0
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Scan each address for history.
	for _, addr := range addrs {
		err := c.scanAddressHistory(ctx, addr, startHeight)
		if err != nil {
			log.Warnf("Failed to scan address %s: %v", addr, err)
		}
	}

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

// scanAddressHistory scans the history of an address from the given start
// height and sends relevant transaction notifications.
func (c *ChainClient) scanAddressHistory(ctx context.Context,
	addr btcutil.Address, startHeight int32) error {

	pkScript, err := scriptFromAddress(addr, c.chainParams)
	if err != nil {
		return err
	}

	scripthash := ScripthashFromScript(pkScript)

	log.Debugf("Scanning history for address %s (scripthash: %s) from height %d",
		addr.EncodeAddress(), scripthash, startHeight)

	history, err := c.client.GetHistory(ctx, scripthash)
	if err != nil {
		return err
	}

	log.Debugf("Found %d history items for address %s",
		len(history), addr.EncodeAddress())

	for _, histItem := range history {
		log.Debugf("History item: txid=%s height=%d",
			histItem.Hash, histItem.Height)

		// Skip unconfirmed and historical transactions.
		if histItem.Height <= 0 || int32(histItem.Height) < startHeight {
			log.Debugf("Skipping tx %s: height=%d < startHeight=%d or unconfirmed",
				histItem.Hash, histItem.Height, startHeight)
			continue
		}

		txHash, err := chainhash.NewHashFromStr(histItem.Hash)
		if err != nil {
			continue
		}

		tx, err := c.client.GetTransactionMsgTx(ctx, txHash)
		if err != nil {
			log.Warnf("Failed to get transaction %s: %v",
				histItem.Hash, err)
			continue
		}

		// Get block hash for this height.
		blockHash, err := c.GetBlockHash(int64(histItem.Height))
		if err != nil {
			log.Warnf("Failed to get block hash for height %d: %v",
				histItem.Height, err)
			continue
		}

		// Send relevant transaction notification.
		log.Infof("scanAddressHistory: Sending RelevantTx for tx %s at height %d for address %s",
			txHash, histItem.Height, addr.EncodeAddress())

		c.notificationChan <- chain.RelevantTx{
			TxRecord: &wtxmgr.TxRecord{
				MsgTx:    *tx,
				Hash:     *txHash,
				Received: time.Now(),
			},
			Block: &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{
					Hash:   *blockHash,
					Height: int32(histItem.Height),
				},
			},
		}

		log.Infof("scanAddressHistory: Successfully sent RelevantTx notification for tx %s", txHash)
	}

	return nil
}

// NotifyReceived marks the addresses to be monitored for incoming transactions.
// It also scans for any existing transactions to these addresses and sends
// notifications for them.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) NotifyReceived(addrs []btcutil.Address) error {
	log.Infof("NotifyReceived called with %d addresses", len(addrs))

	c.watchedAddrsMtx.Lock()
	for _, addr := range addrs {
		log.Debugf("Watching address: %s", addr.EncodeAddress())
		c.watchedAddrs[addr.EncodeAddress()] = addr
	}
	c.watchedAddrsMtx.Unlock()

	// Scan for existing activity on these addresses in a goroutine to avoid
	// blocking. This ensures that if funds were already sent to an address,
	// the wallet will be notified.
	go func() {
		log.Infof("Starting background scan for %d addresses", len(addrs))

		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Minute,
		)
		defer cancel()

		for _, addr := range addrs {
			select {
			case <-c.quit:
				return
			default:
			}

			log.Debugf("Scanning address %s for existing transactions",
				addr.EncodeAddress())

			if err := c.scanAddressForExistingTxs(ctx, addr); err != nil {
				log.Debugf("Failed to scan address %s: %v",
					addr.EncodeAddress(), err)
			}
		}

		log.Infof("Finished background scan for %d addresses", len(addrs))
	}()

	return nil
}

// scanAddressForExistingTxs scans the blockchain for existing transactions
// involving the given address and sends notifications for any found.
func (c *ChainClient) scanAddressForExistingTxs(ctx context.Context,
	addr btcutil.Address) error {

	pkScript, err := scriptFromAddress(addr, c.chainParams)
	if err != nil {
		return err
	}

	scripthash := ScripthashFromScript(pkScript)

	history, err := c.client.GetHistory(ctx, scripthash)
	if err != nil {
		return err
	}

	if len(history) == 0 {
		log.Debugf("No history found for address %s", addr.EncodeAddress())
		return nil
	}

	log.Infof("Found %d transactions for address %s",
		len(history), addr.EncodeAddress())

	for _, histItem := range history {
		txHash, err := chainhash.NewHashFromStr(histItem.Hash)
		if err != nil {
			continue
		}

		tx, err := c.client.GetTransactionMsgTx(ctx, txHash)
		if err != nil {
			log.Warnf("Failed to get transaction %s: %v",
				histItem.Hash, err)
			continue
		}

		var block *wtxmgr.BlockMeta
		if histItem.Height > 0 {
			// Confirmed transaction.
			blockHash, err := c.GetBlockHash(int64(histItem.Height))
			if err != nil {
				log.Warnf("Failed to get block hash for height %d: %v",
					histItem.Height, err)
				continue
			}

			block = &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{
					Hash:   *blockHash,
					Height: int32(histItem.Height),
				},
			}
		}

		// Send relevant transaction notification.
		log.Infof("Sending RelevantTx notification for tx %s (height=%d) to address %s",
			txHash, histItem.Height, addr.EncodeAddress())

		select {
		case c.notificationChan <- chain.RelevantTx{
			TxRecord: &wtxmgr.TxRecord{
				MsgTx:    *tx,
				Hash:     *txHash,
				Received: time.Now(),
			},
			Block: block,
		}:
		case <-c.quit:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// NotifyBlocks starts sending block update notifications to the notification
// channel.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) NotifyBlocks() error {
	c.notifyBlocks.Store(true)
	return nil
}

// Notifications returns a channel that will be sent notifications.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) Notifications() <-chan interface{} {
	return c.notificationChan
}

// BackEnd returns the name of the backend.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) BackEnd() string {
	return electrumBackendName
}

// TestMempoolAccept tests whether a transaction would be accepted to the
// mempool.
//
// NOTE: Electrum does not support this operation.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) TestMempoolAccept(txns []*wire.MsgTx,
	maxFeeRate float64) ([]*btcjson.TestMempoolAcceptResult, error) {

	// Electrum doesn't support testmempoolaccept. Return nil results
	// which should be interpreted as "unknown" by callers.
	return nil, nil
}

// MapRPCErr maps an error from the underlying RPC client to a chain error.
//
// NOTE: This is part of the chain.Interface interface.
func (c *ChainClient) MapRPCErr(err error) error {
	return err
}

// notificationHandler processes incoming notifications from the Electrum
// server.
func (c *ChainClient) notificationHandler(
	headerChan <-chan *SubscribeHeadersResult) {

	defer c.wg.Done()

	for {
		select {
		case header, ok := <-headerChan:
			if !ok {
				log.Warn("Header channel closed")
				return
			}

			c.handleNewHeader(header)

		case <-c.quit:
			return
		}
	}
}

// handleNewHeader processes a new block header notification.
func (c *ChainClient) handleNewHeader(header *SubscribeHeadersResult) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	blockHeader, err := c.client.GetBlockHeader(ctx, uint32(header.Height))
	if err != nil {
		log.Errorf("Failed to get block header at height %d: %v",
			header.Height, err)
		return
	}

	hash := blockHeader.BlockHash()

	// Check for reorg.
	c.bestBlockMtx.RLock()
	prevHeight := c.bestBlock.Height
	prevHash := c.bestBlock.Hash
	c.bestBlockMtx.RUnlock()

	if int32(header.Height) <= prevHeight && !hash.IsEqual(&prevHash) {
		// Potential reorg - notify disconnected blocks.
		for h := prevHeight; h >= int32(header.Height); h-- {
			c.heightToHashMtx.RLock()
			oldHash := c.heightToHash[h]
			c.heightToHashMtx.RUnlock()

			if oldHash != nil && c.notifyBlocks.Load() {
				c.notificationChan <- chain.BlockDisconnected{
					Block: wtxmgr.Block{
						Hash:   *oldHash,
						Height: h,
					},
				}
			}
		}
	}

	// Update best block.
	c.bestBlockMtx.Lock()
	c.bestBlock = waddrmgr.BlockStamp{
		Height:    int32(header.Height),
		Hash:      hash,
		Timestamp: blockHeader.Timestamp,
	}
	c.bestBlockMtx.Unlock()

	// Cache the header.
	c.cacheHeader(int32(header.Height), &hash, blockHeader)

	// Send block connected notification if requested.
	if c.notifyBlocks.Load() {
		c.notificationChan <- chain.BlockConnected{
			Block: wtxmgr.Block{
				Hash:   hash,
				Height: int32(header.Height),
			},
			Time: blockHeader.Timestamp,
		}
	}

	// Check watched addresses for new transactions.
	c.checkWatchedAddresses(ctx, int32(header.Height), &hash)
}

// checkWatchedAddresses checks if any watched addresses have new transactions
// in the given block.
func (c *ChainClient) checkWatchedAddresses(ctx context.Context,
	height int32, blockHash *chainhash.Hash) {

	c.watchedAddrsMtx.RLock()
	addrs := make([]btcutil.Address, 0, len(c.watchedAddrs))
	for _, addr := range c.watchedAddrs {
		addrs = append(addrs, addr)
	}
	c.watchedAddrsMtx.RUnlock()

	log.Debugf("Checking %d watched addresses for block %d", len(addrs), height)

	for _, addr := range addrs {
		pkScript, err := scriptFromAddress(addr, c.chainParams)
		if err != nil {
			log.Warnf("Failed to get pkScript for address %s: %v",
				addr.EncodeAddress(), err)
			continue
		}

		scripthash := ScripthashFromScript(pkScript)

		log.Tracef("Querying history for address %s (scripthash: %s)",
			addr.EncodeAddress(), scripthash)

		history, err := c.client.GetHistory(ctx, scripthash)
		if err != nil {
			log.Warnf("Failed to get history for address %s: %v",
				addr.EncodeAddress(), err)
			continue
		}

		log.Debugf("Address %s has %d history items",
			addr.EncodeAddress(), len(history))

		for _, histItem := range history {
			log.Tracef("History item for %s: txid=%s height=%d (looking for height %d)",
				addr.EncodeAddress(), histItem.Hash, histItem.Height, height)

			if int32(histItem.Height) != height {
				continue
			}

			log.Infof("Found relevant tx %s at height %d for address %s",
				histItem.Hash, height, addr.EncodeAddress())

			txHash, err := chainhash.NewHashFromStr(histItem.Hash)
			if err != nil {
				log.Warnf("Failed to parse tx hash %s: %v", histItem.Hash, err)
				continue
			}

			tx, err := c.client.GetTransactionMsgTx(ctx, txHash)
			if err != nil {
				log.Warnf("Failed to get transaction %s: %v", histItem.Hash, err)
				continue
			}

			log.Infof("Sending RelevantTx notification for tx %s in block %d",
				txHash, height)

			c.notificationChan <- chain.RelevantTx{
				TxRecord: &wtxmgr.TxRecord{
					MsgTx:    *tx,
					Hash:     *txHash,
					Received: time.Now(),
				},
				Block: &wtxmgr.BlockMeta{
					Block: wtxmgr.Block{
						Hash:   *blockHash,
						Height: height,
					},
				},
			}
		}
	}
}

// cacheHeader adds a header to the cache.
func (c *ChainClient) cacheHeader(height int32, hash *chainhash.Hash,
	header *wire.BlockHeader) {

	c.headerCacheMtx.Lock()
	c.headerCache[*hash] = header
	c.headerCacheMtx.Unlock()

	c.heightToHashMtx.Lock()
	hashCopy := *hash
	c.heightToHash[height] = &hashCopy
	c.heightToHashMtx.Unlock()
}

// scriptFromAddress creates a pkScript from an address.
func scriptFromAddress(addr btcutil.Address,
	params *chaincfg.Params) ([]byte, error) {

	return PayToAddrScript(addr)
}

// PayToAddrScript creates a new script to pay to the given address.
func PayToAddrScript(addr btcutil.Address) ([]byte, error) {
	switch addr := addr.(type) {
	case *btcutil.AddressPubKeyHash:
		return payToPubKeyHashScript(addr.ScriptAddress())

	case *btcutil.AddressScriptHash:
		return payToScriptHashScript(addr.ScriptAddress())

	case *btcutil.AddressWitnessPubKeyHash:
		return payToWitnessPubKeyHashScript(addr.ScriptAddress())

	case *btcutil.AddressWitnessScriptHash:
		return payToWitnessScriptHashScript(addr.ScriptAddress())

	case *btcutil.AddressTaproot:
		return payToTaprootScript(addr.ScriptAddress())

	default:
		return nil, fmt.Errorf("unsupported address type: %T", addr)
	}
}

// payToPubKeyHashScript creates a P2PKH script.
func payToPubKeyHashScript(pubKeyHash []byte) ([]byte, error) {
	return []byte{
		0x76, // OP_DUP
		0xa9, // OP_HASH160
		0x14, // Push 20 bytes
		pubKeyHash[0], pubKeyHash[1], pubKeyHash[2], pubKeyHash[3],
		pubKeyHash[4], pubKeyHash[5], pubKeyHash[6], pubKeyHash[7],
		pubKeyHash[8], pubKeyHash[9], pubKeyHash[10], pubKeyHash[11],
		pubKeyHash[12], pubKeyHash[13], pubKeyHash[14], pubKeyHash[15],
		pubKeyHash[16], pubKeyHash[17], pubKeyHash[18], pubKeyHash[19],
		0x88, // OP_EQUALVERIFY
		0xac, // OP_CHECKSIG
	}, nil
}

// payToScriptHashScript creates a P2SH script.
func payToScriptHashScript(scriptHash []byte) ([]byte, error) {
	return []byte{
		0xa9, // OP_HASH160
		0x14, // Push 20 bytes
		scriptHash[0], scriptHash[1], scriptHash[2], scriptHash[3],
		scriptHash[4], scriptHash[5], scriptHash[6], scriptHash[7],
		scriptHash[8], scriptHash[9], scriptHash[10], scriptHash[11],
		scriptHash[12], scriptHash[13], scriptHash[14], scriptHash[15],
		scriptHash[16], scriptHash[17], scriptHash[18], scriptHash[19],
		0x87, // OP_EQUAL
	}, nil
}

// payToWitnessPubKeyHashScript creates a P2WPKH script.
func payToWitnessPubKeyHashScript(pubKeyHash []byte) ([]byte, error) {
	return []byte{
		0x00, // OP_0 (witness version)
		0x14, // Push 20 bytes
		pubKeyHash[0], pubKeyHash[1], pubKeyHash[2], pubKeyHash[3],
		pubKeyHash[4], pubKeyHash[5], pubKeyHash[6], pubKeyHash[7],
		pubKeyHash[8], pubKeyHash[9], pubKeyHash[10], pubKeyHash[11],
		pubKeyHash[12], pubKeyHash[13], pubKeyHash[14], pubKeyHash[15],
		pubKeyHash[16], pubKeyHash[17], pubKeyHash[18], pubKeyHash[19],
	}, nil
}

// payToWitnessScriptHashScript creates a P2WSH script.
func payToWitnessScriptHashScript(scriptHash []byte) ([]byte, error) {
	script := make([]byte, 34)
	script[0] = 0x00 // OP_0 (witness version)
	script[1] = 0x20 // Push 32 bytes
	copy(script[2:], scriptHash)
	return script, nil
}

// payToTaprootScript creates a P2TR script.
func payToTaprootScript(pubKey []byte) ([]byte, error) {
	script := make([]byte, 34)
	script[0] = 0x51 // OP_1 (witness version 1)
	script[1] = 0x20 // Push 32 bytes
	copy(script[2:], pubKey)
	return script, nil
}
