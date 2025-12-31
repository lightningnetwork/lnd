package electrum

import (
	"context"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/routing/chainview"
)

// ChainViewAdapter wraps the Electrum Client to implement the
// chainview.ElectrumClient interface. This adapter bridges the gap between
// the electrum package and the chainview package, avoiding import cycles.
type ChainViewAdapter struct {
	client *Client
}

// NewChainViewAdapter creates a new ChainViewAdapter wrapping the given
// Electrum client.
func NewChainViewAdapter(client *Client) *ChainViewAdapter {
	return &ChainViewAdapter{
		client: client,
	}
}

// Compile time check to ensure ChainViewAdapter implements the
// chainview.ElectrumClient interface.
var _ chainview.ElectrumClient = (*ChainViewAdapter)(nil)

// IsConnected returns true if the client is currently connected to the
// Electrum server.
//
// NOTE: This is part of the chainview.ElectrumClient interface.
func (a *ChainViewAdapter) IsConnected() bool {
	return a.client.IsConnected()
}

// SubscribeHeaders subscribes to new block header notifications and returns
// a channel that will receive header updates.
//
// NOTE: This is part of the chainview.ElectrumClient interface.
func (a *ChainViewAdapter) SubscribeHeaders(
	ctx context.Context) (<-chan *chainview.HeaderResult, error) {

	electrumChan, err := a.client.SubscribeHeaders(ctx)
	if err != nil {
		return nil, err
	}

	// Create an adapter channel that converts electrum results to
	// chainview results.
	resultChan := make(chan *chainview.HeaderResult)

	go func() {
		defer close(resultChan)

		for {
			select {
			case header, ok := <-electrumChan:
				if !ok {
					return
				}

				result := &chainview.HeaderResult{
					Height: int32(header.Height),
				}

				select {
				case resultChan <- result:
				case <-ctx.Done():
					return
				}

			case <-ctx.Done():
				return
			}
		}
	}()

	return resultChan, nil
}

// GetBlockHeader retrieves the block header at the given height.
//
// NOTE: This is part of the chainview.ElectrumClient interface.
func (a *ChainViewAdapter) GetBlockHeader(ctx context.Context,
	height uint32) (*wire.BlockHeader, error) {

	return a.client.GetBlockHeader(ctx, height)
}

// GetHistory retrieves the transaction history for a scripthash.
//
// NOTE: This is part of the chainview.ElectrumClient interface.
func (a *ChainViewAdapter) GetHistory(ctx context.Context,
	scripthash string) ([]*chainview.HistoryResult, error) {

	electrumHistory, err := a.client.GetHistory(ctx, scripthash)
	if err != nil {
		return nil, err
	}

	results := make([]*chainview.HistoryResult, len(electrumHistory))
	for i, item := range electrumHistory {
		results[i] = &chainview.HistoryResult{
			TxHash: item.Hash,
			Height: item.Height,
		}
	}

	return results, nil
}

// GetTransactionMsgTx retrieves a transaction and returns it as a wire.MsgTx.
//
// NOTE: This is part of the chainview.ElectrumClient interface.
func (a *ChainViewAdapter) GetTransactionMsgTx(ctx context.Context,
	txHash *chainhash.Hash) (*wire.MsgTx, error) {

	return a.client.GetTransactionMsgTx(ctx, txHash)
}
