package lntest

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest/node"
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

var harnessNetParams = &chaincfg.RegressionNetParams

type HarnessMiner struct {
	*testing.T
	*rpctest.Harness

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context //nolint:containedctx
	cancel context.CancelFunc

	// logPath is the directory path of the miner's logs.
	logPath string

	// logFilename is the saved log filename of the miner node.
	logFilename string
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

// newMiner creates a new miner using btcd's rpctest.
func newMiner(ctxb context.Context, t *testing.T, minerDirName,
	logFilename string) *HarnessMiner {

	t.Helper()

	handler := &rpcclient.NotificationHandlers{}
	btcdBinary := node.GetBtcdBinary()
	baseLogPath := fmt.Sprintf("%s/%s", node.GetLogDir(), minerDirName)

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

	miner, err := rpctest.New(harnessNetParams, handler, args, btcdBinary)
	require.NoError(t, err, "unable to create mining node")

	ctxt, cancel := context.WithCancel(ctxb)

	return &HarnessMiner{
		T:           t,
		Harness:     miner,
		runCtx:      ctxt,
		cancel:      cancel,
		logPath:     baseLogPath,
		logFilename: logFilename,
	}
}

// saveLogs copies the node logs and save it to the file specified by
// h.logFilename.
func (h *HarnessMiner) saveLogs() {
	// After shutting down the miner, we'll make a copy of the log files
	// before deleting the temporary log dir.
	path := fmt.Sprintf("%s/%s", h.logPath, harnessNetParams.Name)
	files, err := ioutil.ReadDir(path)
	require.NoError(h, err, "unable to read log directory")

	for _, file := range files {
		newFilename := strings.Replace(
			file.Name(), "btcd.log", h.logFilename, 1,
		)
		copyPath := fmt.Sprintf("%s/../%s", h.logPath, newFilename)

		logFile := fmt.Sprintf("%s/%s", path, file.Name())
		err := CopyFile(filepath.Clean(copyPath), logFile)
		require.NoError(h, err, "unable to copy file")
	}

	err = os.RemoveAll(h.logPath)
	require.NoErrorf(h, err, "cannot remove dir %s", h.logPath)
}

// Stop shuts down the miner and saves its logs.
func (h *HarnessMiner) Stop() {
	h.cancel()
	require.NoError(h, h.TearDown(), "tear down miner got error")
	h.saveLogs()
}

// GetBestBlock makes a RPC request to miner and asserts.
func (h *HarnessMiner) GetBestBlock() (*chainhash.Hash, int32) {
	blockHash, height, err := h.Client.GetBestBlock()
	require.NoError(h, err, "failed to GetBestBlock")

	return blockHash, height
}

// GetRawMempool makes a RPC call to the miner's GetRawMempool and
// asserts.
func (h *HarnessMiner) GetRawMempool() []*chainhash.Hash {
	mempool, err := h.Client.GetRawMempool()
	require.NoError(h, err, "unable to get mempool")

	return mempool
}

// GenerateBlocks mine 'num' of blocks and returns them.
func (h *HarnessMiner) GenerateBlocks(num uint32) []*chainhash.Hash {
	blockHashes, err := h.Client.Generate(num)
	require.NoError(h, err, "unable to generate blocks")
	require.Len(h, blockHashes, int(num), "wrong num of blocks generated")

	return blockHashes
}

// GetBlock gets a block using its block hash.
func (h *HarnessMiner) GetBlock(blockHash *chainhash.Hash) *wire.MsgBlock {
	block, err := h.Client.GetBlock(blockHash)
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
// in the provided miner's mempool. It will asserrt if this number is not met
// after the given timeout.
func (h *HarnessMiner) AssertNumTxsInMempool(n int) []*chainhash.Hash {
	var (
		mem []*chainhash.Hash
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
	txid *chainhash.Hash) {

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
func (h *HarnessMiner) GetRawTransaction(txid *chainhash.Hash) *btcutil.Tx {
	tx, err := h.Client.GetRawTransaction(txid)
	require.NoErrorf(h, err, "failed to get raw tx: %v", txid)
	return tx
}

// GetRawTransactionVerbose makes a RPC call to the miner's
// GetRawTransactionVerbose and asserts.
func (h *HarnessMiner) GetRawTransactionVerbose(
	txid *chainhash.Hash) *btcjson.TxRawResult {

	tx, err := h.Client.GetRawTransactionVerbose(txid)
	require.NoErrorf(h, err, "failed to get raw tx verbose: %v", txid)
	return tx
}

// AssertTxInMempool asserts a given transaction can be found in the mempool.
func (h *HarnessMiner) AssertTxInMempool(txid *chainhash.Hash) *wire.MsgTx {
	var msgTx *wire.MsgTx

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool := h.GetRawMempool()

		if len(mempool) == 0 {
			return fmt.Errorf("empty mempool")
		}

		for _, memTx := range mempool {
			// Check the values are equal.
			if *memTx == *txid {
				return nil
			}
		}

		return fmt.Errorf("txid %v not found in mempool: %v", txid,
			mempool)
	}, wait.MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking mempool")

	return msgTx
}

// SendOutputsWithoutChange uses the miner to send the given outputs using the
// specified fee rate and returns the txid.
func (h *HarnessMiner) SendOutputsWithoutChange(outputs []*wire.TxOut,
	feeRate btcutil.Amount) *chainhash.Hash {

	txid, err := h.Harness.SendOutputsWithoutChange(
		outputs, feeRate,
	)
	require.NoErrorf(h, err, "failed to send output")

	return txid
}

// CreateTransaction uses the miner to create a transaction using the given
// outputs using the specified fee rate and returns the transaction.
func (h *HarnessMiner) CreateTransaction(outputs []*wire.TxOut,
	feeRate btcutil.Amount) *wire.MsgTx {

	tx, err := h.Harness.CreateTransaction(outputs, feeRate, false)
	require.NoErrorf(h, err, "failed to create transaction")

	return tx
}

// SendOutput creates, signs, and finally broadcasts a transaction spending
// the harness' available mature coinbase outputs to create the new output.
func (h *HarnessMiner) SendOutput(newOutput *wire.TxOut,
	feeRate btcutil.Amount) *chainhash.Hash {

	hash, err := h.Harness.SendOutputs([]*wire.TxOut{newOutput}, feeRate)
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
		block, err := h.Client.GetBlock(blockHash)
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
			// We don't use `ht.Miner.GetRawTransaction` which
			// asserts a txid must be found. While iterating here,
			// the actual mempool state might have been changed,
			// causing a given txid being removed and cannot be
			// found. For instance, the aggregation logic used in
			// sweeping HTLC outputs will update the mempool by
			// replacing the HTLC spending txes with a single one.
			tx, err := h.Client.GetRawTransaction(txid)
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
	addr, err := h.NewAddress()
	require.NoError(h, err, "failed to create new miner address")
	return addr
}

// MineBlocksWithTxes mines a single block to include the specifies
// transactions only.
func (h *HarnessMiner) MineBlockWithTxes(txes []*btcutil.Tx) *wire.MsgBlock {
	var emptyTime time.Time

	// Generate a block.
	b, err := h.GenerateAndSubmitBlock(txes, -1, emptyTime)
	require.NoError(h, err, "unable to mine block")

	block, err := h.Client.GetBlock(b.Hash())
	require.NoError(h, err, "unable to get block")

	return block
}

// MineEmptyBlocks mines a given number of empty blocks.
func (h *HarnessMiner) MineEmptyBlocks(num int) []*wire.MsgBlock {
	var emptyTime time.Time

	blocks := make([]*wire.MsgBlock, num)
	for i := 0; i < num; i++ {
		// Generate an empty block.
		b, err := h.GenerateAndSubmitBlock(nil, -1, emptyTime)
		require.NoError(h, err, "unable to mine empty block")

		block := h.GetBlock(b.Hash())
		blocks[i] = block
	}

	return blocks
}
