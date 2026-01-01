package esplora

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/stretchr/testify/require"
)

// TestChainClientInterface verifies that ChainClient implements chain.Interface.
func TestChainClientInterface(t *testing.T) {
	t.Parallel()

	var _ chain.Interface = (*ChainClient)(nil)
}

// TestNewChainClient tests creating a new chain client.
func TestNewChainClient(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	require.NotNil(t, chainClient)
	require.NotNil(t, chainClient.client)
	require.NotNil(t, chainClient.headerCache)
	require.NotNil(t, chainClient.heightToHash)
	require.NotNil(t, chainClient.notificationChan)
	require.Equal(t, &chaincfg.MainNetParams, chainClient.chainParams)
}

// TestChainClientBackEnd tests the BackEnd method.
func TestChainClientBackEnd(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	require.Equal(t, "esplora", chainClient.BackEnd())
}

// TestChainClientNotifications tests the Notifications channel.
func TestChainClientNotifications(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	notifChan := chainClient.Notifications()
	require.NotNil(t, notifChan)
}

// TestChainClientTestMempoolAccept tests that TestMempoolAccept returns nil.
func TestChainClientTestMempoolAccept(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	tx := wire.NewMsgTx(wire.TxVersion)
	results, err := chainClient.TestMempoolAccept([]*wire.MsgTx{tx}, 0.0)

	// Esplora doesn't support this, so we expect ErrBackendVersion error
	// which triggers the caller to fall back to direct publish.
	require.ErrorIs(t, err, rpcclient.ErrBackendVersion)
	require.Nil(t, results)
}

// TestChainClientMapRPCErr tests the MapRPCErr method.
func TestChainClientMapRPCErr(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	testErr := ErrNotConnected
	mappedErr := chainClient.MapRPCErr(testErr)

	require.Equal(t, testErr, mappedErr)
}

// TestChainClientNotifyBlocks tests enabling block notifications.
func TestChainClientNotifyBlocks(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	err := chainClient.NotifyBlocks()
	require.NoError(t, err)
	require.True(t, chainClient.notifyBlocks.Load())
}

// TestChainClientNotifyReceived tests adding watched addresses.
func TestChainClientNotifyReceived(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Create a test address.
	pubKeyHash := make([]byte, 20)
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	err = chainClient.NotifyReceived([]btcutil.Address{addr})
	require.NoError(t, err)

	chainClient.watchedAddrsMtx.RLock()
	_, exists := chainClient.watchedAddrs[addr.EncodeAddress()]
	chainClient.watchedAddrsMtx.RUnlock()

	require.True(t, exists)
}

// TestChainClientIsCurrent tests the IsCurrent method.
func TestChainClientIsCurrent(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Without a live connection, IsCurrent() should return false since it
	// cannot fetch the best block from the network.
	require.False(t, chainClient.IsCurrent())
}

// TestChainClientCacheHeader tests the header caching functionality.
func TestChainClientCacheHeader(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Create a test header.
	header := &wire.BlockHeader{
		Version:   1,
		Timestamp: time.Now(),
		Bits:      0x1d00ffff,
	}
	hash := header.BlockHash()
	height := int32(100)

	// Cache the header.
	chainClient.cacheHeader(height, &hash, header)

	// Verify it's in the header cache.
	chainClient.headerCacheMtx.RLock()
	cachedHeader, exists := chainClient.headerCache[hash]
	chainClient.headerCacheMtx.RUnlock()

	require.True(t, exists)
	require.Equal(t, header, cachedHeader)

	// Verify height to hash mapping.
	chainClient.heightToHashMtx.RLock()
	cachedHash, exists := chainClient.heightToHash[height]
	chainClient.heightToHashMtx.RUnlock()

	require.True(t, exists)
	require.Equal(t, &hash, cachedHash)
}

// TestChainClientGetUtxo tests the GetUtxo method.
func TestChainClientGetUtxo(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 1 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Create a test outpoint and pkScript.
	testHash := chainhash.Hash{0x01, 0x02, 0x03}
	op := &wire.OutPoint{
		Hash:  testHash,
		Index: 0,
	}
	pkScript := []byte{0x00, 0x14, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06,
		0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14}

	// Without a connected client, GetUtxo should return an error.
	cancel := make(chan struct{})
	_, err := chainClient.GetUtxo(op, pkScript, 100, cancel)
	require.Error(t, err)
}

// TestEsploraUtxoSourceInterface verifies that ChainClient can be used as a
// UTXO source.
func TestEsploraUtxoSourceInterface(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Define the interface locally to test without importing btcwallet.
	type UtxoSource interface {
		GetUtxo(op *wire.OutPoint, pkScript []byte, heightHint uint32,
			cancel <-chan struct{}) (*wire.TxOut, error)
	}

	// Verify ChainClient implements UtxoSource.
	var _ UtxoSource = chainClient
}

// TestChainClientGetBlockHashCaching tests that GetBlockHash caches results.
func TestChainClientGetBlockHashCaching(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Pre-populate the cache.
	testHash := chainhash.Hash{0x01, 0x02, 0x03, 0x04}
	height := int32(500)

	chainClient.heightToHashMtx.Lock()
	chainClient.heightToHash[height] = &testHash
	chainClient.heightToHashMtx.Unlock()

	// GetBlockHash should return the cached value.
	hash, err := chainClient.GetBlockHash(int64(height))
	require.NoError(t, err)
	require.Equal(t, &testHash, hash)
}

// TestChainClientGetBlockHeaderCaching tests that GetBlockHeader caches results.
func TestChainClientGetBlockHeaderCaching(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Create and cache a test header.
	header := &wire.BlockHeader{
		Version:   1,
		Timestamp: time.Now(),
		Bits:      0x1d00ffff,
	}
	hash := header.BlockHash()

	chainClient.headerCacheMtx.Lock()
	chainClient.headerCache[hash] = header
	chainClient.headerCacheMtx.Unlock()

	// GetBlockHeader should return the cached value.
	cachedHeader, err := chainClient.GetBlockHeader(&hash)
	require.NoError(t, err)
	require.Equal(t, header, cachedHeader)
}

// TestChainClientMultipleAddresses tests watching multiple addresses.
func TestChainClientMultipleAddresses(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}
	client := NewClient(cfg)

	chainClient := NewChainClient(client, &chaincfg.MainNetParams)

	// Create multiple test addresses.
	addrs := make([]btcutil.Address, 5)
	for i := 0; i < 5; i++ {
		pubKeyHash := make([]byte, 20)
		pubKeyHash[0] = byte(i)
		addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
		require.NoError(t, err)
		addrs[i] = addr
	}

	err := chainClient.NotifyReceived(addrs)
	require.NoError(t, err)

	chainClient.watchedAddrsMtx.RLock()
	count := len(chainClient.watchedAddrs)
	chainClient.watchedAddrsMtx.RUnlock()

	require.Equal(t, 5, count)
}
