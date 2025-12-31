package electrum

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/checksum0/go-electrum/electrum"
)

// GetBalance retrieves the confirmed and unconfirmed balance for a scripthash.
// The scripthash is the SHA256 hash of the output script in reverse byte
// order.
func (c *Client) GetBalance(ctx context.Context,
	scripthash string) (electrum.GetBalanceResult, error) {

	var result electrum.GetBalanceResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		balance, err := client.GetBalance(ctx, scripthash)
		if err != nil {
			return err
		}

		result = balance
		return nil
	})

	return result, err
}

// GetHistory retrieves the transaction history for a scripthash.
func (c *Client) GetHistory(ctx context.Context,
	scripthash string) ([]*electrum.GetMempoolResult, error) {

	var result []*electrum.GetMempoolResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		history, err := client.GetHistory(ctx, scripthash)
		if err != nil {
			return err
		}

		result = history
		return nil
	})

	return result, err
}

// ListUnspent retrieves the list of unspent transaction outputs for a
// scripthash.
func (c *Client) ListUnspent(ctx context.Context,
	scripthash string) ([]*electrum.ListUnspentResult, error) {

	var result []*electrum.ListUnspentResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		unspent, err := client.ListUnspent(ctx, scripthash)
		if err != nil {
			return err
		}

		result = unspent
		return nil
	})

	return result, err
}

// GetRawTransaction retrieves a raw transaction hex by its hash.
func (c *Client) GetRawTransaction(ctx context.Context,
	txHash string) (string, error) {

	var result string

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		txHex, err := client.GetRawTransaction(ctx, txHash)
		if err != nil {
			return err
		}

		result = txHex
		return nil
	})

	return result, err
}

// GetTransaction retrieves detailed transaction information by its hash.
func (c *Client) GetTransaction(ctx context.Context,
	txHash string) (*electrum.GetTransactionResult, error) {

	var result *electrum.GetTransactionResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		tx, err := client.GetTransaction(ctx, txHash)
		if err != nil {
			return err
		}

		result = tx
		return nil
	})

	return result, err
}

// GetTransactionMsgTx retrieves a transaction and deserializes it into a
// wire.MsgTx.
func (c *Client) GetTransactionMsgTx(ctx context.Context,
	txHash *chainhash.Hash) (*wire.MsgTx, error) {

	txHex, err := c.GetRawTransaction(ctx, txHash.String())
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

// BroadcastTransaction broadcasts a raw transaction to the network.
func (c *Client) BroadcastTransaction(ctx context.Context,
	txHex string) (string, error) {

	var result string

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		txid, err := client.BroadcastTransaction(ctx, txHex)
		if err != nil {
			return err
		}

		result = txid
		return nil
	})

	return result, err
}

// BroadcastTx broadcasts a wire.MsgTx to the network.
func (c *Client) BroadcastTx(ctx context.Context,
	tx *wire.MsgTx) (*chainhash.Hash, error) {

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

// GetBlockHeader retrieves a block header by height.
func (c *Client) GetBlockHeader(ctx context.Context,
	height uint32) (*wire.BlockHeader, error) {

	var result *wire.BlockHeader

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		header, err := client.GetBlockHeader(ctx, height)
		if err != nil {
			return err
		}

		// Parse the header hex into a wire.BlockHeader.
		headerBytes, err := hex.DecodeString(header.Header)
		if err != nil {
			return fmt.Errorf("failed to decode header: %w", err)
		}

		blockHeader := &wire.BlockHeader{}
		if err := blockHeader.Deserialize(
			bytes.NewReader(headerBytes)); err != nil {

			return fmt.Errorf("failed to parse header: %w", err)
		}

		result = blockHeader
		return nil
	})

	return result, err
}

// GetBlockHeaderRaw retrieves a block header by height and returns the raw
// result from the Electrum server.
func (c *Client) GetBlockHeaderRaw(ctx context.Context,
	height uint32) (*electrum.GetBlockHeaderResult, error) {

	var result *electrum.GetBlockHeaderResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		header, err := client.GetBlockHeader(ctx, height)
		if err != nil {
			return err
		}

		result = header
		return nil
	})

	return result, err
}

// GetBlockHeaders retrieves a range of block headers starting from the given
// height.
func (c *Client) GetBlockHeaders(ctx context.Context, startHeight uint32,
	count uint32) ([]*wire.BlockHeader, error) {

	var result []*wire.BlockHeader

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		headers, err := client.GetBlockHeaders(ctx, startHeight, count)
		if err != nil {
			return err
		}

		result = make([]*wire.BlockHeader, 0, headers.Count)

		// Bitcoin block header is always 80 bytes.
		const headerSize = 80

		hexData, err := hex.DecodeString(headers.Headers)
		if err != nil {
			return fmt.Errorf("failed to decode headers: %w", err)
		}

		for i := 0; i < int(headers.Count); i++ {
			start := i * headerSize
			end := start + headerSize

			if end > len(hexData) {
				return fmt.Errorf("header data truncated")
			}

			blockHeader := &wire.BlockHeader{}
			reader := bytes.NewReader(hexData[start:end])
			if err := blockHeader.Deserialize(reader); err != nil {
				return fmt.Errorf("failed to parse header "+
					"%d: %w", i, err)
			}

			result = append(result, blockHeader)
		}

		return nil
	})

	return result, err
}

// EstimateFee estimates the fee rate (in BTC/kB) needed for a transaction to
// be confirmed within the given number of blocks.
func (c *Client) EstimateFee(ctx context.Context,
	targetBlocks int) (float32, error) {

	var result float32

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		fee, err := client.GetFee(ctx, uint32(targetBlocks))
		if err != nil {
			return err
		}

		result = fee
		return nil
	})

	return result, err
}

// SubscribeHeaders subscribes to new block header notifications.
func (c *Client) SubscribeHeaders(
	ctx context.Context) (<-chan *electrum.SubscribeHeadersResult, error) {

	client, err := c.getClient()
	if err != nil {
		return nil, err
	}

	return client.SubscribeHeaders(ctx)
}

// SubscribeScripthash subscribes to notifications for a scripthash. Returns
// both the subscription object and the notification channel.
func (c *Client) SubscribeScripthash(
	ctx context.Context,
	scripthash string) (*electrum.ScripthashSubscription,
	<-chan *electrum.SubscribeNotif, error) {

	client, err := c.getClient()
	if err != nil {
		return nil, nil, err
	}

	sub, notifChan := client.SubscribeScripthash()
	if err := sub.Add(ctx, scripthash); err != nil {
		return nil, nil, err
	}

	return sub, notifChan, nil
}

// GetMerkle retrieves the merkle proof for a transaction in a block.
func (c *Client) GetMerkle(ctx context.Context, txHash string,
	height uint32) (*electrum.GetMerkleProofResult, error) {

	var result *electrum.GetMerkleProofResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		proof, err := client.GetMerkleProof(ctx, txHash, height)
		if err != nil {
			return err
		}

		result = proof
		return nil
	})

	return result, err
}

// GetRelayFee returns the minimum fee a transaction must pay to be accepted
// into the remote server's memory pool.
func (c *Client) GetRelayFee(ctx context.Context) (float32, error) {
	var result float32

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		fee, err := client.GetRelayFee(ctx)
		if err != nil {
			return err
		}

		result = fee
		return nil
	})

	return result, err
}

// ServerFeatures returns a list of features and services supported by the
// remote server.
func (c *Client) ServerFeatures(
	ctx context.Context) (*electrum.ServerFeaturesResult, error) {

	var result *electrum.ServerFeaturesResult

	err := c.withRetry(ctx, func(ctx context.Context,
		client *electrum.Client) error {

		features, err := client.ServerFeatures(ctx)
		if err != nil {
			return err
		}

		result = features
		return nil
	})

	return result, err
}
