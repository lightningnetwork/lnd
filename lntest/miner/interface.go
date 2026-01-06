package miner

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// MinerBackend defines the interface for different miner backend
// implementations (btcd vs bitcoind).
//
// We keep a single interface to preserve the existing lntest APIs while
// supporting multiple backends.
//
//nolint:interfacebloat
type MinerBackend interface {
	// Start starts the miner backend.
	//
	// For the btcd backend, these parameters map to rpctest.Harness.SetUp.
	// For other backends, these parameters can be ignored.
	Start(setupChain bool, numMatureOutputs uint32) error

	// Stop stops the miner backend and performs cleanup.
	Stop() error

	// GetBestBlock returns the hash and height of the best block.
	GetBestBlock() (*chainhash.Hash, int32, error)

	// GetRawMempool returns all transaction hashes in the mempool.
	GetRawMempool() ([]*chainhash.Hash, error)

	// Generate mines a specified number of blocks.
	Generate(blocks uint32) ([]*chainhash.Hash, error)

	// GetBlock returns the block for the given block hash.
	GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error)

	// GetRawTransaction returns the raw transaction for the given txid.
	GetRawTransaction(txid *chainhash.Hash) (*btcutil.Tx, error)

	// InvalidateBlock marks a block as invalid, triggering a reorg.
	InvalidateBlock(blockHash *chainhash.Hash) error

	// SendRawTransaction sends a raw transaction to the backend.
	SendRawTransaction(tx *wire.MsgTx, allowHighFees bool) (*chainhash.Hash,
		error)

	// NotifyNewTransactions registers for new transaction notifications.
	// For backends that don't support this, it should be a no-op.
	NotifyNewTransactions(verbose bool) error

	// SendOutputsWithoutChange creates and broadcasts a transaction with
	// the given outputs using the specified fee rate.
	SendOutputsWithoutChange(outputs []*wire.TxOut,
		feeRate btcutil.Amount) (*chainhash.Hash, error)

	// CreateTransaction creates a transaction with the given outputs.
	CreateTransaction(outputs []*wire.TxOut,
		feeRate btcutil.Amount) (*wire.MsgTx, error)

	// SendOutputs creates and broadcasts a transaction with the given
	// outputs.
	SendOutputs(outputs []*wire.TxOut,
		feeRate btcutil.Amount) (*chainhash.Hash, error)

	// GenerateAndSubmitBlock generates a block with the given transactions.
	GenerateAndSubmitBlock(txes []*btcutil.Tx, blockVersion int32,
		blockTime time.Time) (*btcutil.Block, error)

	// NewAddress generates a new address.
	NewAddress() (btcutil.Address, error)

	// P2PAddress returns the P2P address of the miner.
	P2PAddress() string

	// ConnectMiner connects this backend to a miner peer at the given
	// address (host:port).
	ConnectMiner(address string) error

	// DisconnectMiner disconnects this backend from a miner peer at the
	// given address (host:port).
	DisconnectMiner(address string) error

	// Name returns the name of the backend implementation.
	Name() string
}

// MinerConfig holds configuration for creating different miner backends.
type MinerConfig struct {
	// Backend specifies which backend to use ("btcd" or "bitcoind").
	Backend string

	// LogDir specifies the directory for log files.
	LogDir string

	// LogFilename specifies the log filename.
	LogFilename string

	// ExtraArgs contains additional command-line arguments for the backend.
	ExtraArgs []string
}
