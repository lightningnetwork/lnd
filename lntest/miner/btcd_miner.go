package miner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// BtcdMinerBackend implements MinerBackend using btcd.
type BtcdMinerBackend struct {
	*testing.T
	*rpctest.Harness

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	//nolint:containedctx
	runCtx context.Context
	cancel context.CancelFunc

	// logPath is the directory path of the miner's logs.
	logPath string

	// logFilename is the saved log filename of the miner node.
	logFilename string
}

// NewBtcdMinerBackend creates a new btcd miner backend.
func NewBtcdMinerBackend(ctxb context.Context, t *testing.T,
	config *MinerConfig) *BtcdMinerBackend {

	t.Helper()

	logDir := config.LogDir
	if logDir == "" {
		logDir = minerLogDir
	}

	logFilename := config.LogFilename
	if logFilename == "" {
		logFilename = minerLogFilename
	}

	handler := &rpcclient.NotificationHandlers{}
	btcdBinary := node.GetBtcdBinary()
	baseLogPath := fmt.Sprintf("%s/%s", node.GetLogDir(), logDir)

	args := []string{
		"--rejectnonstd",
		"--txindex",
		"--nowinservice",
		"--nobanning",
		"--debuglevel=debug",
		"--logdir=" + baseLogPath,
		"--trickleinterval=100ms",
		// Don't disconnect if a reply takes too long.
		"--nostalldetect",
	}

	// Add any extra args from config.
	if config.ExtraArgs != nil {
		args = append(args, config.ExtraArgs...)
	}

	miner, err := rpctest.New(HarnessNetParams, handler, args, btcdBinary)
	require.NoError(t, err, "unable to create mining node")

	ctxt, cancel := context.WithCancel(ctxb)

	return &BtcdMinerBackend{
		T:           t,
		Harness:     miner,
		runCtx:      ctxt,
		cancel:      cancel,
		logPath:     baseLogPath,
		logFilename: logFilename,
	}
}

// Start starts the btcd miner backend.
func (b *BtcdMinerBackend) Start(setupChain bool,
	numMatureOutputs uint32) error {

	return b.SetUp(setupChain, numMatureOutputs)
}

// Stop stops the btcd miner backend and saves logs.
func (b *BtcdMinerBackend) Stop() error {
	b.cancel()
	if err := b.TearDown(); err != nil {
		return fmt.Errorf("tear down miner got error: %w", err)
	}
	b.saveLogs()

	return nil
}

// saveLogs copies the node logs and save it to the file specified by
// b.logFilename.
func (b *BtcdMinerBackend) saveLogs() {
	// After shutting down the miner, we'll make a copy of the log files
	// before deleting the temporary log dir.
	path := fmt.Sprintf("%s/%s", b.logPath, HarnessNetParams.Name)
	files, err := os.ReadDir(path)
	require.NoError(b, err, "unable to read log directory")

	for _, file := range files {
		newFilename := strings.Replace(
			file.Name(), "btcd.log", b.logFilename, 1,
		)
		copyPath := fmt.Sprintf("%s/../%s", b.logPath, newFilename)

		logFile := fmt.Sprintf("%s/%s", path, file.Name())
		err := node.CopyFile(filepath.Clean(copyPath), logFile)
		require.NoError(b, err, "unable to copy file")
	}

	err = os.RemoveAll(b.logPath)
	require.NoErrorf(b, err, "cannot remove dir %s", b.logPath)
}

// GetBestBlock returns the hash and height of the best block.
func (b *BtcdMinerBackend) GetBestBlock() (*chainhash.Hash, int32, error) {
	return b.Client.GetBestBlock()
}

// GetRawMempool returns all transaction hashes in the mempool.
func (b *BtcdMinerBackend) GetRawMempool() ([]*chainhash.Hash, error) {
	return b.Client.GetRawMempool()
}

// Generate mines a specified number of blocks.
func (b *BtcdMinerBackend) Generate(blocks uint32) ([]*chainhash.Hash, error) {
	return b.Client.Generate(blocks)
}

// GetBlock returns the block for the given block hash.
func (b *BtcdMinerBackend) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock,
	error) {

	return b.Client.GetBlock(blockHash)
}

// GetRawTransaction returns the raw transaction for the given txid.
func (b *BtcdMinerBackend) GetRawTransaction(txid *chainhash.Hash) (*btcutil.Tx,
	error) {

	return b.Client.GetRawTransaction(txid)
}

// InvalidateBlock marks a block as invalid, triggering a reorg.
func (b *BtcdMinerBackend) InvalidateBlock(blockHash *chainhash.Hash) error {
	return b.Client.InvalidateBlock(blockHash)
}

// SendRawTransaction sends a raw transaction to the backend.
func (b *BtcdMinerBackend) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	return b.Client.SendRawTransaction(tx, allowHighFees)
}

// NotifyNewTransactions registers for new transaction notifications.
func (b *BtcdMinerBackend) NotifyNewTransactions(verbose bool) error {
	return b.Client.NotifyNewTransactions(verbose)
}

// SendOutputsWithoutChange creates and broadcasts a transaction with
// the given outputs using the specified fee rate.
func (b *BtcdMinerBackend) SendOutputsWithoutChange(outputs []*wire.TxOut,
	feeRate btcutil.Amount) (*chainhash.Hash, error) {

	return b.Harness.SendOutputsWithoutChange(outputs, feeRate)
}

// CreateTransaction creates a transaction with the given outputs.
func (b *BtcdMinerBackend) CreateTransaction(outputs []*wire.TxOut,
	feeRate btcutil.Amount) (*wire.MsgTx, error) {

	return b.Harness.CreateTransaction(outputs, feeRate, false)
}

// SendOutputs creates and broadcasts a transaction with the given outputs.
func (b *BtcdMinerBackend) SendOutputs(outputs []*wire.TxOut,
	feeRate btcutil.Amount) (*chainhash.Hash, error) {

	return b.Harness.SendOutputs(outputs, feeRate)
}

// GenerateAndSubmitBlock generates a block with the given transactions.
func (b *BtcdMinerBackend) GenerateAndSubmitBlock(txes []*btcutil.Tx,
	blockVersion int32,
	blockTime time.Time) (*btcutil.Block, error) {

	return b.Harness.GenerateAndSubmitBlock(txes, blockVersion, blockTime)
}

// NewAddress generates a new address.
func (b *BtcdMinerBackend) NewAddress() (btcutil.Address, error) {
	return b.Harness.NewAddress()
}

// P2PAddress returns the P2P address of the miner.
func (b *BtcdMinerBackend) P2PAddress() string {
	return b.Harness.P2PAddress()
}

// Name returns the name of the backend implementation.
func (b *BtcdMinerBackend) Name() string {
	return "btcd"
}

// ConnectMiner connects this miner to another node using btcjson commands.
func (b *BtcdMinerBackend) ConnectMiner(address string) error {
	return b.Client.Node(btcjson.NConnect, address, &Temp)
}

// DisconnectMiner disconnects this miner from another node.
func (b *BtcdMinerBackend) DisconnectMiner(address string) error {
	return b.Client.Node(btcjson.NDisconnect, address, &Temp)
}
