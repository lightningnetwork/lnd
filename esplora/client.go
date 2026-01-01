package esplora

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

var (
	// ErrClientShutdown is returned when the client has been shut down.
	ErrClientShutdown = errors.New("esplora client has been shut down")

	// ErrNotConnected is returned when the API is not reachable.
	ErrNotConnected = errors.New("esplora API not reachable")

	// ErrBlockNotFound is returned when a block cannot be found.
	ErrBlockNotFound = errors.New("block not found")

	// ErrTxNotFound is returned when a transaction cannot be found.
	ErrTxNotFound = errors.New("transaction not found")
)

// ClientConfig holds the configuration for the Esplora client.
type ClientConfig struct {
	// URL is the base URL of the Esplora API (e.g., http://localhost:3002).
	URL string

	// RequestTimeout is the timeout for individual HTTP requests.
	RequestTimeout time.Duration

	// MaxRetries is the maximum number of retries for failed requests.
	MaxRetries int

	// PollInterval is the interval for polling new blocks.
	PollInterval time.Duration
}

// BlockStatus represents the status of a block.
type BlockStatus struct {
	InBestChain bool   `json:"in_best_chain"`
	Height      int64  `json:"height"`
	NextBest    string `json:"next_best,omitempty"`
}

// BlockInfo represents block information from the API.
type BlockInfo struct {
	ID                string  `json:"id"`
	Height            int64   `json:"height"`
	Version           int32   `json:"version"`
	Timestamp         int64   `json:"timestamp"`
	TxCount           int     `json:"tx_count"`
	Size              int     `json:"size"`
	Weight            int     `json:"weight"`
	MerkleRoot        string  `json:"merkle_root"`
	PreviousBlockHash string  `json:"previousblockhash"`
	MedianTime        int64   `json:"mediantime"`
	Nonce             uint32  `json:"nonce"`
	Bits              uint32  `json:"bits"`
	Difficulty        float64 `json:"difficulty"`
}

// TxStatus represents transaction confirmation status.
type TxStatus struct {
	Confirmed   bool   `json:"confirmed"`
	BlockHeight int64  `json:"block_height,omitempty"`
	BlockHash   string `json:"block_hash,omitempty"`
	BlockTime   int64  `json:"block_time,omitempty"`
}

// TxInfo represents transaction information from the API.
type TxInfo struct {
	TxID     string   `json:"txid"`
	Version  int32    `json:"version"`
	LockTime uint32   `json:"locktime"`
	Size     int      `json:"size"`
	Weight   int      `json:"weight"`
	Fee      int64    `json:"fee"`
	Vin      []TxVin  `json:"vin"`
	Vout     []TxVout `json:"vout"`
	Status   TxStatus `json:"status"`
}

// TxVin represents a transaction input.
type TxVin struct {
	TxID         string   `json:"txid"`
	Vout         uint32   `json:"vout"`
	PrevOut      *TxVout  `json:"prevout,omitempty"`
	ScriptSig    string   `json:"scriptsig"`
	ScriptSigAsm string   `json:"scriptsig_asm"`
	Witness      []string `json:"witness,omitempty"`
	Sequence     uint32   `json:"sequence"`
	IsCoinbase   bool     `json:"is_coinbase"`
}

// TxVout represents a transaction output.
type TxVout struct {
	ScriptPubKey     string `json:"scriptpubkey"`
	ScriptPubKeyAsm  string `json:"scriptpubkey_asm"`
	ScriptPubKeyType string `json:"scriptpubkey_type"`
	ScriptPubKeyAddr string `json:"scriptpubkey_address,omitempty"`
	Value            int64  `json:"value"`
}

// UTXO represents an unspent transaction output.
type UTXO struct {
	TxID   string   `json:"txid"`
	Vout   uint32   `json:"vout"`
	Status TxStatus `json:"status"`
	Value  int64    `json:"value"`
}

// OutSpend represents the spend status of an output.
type OutSpend struct {
	Spent  bool     `json:"spent"`
	TxID   string   `json:"txid,omitempty"`
	Vin    uint32   `json:"vin,omitempty"`
	Status TxStatus `json:"status,omitempty"`
}

// MerkleProof represents a merkle proof for a transaction.
type MerkleProof struct {
	BlockHeight int64    `json:"block_height"`
	Merkle      []string `json:"merkle"`
	Pos         int      `json:"pos"`
}

// FeeEstimates represents fee estimates from the API.
// Keys are confirmation targets (as strings), values are fee rates in sat/vB.
type FeeEstimates map[string]float64

// Client is an HTTP client for the Esplora REST API.
type Client struct {
	cfg *ClientConfig

	httpClient *http.Client

	// started indicates whether the client has been started.
	started atomic.Bool

	// bestBlockMtx protects bestBlock fields.
	bestBlockMtx    sync.RWMutex
	bestBlockHash   string
	bestBlockHeight int64

	// subscribersMtx protects the subscribers map.
	subscribersMtx sync.RWMutex
	// subscribers maps subscriber IDs to their notification channels.
	// Each subscriber gets its own copy of block notifications.
	subscribers map[uint64]chan *BlockInfo
	nextSubID   uint64

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewClient creates a new Esplora client with the given configuration.
func NewClient(cfg *ClientConfig) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		subscribers: make(map[uint64]chan *BlockInfo),
		quit:        make(chan struct{}),
	}
}

// Start initializes the client and begins polling for new blocks.
func (c *Client) Start() error {
	if c.started.Swap(true) {
		return nil
	}

	log.Infof("Starting Esplora client, url=%s", c.cfg.URL)

	// Verify connection by fetching tip.
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.RequestTimeout)
	defer cancel()

	height, err := c.GetTipHeight(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Esplora API: %w", err)
	}

	hash, err := c.GetTipHash(ctx)
	if err != nil {
		return fmt.Errorf("failed to get tip hash: %w", err)
	}

	c.bestBlockMtx.Lock()
	c.bestBlockHeight = height
	c.bestBlockHash = hash
	c.bestBlockMtx.Unlock()

	log.Infof("Connected to Esplora API: tip height=%d, hash=%s", height, hash)

	// Start block polling goroutine.
	c.wg.Add(1)
	go c.blockPoller()

	return nil
}

// Stop shuts down the client.
func (c *Client) Stop() error {
	if !c.started.Load() {
		return nil
	}

	log.Info("Stopping Esplora client")

	close(c.quit)
	c.wg.Wait()

	// Close all subscriber channels.
	c.subscribersMtx.Lock()
	for id, ch := range c.subscribers {
		close(ch)
		delete(c.subscribers, id)
	}
	c.subscribersMtx.Unlock()

	return nil
}

// IsConnected returns true if the client appears to be working.
func (c *Client) IsConnected() bool {
	if !c.started.Load() {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.GetTipHeight(ctx)
	return err == nil
}

// Subscribe registers a new subscriber for block notifications and returns
// the subscription channel and ID. Each subscriber gets its own copy of
// block notifications to prevent race conditions between consumers.
func (c *Client) Subscribe() (<-chan *BlockInfo, uint64) {
	c.subscribersMtx.Lock()
	defer c.subscribersMtx.Unlock()

	id := c.nextSubID
	c.nextSubID++

	// Create a buffered channel for this subscriber.
	ch := make(chan *BlockInfo, 10)
	c.subscribers[id] = ch

	log.Debugf("New block notification subscriber: id=%d, total=%d",
		id, len(c.subscribers))

	return ch, id
}

// Unsubscribe removes a subscriber from block notifications.
func (c *Client) Unsubscribe(id uint64) {
	c.subscribersMtx.Lock()
	defer c.subscribersMtx.Unlock()

	if ch, ok := c.subscribers[id]; ok {
		close(ch)
		delete(c.subscribers, id)
		log.Debugf("Removed block notification subscriber: id=%d, remaining=%d",
			id, len(c.subscribers))
	}
}

// notifySubscribers sends a block notification to all subscribers.
func (c *Client) notifySubscribers(blockInfo *BlockInfo) {
	c.subscribersMtx.RLock()
	defer c.subscribersMtx.RUnlock()

	for id, ch := range c.subscribers {
		select {
		case ch <- blockInfo:
			// Successfully sent
		default:
			// Channel full, log warning but don't block
			log.Warnf("Block notification channel full for subscriber %d, "+
				"skipping height %d", id, blockInfo.Height)
		}
	}
}

// blockPoller polls for new blocks at regular intervals.
func (c *Client) blockPoller() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.quit:
			return
		case <-ticker.C:
			c.checkForNewBlocks()
		}
	}
}

// checkForNewBlocks checks if there are new blocks and sends notifications.
func (c *Client) checkForNewBlocks() {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.RequestTimeout)
	defer cancel()

	newHeight, err := c.GetTipHeight(ctx)
	if err != nil {
		log.Debugf("Failed to get tip height: %v", err)
		return
	}

	c.bestBlockMtx.RLock()
	currentHeight := c.bestBlockHeight
	currentHash := c.bestBlockHash
	c.bestBlockMtx.RUnlock()

	if newHeight <= currentHeight {
		// Check for reorg by comparing hashes.
		newHash, err := c.GetTipHash(ctx)
		if err != nil {
			return
		}
		if newHash != currentHash && newHeight == currentHeight {
			// Possible reorg at same height.
			log.Warnf("Possible reorg detected at height %d: old=%s new=%s",
				currentHeight, currentHash, newHash)
		}
		return
	}

	// New blocks detected, fetch and notify for each.
	for height := currentHeight + 1; height <= newHeight; height++ {
		blockHash, err := c.GetBlockHashByHeight(ctx, height)
		if err != nil {
			log.Warnf("Failed to get block hash at height %d: %v", height, err)
			continue
		}

		blockInfo, err := c.GetBlockInfo(ctx, blockHash)
		if err != nil {
			log.Warnf("Failed to get block info for %s: %v", blockHash, err)
			continue
		}

		// Update best block.
		c.bestBlockMtx.Lock()
		c.bestBlockHeight = height
		c.bestBlockHash = blockHash
		c.bestBlockMtx.Unlock()

		// Send notification to all subscribers.
		log.Debugf("New block notification: height=%d hash=%s", height, blockHash)
		c.notifySubscribers(blockInfo)
	}
}

// doRequest performs an HTTP request with retries.
func (c *Client) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	url := c.cfg.URL + path

	var lastErr error
	for i := 0; i <= c.cfg.MaxRetries; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.quit:
			return nil, ErrClientShutdown
		default:
		}

		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		if body != nil {
			req.Header.Set("Content-Type", "text/plain")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if i < c.cfg.MaxRetries {
				time.Sleep(time.Duration(i+1) * 100 * time.Millisecond)
			}
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("request failed after %d attempts: %w", c.cfg.MaxRetries+1, lastErr)
}

// doGet performs a GET request and returns the response body.
func (c *Client) doGet(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetTipHeight returns the current blockchain tip height.
func (c *Client) GetTipHeight(ctx context.Context) (int64, error) {
	body, err := c.doGet(ctx, "/blocks/tip/height")
	if err != nil {
		return 0, err
	}

	height, err := strconv.ParseInt(string(body), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse height: %w", err)
	}

	return height, nil
}

// GetTipHash returns the current blockchain tip hash.
func (c *Client) GetTipHash(ctx context.Context) (string, error) {
	body, err := c.doGet(ctx, "/blocks/tip/hash")
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// GetBlockInfo fetches block information by hash.
func (c *Client) GetBlockInfo(ctx context.Context, blockHash string) (*BlockInfo, error) {
	body, err := c.doGet(ctx, "/block/"+blockHash)
	if err != nil {
		return nil, err
	}

	var info BlockInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &info, nil
}

// GetBlockStatus fetches block status by hash.
func (c *Client) GetBlockStatus(ctx context.Context, blockHash string) (*BlockStatus, error) {
	body, err := c.doGet(ctx, "/block/"+blockHash+"/status")
	if err != nil {
		return nil, err
	}

	var status BlockStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &status, nil
}

// GetBlockHeader fetches the raw block header by hash.
func (c *Client) GetBlockHeader(ctx context.Context, blockHash string) (*wire.BlockHeader, error) {
	body, err := c.doGet(ctx, "/block/"+blockHash+"/header")
	if err != nil {
		return nil, err
	}

	headerBytes, err := hex.DecodeString(string(body))
	if err != nil {
		return nil, fmt.Errorf("failed to decode header hex: %w", err)
	}

	header := &wire.BlockHeader{}
	if err := header.Deserialize(bytes.NewReader(headerBytes)); err != nil {
		return nil, fmt.Errorf("failed to deserialize header: %w", err)
	}

	return header, nil
}

// GetBlockHeaderByHeight fetches block header by height.
func (c *Client) GetBlockHeaderByHeight(ctx context.Context, height int64) (*wire.BlockHeader, error) {
	hash, err := c.GetBlockHashByHeight(ctx, height)
	if err != nil {
		return nil, err
	}

	return c.GetBlockHeader(ctx, hash)
}

// GetBlockHashByHeight fetches the block hash at a given height.
func (c *Client) GetBlockHashByHeight(ctx context.Context, height int64) (string, error) {
	body, err := c.doGet(ctx, fmt.Sprintf("/block-height/%d", height))
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// GetBlockTxIDs fetches all transaction IDs in a block.
func (c *Client) GetBlockTxIDs(ctx context.Context, blockHash string) ([]string, error) {
	body, err := c.doGet(ctx, "/block/"+blockHash+"/txids")
	if err != nil {
		return nil, err
	}

	var txids []string
	if err := json.Unmarshal(body, &txids); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return txids, nil
}

// GetBlock fetches a full block with all transactions.
func (c *Client) GetBlock(ctx context.Context, blockHash *chainhash.Hash) (*btcutil.Block, error) {
	hashStr := blockHash.String()

	// Get block info for header data.
	blockInfo, err := c.GetBlockInfo(ctx, hashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to get block info: %w", err)
	}

	// Get all transaction IDs.
	txids, err := c.GetBlockTxIDs(ctx, hashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to get block txids: %w", err)
	}

	// Fetch each transaction.
	transactions := make([]*wire.MsgTx, 0, len(txids))
	for _, txid := range txids {
		tx, err := c.GetRawTransactionMsgTx(ctx, txid)
		if err != nil {
			return nil, fmt.Errorf("failed to get tx %s: %w", txid, err)
		}
		transactions = append(transactions, tx)
	}

	// Build the block header.
	prevHash, err := chainhash.NewHashFromStr(blockInfo.PreviousBlockHash)
	if err != nil {
		return nil, fmt.Errorf("invalid prev block hash: %w", err)
	}

	merkleRoot, err := chainhash.NewHashFromStr(blockInfo.MerkleRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid merkle root: %w", err)
	}

	header := wire.BlockHeader{
		Version:    blockInfo.Version,
		PrevBlock:  *prevHash,
		MerkleRoot: *merkleRoot,
		Timestamp:  time.Unix(blockInfo.Timestamp, 0),
		Bits:       blockInfo.Bits,
		Nonce:      blockInfo.Nonce,
	}

	msgBlock := wire.MsgBlock{
		Header:       header,
		Transactions: transactions,
	}

	return btcutil.NewBlock(&msgBlock), nil
}

// GetTransaction fetches transaction information by txid.
func (c *Client) GetTransaction(ctx context.Context, txid string) (*TxInfo, error) {
	body, err := c.doGet(ctx, "/tx/"+txid)
	if err != nil {
		return nil, err
	}

	var info TxInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &info, nil
}

// GetRawTransaction fetches the raw transaction hex by txid.
func (c *Client) GetRawTransaction(ctx context.Context, txid string) (string, error) {
	body, err := c.doGet(ctx, "/tx/"+txid+"/hex")
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// GetRawTransactionMsgTx fetches and deserializes a transaction.
func (c *Client) GetRawTransactionMsgTx(ctx context.Context, txid string) (*wire.MsgTx, error) {
	txHex, err := c.GetRawTransaction(ctx, txid)
	if err != nil {
		return nil, err
	}

	txBytes, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode tx hex: %w", err)
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		return nil, fmt.Errorf("failed to deserialize tx: %w", err)
	}

	return tx, nil
}

// GetTransactionMsgTx fetches a transaction by hash and returns it as wire.MsgTx.
func (c *Client) GetTransactionMsgTx(ctx context.Context, txHash *chainhash.Hash) (*wire.MsgTx, error) {
	return c.GetRawTransactionMsgTx(ctx, txHash.String())
}

// GetTxStatus fetches the confirmation status of a transaction.
func (c *Client) GetTxStatus(ctx context.Context, txid string) (*TxStatus, error) {
	body, err := c.doGet(ctx, "/tx/"+txid+"/status")
	if err != nil {
		return nil, err
	}

	var status TxStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &status, nil
}

// GetTxMerkleProof fetches the merkle proof for a transaction.
func (c *Client) GetTxMerkleProof(ctx context.Context, txid string) (*MerkleProof, error) {
	body, err := c.doGet(ctx, "/tx/"+txid+"/merkle-proof")
	if err != nil {
		return nil, err
	}

	var proof MerkleProof
	if err := json.Unmarshal(body, &proof); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &proof, nil
}

// GetTxOutSpend checks if a specific output is spent.
func (c *Client) GetTxOutSpend(ctx context.Context, txid string, vout uint32) (*OutSpend, error) {
	body, err := c.doGet(ctx, fmt.Sprintf("/tx/%s/outspend/%d", txid, vout))
	if err != nil {
		return nil, err
	}

	var outSpend OutSpend
	if err := json.Unmarshal(body, &outSpend); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &outSpend, nil
}

// GetTxOutSpends checks the spend status of all outputs in a transaction.
func (c *Client) GetTxOutSpends(ctx context.Context, txid string) ([]OutSpend, error) {
	body, err := c.doGet(ctx, "/tx/"+txid+"/outspends")
	if err != nil {
		return nil, err
	}

	var outSpends []OutSpend
	if err := json.Unmarshal(body, &outSpends); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return outSpends, nil
}

// GetAddressTxs fetches transactions for an address.
func (c *Client) GetAddressTxs(ctx context.Context, address string) ([]*TxInfo, error) {
	body, err := c.doGet(ctx, "/address/"+address+"/txs")
	if err != nil {
		return nil, err
	}

	var txs []*TxInfo
	if err := json.Unmarshal(body, &txs); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return txs, nil
}

// GetAddressUTXOs fetches unspent outputs for an address.
func (c *Client) GetAddressUTXOs(ctx context.Context, address string) ([]*UTXO, error) {
	body, err := c.doGet(ctx, "/address/"+address+"/utxo")
	if err != nil {
		return nil, err
	}

	var utxos []*UTXO
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return utxos, nil
}

// GetScripthashTxs fetches transactions for a scripthash.
func (c *Client) GetScripthashTxs(ctx context.Context, scripthash string) ([]*TxInfo, error) {
	body, err := c.doGet(ctx, "/scripthash/"+scripthash+"/txs")
	if err != nil {
		return nil, err
	}

	var txs []*TxInfo
	if err := json.Unmarshal(body, &txs); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return txs, nil
}

// GetScripthashUTXOs fetches unspent outputs for a scripthash.
func (c *Client) GetScripthashUTXOs(ctx context.Context, scripthash string) ([]*UTXO, error) {
	body, err := c.doGet(ctx, "/scripthash/"+scripthash+"/utxo")
	if err != nil {
		return nil, err
	}

	var utxos []*UTXO
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return utxos, nil
}

// GetFeeEstimates fetches fee estimates for various confirmation targets.
func (c *Client) GetFeeEstimates(ctx context.Context) (FeeEstimates, error) {
	body, err := c.doGet(ctx, "/fee-estimates")
	if err != nil {
		return nil, err
	}

	var estimates FeeEstimates
	if err := json.Unmarshal(body, &estimates); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return estimates, nil
}

// BroadcastTransaction broadcasts a raw transaction to the network.
// Returns the txid on success.
func (c *Client) BroadcastTransaction(ctx context.Context, txHex string) (string, error) {
	resp, err := c.doRequest(ctx, http.MethodPost, "/tx", bytes.NewBufferString(txHex))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("broadcast failed with status %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}

// BroadcastTx broadcasts a wire.MsgTx to the network.
func (c *Client) BroadcastTx(ctx context.Context, tx *wire.MsgTx) (*chainhash.Hash, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("failed to serialize tx: %w", err)
	}

	txHex := hex.EncodeToString(buf.Bytes())
	txid, err := c.BroadcastTransaction(ctx, txHex)
	if err != nil {
		return nil, err
	}

	return chainhash.NewHashFromStr(txid)
}

// GetTxIndex finds the index of a transaction within a block.
func (c *Client) GetTxIndex(ctx context.Context, blockHash string, txid string) (uint32, error) {
	txids, err := c.GetBlockTxIDs(ctx, blockHash)
	if err != nil {
		return 0, err
	}

	for i, id := range txids {
		if id == txid {
			return uint32(i), nil
		}
	}

	return 0, fmt.Errorf("transaction %s not found in block %s", txid, blockHash)
}

// GetTxIndexByHeight finds the transaction index in a block at the given height.
func (c *Client) GetTxIndexByHeight(ctx context.Context, height int64, txid string) (uint32, string, error) {
	blockHash, err := c.GetBlockHashByHeight(ctx, height)
	if err != nil {
		return 0, "", err
	}

	txIndex, err := c.GetTxIndex(ctx, blockHash, txid)
	if err != nil {
		return 0, "", err
	}

	return txIndex, blockHash, nil
}

// GetBestBlock returns the current best block hash and height.
func (c *Client) GetBestBlock() (string, int64) {
	c.bestBlockMtx.RLock()
	defer c.bestBlockMtx.RUnlock()
	return c.bestBlockHash, c.bestBlockHeight
}
