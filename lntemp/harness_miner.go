package lntemp

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

const (
	// minerLogFilename is the default log filename for the miner node.
	minerLogFilename = "output_btcd_miner.log"

	// minerLogDir is the default log dir for the miner node.
	minerLogDir = ".minerlogs"
)

var harnessNetParams = &chaincfg.RegressionNetParams

type HarnessMiner struct {
	*testing.T
	*rpctest.Harness

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	runCtx context.Context
	cancel context.CancelFunc

	// logPath is the directory path of the miner's logs.
	logPath string

	// logFilename is the saved log filename of the miner node.
	logFilename string
}

// NewMiner creates a new miner using btcd backend with the default log file
// dir and name.
func NewMiner(ctxt context.Context, t *testing.T) *HarnessMiner {
	return newMiner(ctxt, t, minerLogDir, minerLogFilename)
}

// newMiner creates a new miner using btcd's rpctest.
func newMiner(ctxb context.Context, t *testing.T, minerDirName,
	logFilename string) *HarnessMiner {

	handler := &rpcclient.NotificationHandlers{}
	btcdBinary := lntest.GetBtcdBinary()
	baseLogPath := fmt.Sprintf("%s/%s", lntest.GetLogDir(), minerDirName)

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
	}, lntest.MinerMempoolTimeout)
	require.NoError(h, err, "assert tx in mempool timeout")

	return mem
}
