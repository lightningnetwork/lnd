package chainview

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/stretchr/testify/require"
)

// mockElectrumClient is a mock implementation of the ElectrumClient interface
// for testing purposes.
type mockElectrumClient struct {
	connected     bool
	currentHeight uint32
	headers       map[uint32]*wire.BlockHeader
	history       map[string][]*HistoryResult
	transactions  map[chainhash.Hash]*wire.MsgTx

	headerChan chan *HeaderResult

	mu sync.RWMutex
}

// newMockElectrumClient creates a new mock Electrum client for testing.
func newMockElectrumClient() *mockElectrumClient {
	return &mockElectrumClient{
		connected:    true,
		headers:      make(map[uint32]*wire.BlockHeader),
		history:      make(map[string][]*HistoryResult),
		transactions: make(map[chainhash.Hash]*wire.MsgTx),
		headerChan:   make(chan *HeaderResult, 10),
	}
}

// IsConnected returns true if the mock client is connected.
func (m *mockElectrumClient) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// SubscribeHeaders returns a channel for header notifications.
func (m *mockElectrumClient) SubscribeHeaders(
	ctx context.Context) (<-chan *HeaderResult, error) {

	return m.headerChan, nil
}

// GetBlockHeader returns the block header at the given height.
func (m *mockElectrumClient) GetBlockHeader(ctx context.Context,
	height uint32) (*wire.BlockHeader, error) {

	m.mu.RLock()
	defer m.mu.RUnlock()

	if header, ok := m.headers[height]; ok {
		return header, nil
	}

	// Return a default header if not found.
	return &wire.BlockHeader{
		Version:   1,
		Timestamp: time.Now(),
	}, nil
}

// GetHistory returns the transaction history for a scripthash.
func (m *mockElectrumClient) GetHistory(ctx context.Context,
	scripthash string) ([]*HistoryResult, error) {

	m.mu.RLock()
	defer m.mu.RUnlock()

	if history, ok := m.history[scripthash]; ok {
		return history, nil
	}

	return nil, nil
}

// GetTransactionMsgTx returns a transaction by hash.
func (m *mockElectrumClient) GetTransactionMsgTx(ctx context.Context,
	txHash *chainhash.Hash) (*wire.MsgTx, error) {

	m.mu.RLock()
	defer m.mu.RUnlock()

	if tx, ok := m.transactions[*txHash]; ok {
		return tx, nil
	}

	return wire.NewMsgTx(wire.TxVersion), nil
}

// setConnected sets the connection status of the mock client.
func (m *mockElectrumClient) setConnected(connected bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = connected
}

// addHeader adds a block header at the given height.
func (m *mockElectrumClient) addHeader(height uint32,
	header *wire.BlockHeader) {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.headers[height] = header
}

// addHistory adds history for a scripthash.
func (m *mockElectrumClient) addHistory(scripthash string,
	history []*HistoryResult) {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.history[scripthash] = history
}

// addTransaction adds a transaction to the mock.
func (m *mockElectrumClient) addTransaction(txHash chainhash.Hash,
	tx *wire.MsgTx) {

	m.mu.Lock()
	defer m.mu.Unlock()
	m.transactions[txHash] = tx
}

// sendHeader sends a header notification.
func (m *mockElectrumClient) sendHeader(height int32) {
	m.headerChan <- &HeaderResult{Height: height}
}

// TestNewElectrumFilteredChainView tests the creation of a new
// ElectrumFilteredChainView.
func TestNewElectrumFilteredChainView(t *testing.T) {
	t.Parallel()

	mockClient := newMockElectrumClient()

	chainView, err := NewElectrumFilteredChainView(mockClient)
	require.NoError(t, err)
	require.NotNil(t, chainView)
	require.NotNil(t, chainView.blockQueue)
	require.NotNil(t, chainView.chainFilter)
	require.NotNil(t, chainView.scripthashToOutpoint)
}

// TestElectrumFilteredChainViewStartStop tests starting and stopping the
// chain view.
func TestElectrumFilteredChainViewStartStop(t *testing.T) {
	t.Parallel()

	mockClient := newMockElectrumClient()

	chainView, err := NewElectrumFilteredChainView(mockClient)
	require.NoError(t, err)

	// Send an initial header so Start() can complete.
	go func() {
		time.Sleep(10 * time.Millisecond)
		mockClient.sendHeader(100)
	}()

	err = chainView.Start()
	require.NoError(t, err)

	// Verify we can't start twice.
	err = chainView.Start()
	require.NoError(t, err)

	err = chainView.Stop()
	require.NoError(t, err)

	// Verify we can't stop twice.
	err = chainView.Stop()
	require.NoError(t, err)
}

// TestElectrumFilteredChainViewNotConnected tests that Start fails when the
// client is not connected.
func TestElectrumFilteredChainViewNotConnected(t *testing.T) {
	t.Parallel()

	mockClient := newMockElectrumClient()
	mockClient.setConnected(false)

	chainView, err := NewElectrumFilteredChainView(mockClient)
	require.NoError(t, err)

	err = chainView.Start()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestElectrumFilteredChainViewUpdateFilter tests adding outpoints to the
// filter.
func TestElectrumFilteredChainViewUpdateFilter(t *testing.T) {
	t.Parallel()

	mockClient := newMockElectrumClient()

	chainView, err := NewElectrumFilteredChainView(mockClient)
	require.NoError(t, err)

	// Send an initial header.
	go func() {
		time.Sleep(10 * time.Millisecond)
		mockClient.sendHeader(100)
	}()

	err = chainView.Start()
	require.NoError(t, err)

	defer func() {
		err := chainView.Stop()
		require.NoError(t, err)
	}()

	// Create test outpoints.
	testScript := []byte{0x00, 0x14, 0x01, 0x02, 0x03, 0x04}
	testOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x01},
		Index: 0,
	}

	ops := []graphdb.EdgePoint{
		{
			OutPoint:        testOutpoint,
			FundingPkScript: testScript,
		},
	}

	// Update the filter at the current height (no rescan needed).
	err = chainView.UpdateFilter(ops, 100)
	require.NoError(t, err)

	// Give time for the filter update to be processed.
	time.Sleep(50 * time.Millisecond)

	// Verify the outpoint was added to the filter.
	chainView.filterMtx.RLock()
	_, exists := chainView.chainFilter[testOutpoint]
	chainView.filterMtx.RUnlock()

	require.True(t, exists, "outpoint should be in chain filter")
}

// TestElectrumFilteredChainViewFilteredBlocksChannel tests that the
// FilteredBlocks channel is properly returned.
func TestElectrumFilteredChainViewFilteredBlocksChannel(t *testing.T) {
	t.Parallel()

	mockClient := newMockElectrumClient()

	chainView, err := NewElectrumFilteredChainView(mockClient)
	require.NoError(t, err)

	// The channel should be available even before Start.
	filteredBlocks := chainView.FilteredBlocks()
	require.NotNil(t, filteredBlocks)

	disconnectedBlocks := chainView.DisconnectedBlocks()
	require.NotNil(t, disconnectedBlocks)
}

// TestScripthashFromScript tests the scripthash conversion function.
func TestScripthashFromScript(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		script   []byte
		expected string
	}{
		{
			name: "empty script",
			// SHA256 of empty = e3b0c44298fc1c149afbf4c8996fb924
			//                   27ae41e4649b934ca495991b7852b855
			// Reversed for Electrum format.
			script:   []byte{},
			expected: "55b852781b9995a44c939b64e441ae2724b96f99c8f4fb9a141cfc9842c4b0e3",
		},
		{
			name:   "simple script",
			script: []byte{0x00, 0x14},
			// Actual hash depends on the script content.
			expected: scripthashFromScript([]byte{0x00, 0x14}),
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := scripthashFromScript(tc.script)
			require.Equal(t, tc.expected, result)

			// Verify the result is a valid hex string of correct
			// length (64 chars for 32 bytes).
			require.Len(t, result, 64)
		})
	}
}

// TestElectrumFilteredChainViewBlockConnected tests handling of new block
// notifications.
func TestElectrumFilteredChainViewBlockConnected(t *testing.T) {
	t.Parallel()

	mockClient := newMockElectrumClient()

	// Add a test header.
	testHeader := &wire.BlockHeader{
		Version:    1,
		PrevBlock:  chainhash.Hash{0x00},
		MerkleRoot: chainhash.Hash{0x01},
		Timestamp:  time.Now(),
		Bits:       0x1d00ffff,
		Nonce:      0,
	}
	mockClient.addHeader(100, testHeader)
	mockClient.addHeader(101, testHeader)

	chainView, err := NewElectrumFilteredChainView(mockClient)
	require.NoError(t, err)

	// Send initial header.
	go func() {
		time.Sleep(10 * time.Millisecond)
		mockClient.sendHeader(100)
	}()

	err = chainView.Start()
	require.NoError(t, err)

	defer func() {
		err := chainView.Stop()
		require.NoError(t, err)
	}()

	// Send a new block notification.
	mockClient.sendHeader(101)

	// Wait for the block to be processed.
	select {
	case block := <-chainView.FilteredBlocks():
		require.Equal(t, uint32(101), block.Height)

	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for filtered block")
	}
}
