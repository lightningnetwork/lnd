package electrum

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/stretchr/testify/require"
)

// TestChainViewAdapterInterface verifies that ChainViewAdapter implements the
// chainview.ElectrumClient interface.
func TestChainViewAdapterInterface(t *testing.T) {
	t.Parallel()

	// This is a compile-time check that ChainViewAdapter implements the
	// chainview.ElectrumClient interface. If this fails to compile, the
	// interface is not properly implemented.
	var _ chainview.ElectrumClient = (*ChainViewAdapter)(nil)
}

// TestNewChainViewAdapter tests the creation of a new ChainViewAdapter.
func TestNewChainViewAdapter(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    30 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        3,
	}

	client := NewClient(cfg)
	adapter := NewChainViewAdapter(client)

	require.NotNil(t, adapter)
	require.NotNil(t, adapter.client)
	require.Equal(t, client, adapter.client)
}

// TestChainViewAdapterIsConnected tests the IsConnected method.
func TestChainViewAdapterIsConnected(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    30 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        3,
	}

	client := NewClient(cfg)
	adapter := NewChainViewAdapter(client)

	// Client should not be connected since we haven't started it.
	require.False(t, adapter.IsConnected())
}

// TestChainViewAdapterGetBlockHeaderNotConnected tests that GetBlockHeader
// returns an error when the client is not connected.
func TestChainViewAdapterGetBlockHeaderNotConnected(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    1 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
	}

	client := NewClient(cfg)
	adapter := NewChainViewAdapter(client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := adapter.GetBlockHeader(ctx, 100)
	require.Error(t, err)
}

// TestChainViewAdapterGetHistoryNotConnected tests that GetHistory returns
// an error when the client is not connected.
func TestChainViewAdapterGetHistoryNotConnected(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    1 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
	}

	client := NewClient(cfg)
	adapter := NewChainViewAdapter(client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := adapter.GetHistory(ctx, "testscripthash")
	require.Error(t, err)
}

// TestChainViewAdapterGetTransactionMsgTxNotConnected tests that
// GetTransactionMsgTx returns an error when the client is not connected.
func TestChainViewAdapterGetTransactionMsgTxNotConnected(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    1 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
	}

	client := NewClient(cfg)
	adapter := NewChainViewAdapter(client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	txHash := &chainhash.Hash{}
	_, err := adapter.GetTransactionMsgTx(ctx, txHash)
	require.Error(t, err)
}

// TestChainViewAdapterSubscribeHeadersNotConnected tests that SubscribeHeaders
// returns an error when the client is not connected.
func TestChainViewAdapterSubscribeHeadersNotConnected(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		Server:            "localhost:50001",
		UseSSL:            false,
		ReconnectInterval: 10 * time.Second,
		RequestTimeout:    1 * time.Second,
		PingInterval:      60 * time.Second,
		MaxRetries:        0,
	}

	client := NewClient(cfg)
	adapter := NewChainViewAdapter(client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := adapter.SubscribeHeaders(ctx)
	require.Error(t, err)
}
