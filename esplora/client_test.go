package esplora

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewClient tests creating a new Esplora client.
func TestNewClient(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}

	client := NewClient(cfg)

	require.NotNil(t, client)
	require.NotNil(t, client.cfg)
	require.NotNil(t, client.httpClient)
	require.NotNil(t, client.subscribers)
	require.NotNil(t, client.quit)
	require.Equal(t, cfg.URL, client.cfg.URL)
	require.Equal(t, cfg.RequestTimeout, client.cfg.RequestTimeout)
	require.Equal(t, cfg.MaxRetries, client.cfg.MaxRetries)
	require.Equal(t, cfg.PollInterval, client.cfg.PollInterval)
}

// TestClientConfig tests the ClientConfig struct.
func TestClientConfig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		cfg  *ClientConfig
	}{
		{
			name: "minimal config",
			cfg: &ClientConfig{
				URL: "http://localhost:3002",
			},
		},
		{
			name: "full config",
			cfg: &ClientConfig{
				URL:            "https://blockstream.info/api",
				RequestTimeout: 60 * time.Second,
				MaxRetries:     5,
				PollInterval:   30 * time.Second,
			},
		},
		{
			name: "testnet config",
			cfg: &ClientConfig{
				URL:            "https://blockstream.info/testnet/api",
				RequestTimeout: 30 * time.Second,
				MaxRetries:     3,
				PollInterval:   10 * time.Second,
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := NewClient(tc.cfg)
			require.NotNil(t, client)
			require.Equal(t, tc.cfg.URL, client.cfg.URL)
		})
	}
}

// TestClientIsConnectedNotStarted tests that IsConnected returns false when
// the client is not started.
func TestClientIsConnectedNotStarted(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 1 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}

	client := NewClient(cfg)

	// Client should not be connected since we haven't started it.
	require.False(t, client.IsConnected())
}

// TestClientSubscribe tests that Subscribe returns a channel and ID.
func TestClientSubscribe(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}

	client := NewClient(cfg)

	// Subscribe should return a channel and ID.
	notifChan, id := client.Subscribe()
	require.NotNil(t, notifChan)
	require.Equal(t, uint64(0), id)

	// Second subscriber should get ID 1.
	notifChan2, id2 := client.Subscribe()
	require.NotNil(t, notifChan2)
	require.Equal(t, uint64(1), id2)

	// Unsubscribe should work.
	client.Unsubscribe(id)
	client.Unsubscribe(id2)
}

// TestClientGetBestBlock tests the GetBestBlock method.
func TestClientGetBestBlock(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:3002",
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		PollInterval:   10 * time.Second,
	}

	client := NewClient(cfg)

	// Before starting, best block should be empty.
	hash, height := client.GetBestBlock()
	require.Empty(t, hash)
	require.Equal(t, int64(0), height)
}

// TestClientStartStopNotConnected tests starting and stopping the client
// when no server is available.
func TestClientStartStopNotConnected(t *testing.T) {
	t.Parallel()

	cfg := &ClientConfig{
		URL:            "http://localhost:19999", // Non-existent port
		RequestTimeout: 1 * time.Second,
		MaxRetries:     0,
		PollInterval:   10 * time.Second,
	}

	client := NewClient(cfg)

	// Start should fail when server is not available.
	err := client.Start()
	require.Error(t, err)

	// Stop should still work without error.
	err = client.Stop()
	require.NoError(t, err)
}

// TestBlockInfoStruct tests the BlockInfo struct fields.
func TestBlockInfoStruct(t *testing.T) {
	t.Parallel()

	blockInfo := &BlockInfo{
		ID:                "00000000000000000001a2b3c4d5e6f7",
		Height:            800000,
		Version:           536870912,
		Timestamp:         1699999999,
		TxCount:           3000,
		Size:              1500000,
		Weight:            4000000,
		MerkleRoot:        "abcdef1234567890",
		PreviousBlockHash: "00000000000000000000fedcba987654",
		Nonce:             12345678,
		Bits:              386089497,
	}

	require.Equal(t, "00000000000000000001a2b3c4d5e6f7", blockInfo.ID)
	require.Equal(t, int64(800000), blockInfo.Height)
	require.Equal(t, int32(536870912), blockInfo.Version)
	require.Equal(t, int64(1699999999), blockInfo.Timestamp)
	require.Equal(t, 3000, blockInfo.TxCount)
}

// TestTxInfoStruct tests the TxInfo struct fields.
func TestTxInfoStruct(t *testing.T) {
	t.Parallel()

	txInfo := &TxInfo{
		TxID:     "abcdef1234567890abcdef1234567890",
		Version:  2,
		LockTime: 0,
		Size:     250,
		Weight:   1000,
		Fee:      5000,
		Status: TxStatus{
			Confirmed:   true,
			BlockHeight: 800000,
			BlockHash:   "00000000000000000001a2b3c4d5e6f7",
			BlockTime:   1699999999,
		},
	}

	require.Equal(t, "abcdef1234567890abcdef1234567890", txInfo.TxID)
	require.Equal(t, int32(2), txInfo.Version)
	require.True(t, txInfo.Status.Confirmed)
	require.Equal(t, int64(800000), txInfo.Status.BlockHeight)
}

// TestUTXOStruct tests the UTXO struct fields.
func TestUTXOStruct(t *testing.T) {
	t.Parallel()

	utxo := &UTXO{
		TxID:  "abcdef1234567890abcdef1234567890",
		Vout:  0,
		Value: 100000000,
		Status: TxStatus{
			Confirmed:   true,
			BlockHeight: 800000,
		},
	}

	require.Equal(t, "abcdef1234567890abcdef1234567890", utxo.TxID)
	require.Equal(t, uint32(0), utxo.Vout)
	require.Equal(t, int64(100000000), utxo.Value)
	require.True(t, utxo.Status.Confirmed)
}

// TestOutSpendStruct tests the OutSpend struct fields.
func TestOutSpendStruct(t *testing.T) {
	t.Parallel()

	// Unspent output.
	unspent := &OutSpend{
		Spent: false,
	}
	require.False(t, unspent.Spent)

	// Spent output.
	spent := &OutSpend{
		Spent: true,
		TxID:  "spendertxid1234567890",
		Vin:   0,
		Status: TxStatus{
			Confirmed:   true,
			BlockHeight: 800001,
		},
	}
	require.True(t, spent.Spent)
	require.Equal(t, "spendertxid1234567890", spent.TxID)
	require.Equal(t, uint32(0), spent.Vin)
}

// TestMerkleProofStruct tests the MerkleProof struct fields.
func TestMerkleProofStruct(t *testing.T) {
	t.Parallel()

	proof := &MerkleProof{
		BlockHeight: 800000,
		Merkle:      []string{"hash1", "hash2", "hash3"},
		Pos:         5,
	}

	require.Equal(t, int64(800000), proof.BlockHeight)
	require.Len(t, proof.Merkle, 3)
	require.Equal(t, 5, proof.Pos)
}

// TestFeeEstimatesStruct tests the FeeEstimates map type.
func TestFeeEstimatesStruct(t *testing.T) {
	t.Parallel()

	estimates := FeeEstimates{
		"1":   50.0,
		"2":   40.0,
		"3":   30.0,
		"6":   20.0,
		"12":  10.0,
		"25":  5.0,
		"144": 1.0,
	}

	require.Equal(t, float64(50.0), estimates["1"])
	require.Equal(t, float64(20.0), estimates["6"])
	require.Equal(t, float64(1.0), estimates["144"])
}

// TestClientConfigDefaults tests that zero values work correctly.
func TestClientConfigDefaults(t *testing.T) {
	t.Parallel()

	// Create a config with minimal settings.
	cfg := &ClientConfig{
		URL: "http://localhost:3002",
	}

	client := NewClient(cfg)
	require.NotNil(t, client)

	// HTTP client should have been created with zero timeout.
	require.NotNil(t, client.httpClient)
}

// TestTxVinStruct tests the TxVin struct fields.
func TestTxVinStruct(t *testing.T) {
	t.Parallel()

	// Regular input.
	vin := TxVin{
		TxID:         "previoustxid",
		Vout:         0,
		ScriptSig:    "scriptsighex",
		ScriptSigAsm: "OP_DUP OP_HASH160...",
		Witness:      []string{"witness1", "witness2"},
		Sequence:     0xffffffff,
		IsCoinbase:   false,
	}

	require.Equal(t, "previoustxid", vin.TxID)
	require.Equal(t, uint32(0), vin.Vout)
	require.False(t, vin.IsCoinbase)
	require.Len(t, vin.Witness, 2)

	// Coinbase input.
	coinbase := TxVin{
		IsCoinbase: true,
		Sequence:   0xffffffff,
	}

	require.True(t, coinbase.IsCoinbase)
}

// TestTxVoutStruct tests the TxVout struct fields.
func TestTxVoutStruct(t *testing.T) {
	t.Parallel()

	vout := TxVout{
		ScriptPubKey:     "76a914...88ac",
		ScriptPubKeyAsm:  "OP_DUP OP_HASH160...",
		ScriptPubKeyType: "pubkeyhash",
		ScriptPubKeyAddr: "1BitcoinAddress...",
		Value:            100000000,
	}

	require.Equal(t, "76a914...88ac", vout.ScriptPubKey)
	require.Equal(t, "pubkeyhash", vout.ScriptPubKeyType)
	require.Equal(t, "1BitcoinAddress...", vout.ScriptPubKeyAddr)
	require.Equal(t, int64(100000000), vout.Value)
}

// TestBlockStatusStruct tests the BlockStatus struct fields.
func TestBlockStatusStruct(t *testing.T) {
	t.Parallel()

	// Block in best chain.
	inBestChain := &BlockStatus{
		InBestChain: true,
		Height:      800000,
	}
	require.True(t, inBestChain.InBestChain)
	require.Equal(t, int64(800000), inBestChain.Height)

	// Orphaned block.
	orphaned := &BlockStatus{
		InBestChain: false,
		Height:      800000,
		NextBest:    "nextblockhash",
	}
	require.False(t, orphaned.InBestChain)
	require.Equal(t, "nextblockhash", orphaned.NextBest)
}

// TestClientErrors tests that error variables are defined correctly.
func TestClientErrors(t *testing.T) {
	t.Parallel()

	require.NotNil(t, ErrClientShutdown)
	require.NotNil(t, ErrNotConnected)
	require.NotNil(t, ErrBlockNotFound)
	require.NotNil(t, ErrTxNotFound)

	require.Contains(t, ErrClientShutdown.Error(), "shut down")
	require.Contains(t, ErrNotConnected.Error(), "not reachable")
	require.Contains(t, ErrBlockNotFound.Error(), "block not found")
	require.Contains(t, ErrTxNotFound.Error(), "transaction")
}
