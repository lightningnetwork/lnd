package miner

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

const (
	// minerLogFilename is the default log filename for the miner node.
	minerLogFilename = "output_btcd_miner.log"

	// minerLogDir is the default log dir for the miner node.
	minerLogDir = ".minerlogs"

	// slowMineDelay defines a wait period between mining new blocks.
	slowMineDelay = 100 * time.Millisecond
)

var (
	HarnessNetParams = &chaincfg.RegressionNetParams

	// temp is used to signal we want to establish a temporary connection
	// using the btcd Node API.
	//
	// NOTE: Cannot be const, since the node API expects a reference.
	Temp = "temp"
)

type HarnessMiner struct {
	*testing.T

	// backend is the underlying miner backend implementation (btcd or
	// bitcoind). This is always set.
	backend MinerBackend

	// Harness is the btcd-specific harness. This is only set when using
	// the btcd backend, and is nil when using bitcoind.
	*rpctest.Harness

	// ActiveNet is the miner network parameters used by lntest.
	//
	// This field exists for backwards compatibility with existing itests
	// that reference `ht.Miner().ActiveNet`. When using the bitcoind miner
	// backend, the embedded btcd rpctest harness is nil, but callers still
	// expect this value to be set.
	ActiveNet *chaincfg.Params

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	//
	// NOTE: The backend owns lifecycle management; these fields are kept
	// for compatibility with existing patterns in lntest.
	//nolint:containedctx
	runCtx context.Context
	cancel context.CancelFunc
}

// NewMiner creates a new miner using btcd backend with the default log file
// dir and name.
func NewMiner(ctxt context.Context, t *testing.T) *HarnessMiner {
	t.Helper()
	return newMiner(ctxt, t, minerLogDir, minerLogFilename)
}

// NewTempMiner creates a new miner using btcd backend with the specified log
// file dir and name.
func NewTempMiner(ctxt context.Context, t *testing.T,
	tempDir, tempLogFilename string) *HarnessMiner {

	t.Helper()

	return newMiner(ctxt, t, tempDir, tempLogFilename)
}

// NewBitcoindMiner creates a new miner using bitcoind backend with the
// default log file dir and name.
func NewBitcoindMiner(ctxt context.Context, t *testing.T) *HarnessMiner {
	t.Helper()
	return newBitcoindMiner(
		ctxt, t, minerLogDir, "output_bitcoind_miner.log",
	)
}

// NewBitcoindTempMiner creates a new miner using bitcoind backend with the
// specified log file dir and name.
func NewBitcoindTempMiner(ctxt context.Context, t *testing.T,
	tempDir, tempLogFilename string) *HarnessMiner {

	t.Helper()
	return newBitcoindMiner(ctxt, t, tempDir, tempLogFilename)
}

// NewMinerWithConfig creates a new miner with the specified configuration.
// The Backend field in the config determines which backend to use ("btcd" or
// "bitcoind").
func NewMinerWithConfig(ctxt context.Context, t *testing.T,
	config *MinerConfig) *HarnessMiner {

	t.Helper()

	// Set defaults if not specified.
	logDir := config.LogDir
	if logDir == "" {
		logDir = minerLogDir
	}

	logFilename := config.LogFilename
	if logFilename == "" {
		if config.Backend == "bitcoind" {
			logFilename = "output_bitcoind_miner.log"
		} else {
			logFilename = minerLogFilename
		}
	}

	// Choose backend based on config.
	switch config.Backend {
	case "bitcoind":
		return newBitcoindMinerWithConfig(ctxt, t, config, logDir,
			logFilename)
	case "btcd", "":
		// Default to btcd for backward compatibility.
		return newBtcdMinerWithConfig(ctxt, t, config, logDir,
			logFilename)
	default:
		require.Failf(t, "unknown backend",
			"backend %s not supported", config.Backend)
		return nil
	}
}

// newMiner creates a new miner using btcd's rpctest.
func newMiner(ctxb context.Context, t *testing.T, minerDirName,
	logFilename string) *HarnessMiner {

	t.Helper()

	config := &MinerConfig{
		Backend:     "btcd",
		LogDir:      minerDirName,
		LogFilename: logFilename,
	}

	return newBtcdMinerWithConfig(ctxb, t, config, minerDirName,
		logFilename)
}

// newBitcoindMiner creates a new miner using bitcoind.
func newBitcoindMiner(ctxb context.Context, t *testing.T, minerDirName,
	logFilename string) *HarnessMiner {

	t.Helper()

	config := &MinerConfig{
		Backend:     "bitcoind",
		LogDir:      minerDirName,
		LogFilename: logFilename,
	}

	return newBitcoindMinerWithConfig(ctxb, t, config, minerDirName,
		logFilename)
}

// newBtcdMinerWithConfig creates a new miner using btcd with the given config.
func newBtcdMinerWithConfig(ctxb context.Context, t *testing.T,
	config *MinerConfig, minerDirName, logFilename string) *HarnessMiner {

	t.Helper()

	// Create the btcd backend wrapper. Note that we don't start the backend
	// here, as callers (lntest harness) often tweak harness settings (e.g.
	// connection retries) before calling SetUp/Start.
	btcdBackend := NewBtcdMinerBackend(ctxb, t, &MinerConfig{
		Backend:     "btcd",
		LogDir:      minerDirName,
		LogFilename: logFilename,
		ExtraArgs:   config.ExtraArgs,
	})

	return &HarnessMiner{
		T:         t,
		backend:   btcdBackend,
		Harness:   btcdBackend.Harness,
		ActiveNet: HarnessNetParams,
		runCtx:    btcdBackend.runCtx,
		cancel:    btcdBackend.cancel,
	}
}

// newBitcoindMinerWithConfig creates a new miner using bitcoind with the
// given config.
func newBitcoindMinerWithConfig(ctxb context.Context, t *testing.T,
	config *MinerConfig, minerDirName, logFilename string) *HarnessMiner {

	t.Helper()

	// Create the bitcoind backend.
	bitcoindBackend := NewBitcoindMinerBackend(ctxb, t, &MinerConfig{
		Backend:     "bitcoind",
		LogDir:      minerDirName,
		LogFilename: logFilename,
		ExtraArgs:   config.ExtraArgs,
	})

	return &HarnessMiner{
		T:       t,
		backend: bitcoindBackend,
		// No btcd harness when using bitcoind.
		Harness:   nil,
		ActiveNet: HarnessNetParams,
		runCtx:    bitcoindBackend.runCtx,
		cancel:    bitcoindBackend.cancel,
	}
}

// Start starts the miner backend.
func (h *HarnessMiner) Start(setupChain bool, numMatureOutputs uint32) error {
	return h.backend.Start(setupChain, numMatureOutputs)
}

// NotifyNewTransactions registers for new transaction notifications.
func (h *HarnessMiner) NotifyNewTransactions(verbose bool) error {
	return h.backend.NotifyNewTransactions(verbose)
}

// Stop shuts down the miner and saves its logs.
func (h *HarnessMiner) Stop() {
	err := h.backend.Stop()
	require.NoError(h, err, "failed to stop miner backend")
}

// GetBestBlock makes a RPC request to miner and asserts.
func (h *HarnessMiner) GetBestBlock() (*chainhash.Hash, int32) {
	blockHash, height, err := h.backend.GetBestBlock()
	require.NoError(h, err, "failed to GetBestBlock")

	return blockHash, height
}

// GetRawMempool makes a RPC call to the miner's GetRawMempool and
// asserts.
func (h *HarnessMiner) GetRawMempool() []chainhash.Hash {
	mempool, err := h.backend.GetRawMempool()
	require.NoError(h, err, "unable to get mempool")

	txns := make([]chainhash.Hash, 0, len(mempool))
	for _, txid := range mempool {
		txns = append(txns, *txid)
	}

	return txns
}

// GenerateBlocks mine 'num' of blocks and returns them.
func (h *HarnessMiner) GenerateBlocks(num uint32) []*chainhash.Hash {
	blockHashes, err := h.backend.Generate(num)
	require.NoError(h, err, "unable to generate blocks")
	require.Len(h, blockHashes, int(num), "wrong num of blocks generated")

	return blockHashes
}

// GetBlock gets a block using its block hash.
func (h *HarnessMiner) GetBlock(blockHash *chainhash.Hash) *wire.MsgBlock {
	block, err := h.backend.GetBlock(blockHash)
	require.NoError(h, err, "unable to get block")

	return block
}

// MineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain.
func (h *HarnessMiner) MineBlocks(num uint32) []*wire.MsgBlock {
	blocks := make([]*wire.MsgBlock, num)

	blockHashes := h.GenerateBlocks(num)

	for i, blockHash := range blockHashes {
		block := h.GetBlock(blockHash)
		blocks[i] = block
	}

	return blocks
}

// AssertNumTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. It will assert if this number is not met
// after the given timeout.
func (h *HarnessMiner) AssertNumTxsInMempool(n int) []chainhash.Hash {
	var (
		mem []chainhash.Hash
		err error
	)

	err = wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mem = h.GetRawMempool()
		if len(mem) == n {
			return nil
		}

		return fmt.Errorf("want %v, got %v in mempool: %v",
			n, len(mem), mem)
	}, wait.MinerMempoolTimeout)
	require.NoError(h, err, "assert tx in mempool timeout")

	return mem
}

// AssertTxInBlock asserts that a given txid can be found in the passed block.
func (h *HarnessMiner) AssertTxInBlock(block *wire.MsgBlock,
	txid chainhash.Hash) {

	blockTxes := make([]chainhash.Hash, 0)

	for _, tx := range block.Transactions {
		sha := tx.TxHash()
		blockTxes = append(blockTxes, sha)

		if bytes.Equal(txid[:], sha[:]) {
			return
		}
	}

	require.Failf(h, "tx was not included in block", "tx:%v, block has:%v",
		txid, blockTxes)
}

// MineBlocksAndAssertNumTxes mine 'num' of blocks and check that blocks are
// present in node blockchain. numTxs should be set to the number of
// transactions (excluding the coinbase) we expect to be included in the first
// mined block.
func (h *HarnessMiner) MineBlocksAndAssertNumTxes(num uint32,
	numTxs int) []*wire.MsgBlock {

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	txids := h.AssertNumTxsInMempool(numTxs)

	// Mine blocks.
	blocks := h.MineBlocks(num)

	// Finally, assert that all the transactions were included in the first
	// block.
	for _, txid := range txids {
		h.AssertTxInBlock(blocks[0], txid)
	}

	return blocks
}

// GetRawTransaction makes a RPC call to the miner's GetRawTransaction and
// asserts.
func (h *HarnessMiner) GetRawTransaction(txid chainhash.Hash) *btcutil.Tx {
	tx, err := h.backend.GetRawTransaction(&txid)
	require.NoErrorf(h, err, "failed to get raw tx: %v", txid)
	return tx
}

// GetRawTransactionNoAssert makes a RPC call to the miner's GetRawTransaction
// and returns the error to the caller.
func (h *HarnessMiner) GetRawTransactionNoAssert(
	txid chainhash.Hash) (*btcutil.Tx, error) {

	return h.backend.GetRawTransaction(&txid)
}

// InvalidateBlock marks a block as invalid, triggering a reorg.
func (h *HarnessMiner) InvalidateBlock(blockHash *chainhash.Hash) error {
	return h.backend.InvalidateBlock(blockHash)
}

// GetRawTransactionVerbose makes a RPC call to the miner's
// GetRawTransactionVerbose and asserts.
func (h *HarnessMiner) GetRawTransactionVerbose(
	txid chainhash.Hash) *btcjson.TxRawResult {

	// This method is only supported for btcd backend currently.
	// For bitcoind, callers should use GetRawTransaction instead.
	if h.Harness == nil {
		require.Fail(h, "GetRawTransactionVerbose not supported for "+
			"this backend, use GetRawTransaction instead")
		return nil
	}

	tx, err := h.Client.GetRawTransactionVerbose(&txid)
	require.NoErrorf(h, err, "failed to get raw tx verbose: %v", txid)
	return tx
}

// AssertTxInMempool asserts a given transaction can be found in the mempool.
func (h *HarnessMiner) AssertTxInMempool(txid chainhash.Hash) *wire.MsgTx {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool := h.GetRawMempool()

		if len(mempool) == 0 {
			return fmt.Errorf("empty mempool")
		}

		result := fn.Find(mempool, fn.Eq(txid))

		if result.IsNone() {
			return fmt.Errorf("txid %v not found in "+
				"mempool: %v", txid, mempool)
		}

		return nil
	}, wait.MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking mempool")

	return h.GetRawTransaction(txid).MsgTx()
}

// AssertTxnsNotInMempool asserts the given txns are not found in the mempool.
// It assumes the mempool is not empty.
func (h *HarnessMiner) AssertTxnsNotInMempool(txids []chainhash.Hash) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool := h.GetRawMempool()

		// Turn the mempool into a txn set for faster lookups.
		mempoolTxns := fn.NewSet(mempool...)

		// Check if any of the txids are in the mempool.
		for _, txid := range txids {
			// Skip if the tx is not in the mempool.
			if !mempoolTxns.Contains(txid) {
				continue
			}

			return fmt.Errorf("expect txid %v to be NOT found in "+
				"mempool", txid)
		}

		return nil
	}, wait.MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking txns not in mempool")
}

// AssertTxNotInMempool asserts a given transaction cannot be found in the
// mempool. It assumes the mempool is not empty.
//
// NOTE: this should be used after `AssertTxInMempool` to ensure the tx has
// entered the mempool before. Otherwise it might give false positive and the
// tx may enter the mempool after the check.
func (h *HarnessMiner) AssertTxNotInMempool(txid chainhash.Hash) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool := h.GetRawMempool()

		for _, memTx := range mempool {
			// Check the values are equal.
			if txid == memTx {
				return fmt.Errorf("expect txid %v to be NOT "+
					"found in mempool", txid)
			}
		}

		return nil
	}, wait.MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking tx not in mempool")
}

// SendOutputsWithoutChange uses the miner to send the given outputs using the
// specified fee rate and returns the txid.
func (h *HarnessMiner) SendOutputsWithoutChange(outputs []*wire.TxOut,
	feeRate btcutil.Amount) *chainhash.Hash {

	txid, err := h.backend.SendOutputsWithoutChange(outputs, feeRate)
	require.NoErrorf(h, err, "failed to send output")

	return txid
}

// CreateTransaction uses the miner to create a transaction using the given
// outputs using the specified fee rate and returns the transaction.
func (h *HarnessMiner) CreateTransaction(outputs []*wire.TxOut,
	feeRate btcutil.Amount) *wire.MsgTx {

	tx, err := h.backend.CreateTransaction(outputs, feeRate)
	require.NoErrorf(h, err, "failed to create transaction")

	return tx
}

// SendOutput creates, signs, and finally broadcasts a transaction spending
// the harness' available mature coinbase outputs to create the new output.
func (h *HarnessMiner) SendOutput(newOutput *wire.TxOut,
	feeRate btcutil.Amount) *chainhash.Hash {

	hash, err := h.backend.SendOutputs([]*wire.TxOut{newOutput}, feeRate)
	require.NoErrorf(h, err, "failed to send outputs")

	return hash
}

// MineBlocksSlow mines 'num' of blocks. Between each mined block an artificial
// delay is introduced to give all network participants time to catch up.
func (h *HarnessMiner) MineBlocksSlow(num uint32) []*wire.MsgBlock {
	blocks := make([]*wire.MsgBlock, num)
	blockHashes := make([]*chainhash.Hash, 0, num)

	for i := uint32(0); i < num; i++ {
		generatedHashes := h.GenerateBlocks(1)
		blockHashes = append(blockHashes, generatedHashes...)

		time.Sleep(slowMineDelay)
	}

	for i, blockHash := range blockHashes {
		block, err := h.backend.GetBlock(blockHash)
		require.NoError(h, err, "get blocks")

		blocks[i] = block
	}

	return blocks
}

// AssertOutpointInMempool asserts a given outpoint can be found in the mempool.
func (h *HarnessMiner) AssertOutpointInMempool(op wire.OutPoint) *wire.MsgTx {
	var msgTx *wire.MsgTx

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool := h.GetRawMempool()

		if len(mempool) == 0 {
			return fmt.Errorf("empty mempool")
		}

		for _, txid := range mempool {
			// We don't use `ht.GetRawTransaction` which
			// asserts a txid must be found. While iterating here,
			// the actual mempool state might have been changed,
			// causing a given txid being removed and cannot be
			// found. For instance, the aggregation logic used in
			// sweeping HTLC outputs will update the mempool by
			// replacing the HTLC spending txes with a single one.
			tx, err := h.backend.GetRawTransaction(&txid)
			if err != nil {
				return err
			}

			msgTx = tx.MsgTx()
			for _, txIn := range msgTx.TxIn {
				if txIn.PreviousOutPoint == op {
					return nil
				}
			}
		}

		return fmt.Errorf("outpoint %v not found in mempool", op)
	}, wait.MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking mempool")

	return msgTx
}

// GetNumTxsFromMempool polls until finding the desired number of transactions
// in the miner's mempool and returns the full transactions to the caller.
func (h *HarnessMiner) GetNumTxsFromMempool(n int) []*wire.MsgTx {
	txids := h.AssertNumTxsInMempool(n)

	var txes []*wire.MsgTx
	for _, txid := range txids {
		tx := h.GetRawTransaction(txid)
		txes = append(txes, tx.MsgTx())
	}

	return txes
}

// NewMinerAddress creates a new address for the miner and asserts.
func (h *HarnessMiner) NewMinerAddress() btcutil.Address {
	addr, err := h.backend.NewAddress()
	require.NoError(h, err, "failed to create new miner address")
	return addr
}

// MineBlocksWithTxes mines a single block to include the specifies
// transactions only.
func (h *HarnessMiner) MineBlockWithTxes(txes []*btcutil.Tx) *wire.MsgBlock {
	var emptyTime time.Time

	// Generate a block.
	b, err := h.backend.GenerateAndSubmitBlock(txes, -1, emptyTime)
	require.NoError(h, err, "unable to mine block")

	block, err := h.backend.GetBlock(b.Hash())
	require.NoError(h, err, "unable to get block")

	// Make sure the mempool has been updated.
	for _, tx := range txes {
		h.AssertTxNotInMempool(*tx.Hash())
	}

	return block
}

// MineBlocksWithTx mines a single block to include the specifies tx only.
func (h *HarnessMiner) MineBlockWithTx(tx *wire.MsgTx) *wire.MsgBlock {
	var emptyTime time.Time

	txes := []*btcutil.Tx{btcutil.NewTx(tx)}

	// Generate a block.
	b, err := h.backend.GenerateAndSubmitBlock(txes, -1, emptyTime)
	require.NoError(h, err, "unable to mine block")

	block, err := h.backend.GetBlock(b.Hash())
	require.NoError(h, err, "unable to get block")

	// Make sure the mempool has been updated.
	h.AssertTxNotInMempool(tx.TxHash())

	return block
}

// MineEmptyBlocks mines a given number of empty blocks.
func (h *HarnessMiner) MineEmptyBlocks(num int) []*wire.MsgBlock {
	var emptyTime time.Time

	blocks := make([]*wire.MsgBlock, num)
	for i := 0; i < num; i++ {
		// Generate an empty block.
		b, err := h.backend.GenerateAndSubmitBlock(nil, -1, emptyTime)
		require.NoError(h, err, "unable to mine empty block")

		block := h.GetBlock(b.Hash())
		blocks[i] = block
	}

	return blocks
}

// SpawnTempMiner creates a temp miner and syncs it with the current miner.
// Once miners are synced, the temp miner is disconnected from the original
// miner and returned.
func (h *HarnessMiner) SpawnTempMiner() *HarnessMiner {
	require := require.New(h.T)

	// Setup a temp miner.
	tempLogDir := ".tempminerlogs"
	logFilename := "output-temp_miner.log"
	var tempMiner *HarnessMiner
	switch h.BackendName() {
	case "bitcoind":
		tempMiner = NewBitcoindTempMiner(
			h.runCtx, h.T, tempLogDir, logFilename,
		)
	case "btcd":
		tempMiner = NewTempMiner(h.runCtx, h.T, tempLogDir, logFilename)
	default:
		require.Failf("unknown miner backend",
			"backend %s not supported", h.BackendName())
		return nil
	}

	// Make sure to clean the miner when the test ends.
	h.T.Cleanup(tempMiner.Stop)

	// Start the miner.
	require.NoError(tempMiner.Start(false, 0), "unable to start miner")

	// Connect the temp miner to the original miner.
	err := h.backend.ConnectMiner(tempMiner.P2PAddress())
	require.NoError(err, "unable to connect node")

	// Sync the blocks.
	if h.Harness != nil && tempMiner.Harness != nil {
		nodeSlice := []*rpctest.Harness{h.Harness, tempMiner.Harness}
		err = rpctest.JoinNodes(nodeSlice, rpctest.Blocks)
		require.NoError(err, "unable to join node on blocks")
	} else {
		h.AssertMinerBlockHeightDelta(tempMiner, 0)
	}

	// The two miners should be on the same block height.
	h.AssertMinerBlockHeightDelta(tempMiner, 0)

	// Once synced, we now disconnect the temp miner so it'll be
	// independent from the original miner.
	err = h.backend.DisconnectMiner(tempMiner.P2PAddress())
	require.NoError(err, "unable to disconnect miners")

	return tempMiner
}

// ConnectMiner connects the miner to a temp miner.
func (h *HarnessMiner) ConnectMiner(tempMiner *HarnessMiner) {
	require := require.New(h.T)

	// Connect the current miner to the temporary miner.
	err := h.backend.ConnectMiner(tempMiner.P2PAddress())
	require.NoError(err, "unable to connect temp miner")

	if h.Harness != nil && tempMiner.Harness != nil {
		nodes := []*rpctest.Harness{tempMiner.Harness, h.Harness}
		err = rpctest.JoinNodes(nodes, rpctest.Blocks)
		require.NoError(err, "unable to join node on blocks")
	} else {
		h.AssertMinerBlockHeightDelta(tempMiner, 0)
	}
}

// DisconnectMiner disconnects the miner from the temp miner.
func (h *HarnessMiner) DisconnectMiner(tempMiner *HarnessMiner) {
	err := h.backend.DisconnectMiner(tempMiner.P2PAddress())
	require.NoError(h.T, err, "unable to disconnect temp miner")
}

// AssertMinerBlockHeightDelta ensures that tempMiner is 'delta' blocks ahead
// of miner.
func (h *HarnessMiner) AssertMinerBlockHeightDelta(tempMiner *HarnessMiner,
	delta int32) {

	// Ensure the chain lengths are what we expect.
	err := wait.NoError(func() error {
		_, tempMinerHeight, err := tempMiner.backend.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get current "+
				"blockheight %v", err)
		}

		_, minerHeight, err := h.backend.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get current "+
				"blockheight %v", err)
		}

		if tempMinerHeight != minerHeight+delta {
			return fmt.Errorf("expected new miner(%d) to be %d "+
				"blocks ahead of original miner(%d)",
				tempMinerHeight, delta, minerHeight)
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(h.T, err, "failed to assert block height delta")
}

// P2PAddress returns the P2P address of the miner.
func (h *HarnessMiner) P2PAddress() string {
	return h.backend.P2PAddress()
}

// BackendName returns the name of the backend implementation.
func (h *HarnessMiner) BackendName() string {
	return h.backend.Name()
}

// SendRawTransaction sends a raw transaction with optional high fee allowance.
func (h *HarnessMiner) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	return h.backend.SendRawTransaction(tx, allowHighFees)
}
