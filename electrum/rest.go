package electrum

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// RESTClient provides methods to fetch data from the mempool/electrs REST API.
type RESTClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewRESTClient creates a new REST client for the mempool/electrs API.
func NewRESTClient(baseURL string) *RESTClient {
	return &RESTClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// BlockInfo represents the response from the /api/block/:hash endpoint.
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

// TxInfo represents the response from the /api/tx/:txid endpoint.
type TxInfo struct {
	TxID     string `json:"txid"`
	Version  int32  `json:"version"`
	LockTime uint32 `json:"locktime"`
	Size     int    `json:"size"`
	Weight   int    `json:"weight"`
	Fee      int64  `json:"fee"`
	Status   struct {
		Confirmed   bool   `json:"confirmed"`
		BlockHeight int64  `json:"block_height"`
		BlockHash   string `json:"block_hash"`
		BlockTime   int64  `json:"block_time"`
	} `json:"status"`
}

// GetBlockInfo fetches block information from the REST API.
func (r *RESTClient) GetBlockInfo(ctx context.Context, blockHash string) (*BlockInfo, error) {
	url := fmt.Sprintf("%s/block/%s", r.baseURL, blockHash)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var blockInfo BlockInfo
	if err := json.NewDecoder(resp.Body).Decode(&blockInfo); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &blockInfo, nil
}

// GetBlockTxIDs fetches the transaction IDs for a block from the REST API.
func (r *RESTClient) GetBlockTxIDs(ctx context.Context, blockHash string) ([]string, error) {
	url := fmt.Sprintf("%s/block/%s/txids", r.baseURL, blockHash)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var txids []string
	if err := json.NewDecoder(resp.Body).Decode(&txids); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return txids, nil
}

// getRawTransaction fetches the raw transaction hex from the REST API.
// This is an internal method used by GetBlock. For fetching transactions,
// use the Electrum protocol methods in methods.go instead.
func (r *RESTClient) getRawTransaction(ctx context.Context, txid string) (string, error) {
	url := fmt.Sprintf("%s/tx/%s/hex", r.baseURL, txid)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}

// getTransaction fetches a parsed transaction from the REST API.
// This is an internal method used by GetBlock. For fetching transactions,
// use the Electrum protocol methods in methods.go instead.
func (r *RESTClient) getTransaction(ctx context.Context, txid string) (*wire.MsgTx, error) {
	txHex, err := r.getRawTransaction(ctx, txid)
	if err != nil {
		return nil, err
	}

	txBytes, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode tx hex: %w", err)
	}

	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(hex.NewDecoder(
		&hexStringReader{s: txHex},
	)); err != nil {
		// Try direct bytes deserialization
		reader := &byteReader{data: txBytes, pos: 0}
		if err := msgTx.Deserialize(reader); err != nil {
			return nil, fmt.Errorf("failed to deserialize tx: %w", err)
		}
	}

	return &msgTx, nil
}

// GetBlock fetches a full block with all transactions from the REST API.
// This is done by first fetching the block's txids, then fetching each tx.
func (r *RESTClient) GetBlock(ctx context.Context, blockHash *chainhash.Hash) (*btcutil.Block, error) {
	hashStr := blockHash.String()

	// Get block info first
	blockInfo, err := r.GetBlockInfo(ctx, hashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to get block info: %w", err)
	}

	// Get all transaction IDs in the block
	txids, err := r.GetBlockTxIDs(ctx, hashStr)
	if err != nil {
		return nil, fmt.Errorf("failed to get block txids: %w", err)
	}

	// Fetch each transaction
	transactions := make([]*wire.MsgTx, 0, len(txids))
	for _, txid := range txids {
		tx, err := r.getTransaction(ctx, txid)
		if err != nil {
			return nil, fmt.Errorf("failed to get tx %s: %w", txid, err)
		}
		transactions = append(transactions, tx)
	}

	// Build the block header
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

	// Build the wire.MsgBlock
	msgBlock := wire.MsgBlock{
		Header:       header,
		Transactions: transactions,
	}

	return btcutil.NewBlock(&msgBlock), nil
}

// GetTxIndex finds the index of a transaction within a block.
// Returns the position (0-based) of the transaction in the block's tx list.
func (r *RESTClient) GetTxIndex(ctx context.Context, blockHash string, txid string) (uint32, error) {
	txids, err := r.GetBlockTxIDs(ctx, blockHash)
	if err != nil {
		return 0, fmt.Errorf("failed to get block txids: %w", err)
	}

	for i, id := range txids {
		if id == txid {
			return uint32(i), nil
		}
	}

	return 0, fmt.Errorf("transaction %s not found in block %s", txid, blockHash)
}

// GetTxIndexByHeight finds the index of a transaction within a block at the given height.
func (r *RESTClient) GetTxIndexByHeight(ctx context.Context, height int64, txid string) (uint32, string, error) {
	// First get the block hash at this height
	url := fmt.Sprintf("%s/block-height/%d", r.baseURL, height)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("failed to read response: %w", err)
	}

	blockHash := string(body)
	txIndex, err := r.GetTxIndex(ctx, blockHash, txid)
	if err != nil {
		return 0, "", err
	}

	return txIndex, blockHash, nil
}

// GetBlockByHeight fetches a block by its height.
func (r *RESTClient) GetBlockByHeight(ctx context.Context, height int64) (*btcutil.Block, error) {
	// First get the block hash at this height
	url := fmt.Sprintf("%s/block-height/%d", r.baseURL, height)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	blockHash, err := chainhash.NewHashFromStr(string(body))
	if err != nil {
		return nil, fmt.Errorf("invalid block hash: %w", err)
	}

	return r.GetBlock(ctx, blockHash)
}

// hexStringReader is a helper for reading hex strings.
type hexStringReader struct {
	s   string
	pos int
}

func (r *hexStringReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.s) {
		return 0, io.EOF
	}
	n = copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}

// byteReader is a helper for reading bytes.
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
