//go:build bitcoind
// +build bitcoind

package lntest

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
)

// logDirPattern is the pattern of the name of the temporary log directory.
const logDirPattern = "%s/.backendlogs"

// BitcoindBackendConfig is an implementation of the BackendConfig interface
// backed by a Bitcoind node.
type BitcoindBackendConfig struct {
	rpcHost      string
	rpcUser      string
	rpcPass      string
	zmqBlockPath string
	zmqTxPath    string
	p2pPort      int
	rpcClient    *rpcclient.Client

	// minerAddr is the p2p address of the miner to connect to.
	minerAddr string
}

// A compile time assertion to ensure BitcoindBackendConfig meets the
// BackendConfig interface.
var _ BackendConfig = (*BitcoindBackendConfig)(nil)

// GenArgs returns the arguments needed to be passed to LND at startup for
// using this node as a chain backend.
func (b BitcoindBackendConfig) GenArgs() []string {
	var args []string
	args = append(args, "--bitcoin.node=bitcoind")
	args = append(args, fmt.Sprintf("--bitcoind.rpchost=%v", b.rpcHost))
	args = append(args, fmt.Sprintf("--bitcoind.rpcuser=%v", b.rpcUser))
	args = append(args, fmt.Sprintf("--bitcoind.rpcpass=%v", b.rpcPass))
	args = append(args, fmt.Sprintf("--bitcoind.zmqpubrawblock=%v",
		b.zmqBlockPath))
	args = append(args, fmt.Sprintf("--bitcoind.zmqpubrawtx=%v",
		b.zmqTxPath))

	return args
}

// ConnectMiner is called to establish a connection to the test miner.
func (b BitcoindBackendConfig) ConnectMiner() error {
	return b.rpcClient.AddNode(b.minerAddr, rpcclient.ANAdd)
}

// DisconnectMiner is called to disconnect the miner.
func (b BitcoindBackendConfig) DisconnectMiner() error {
	return b.rpcClient.AddNode(b.minerAddr, rpcclient.ANRemove)
}

// Name returns the name of the backend type.
func (b BitcoindBackendConfig) Name() string {
	return "bitcoind"
}

// newBackend starts a bitcoind node with the given extra parameters and returns
// a BitcoindBackendConfig for that node.
func newBackend(miner string, netParams *chaincfg.Params, extraArgs []string) (
	*BitcoindBackendConfig, func() error, error) {

	baseLogDir := fmt.Sprintf(logDirPattern, GetLogDir())
	if netParams != &chaincfg.RegressionNetParams {
		return nil, nil, fmt.Errorf("only regtest supported")
	}

	if err := os.MkdirAll(baseLogDir, 0700); err != nil {
		return nil, nil, err
	}

	logFile, err := filepath.Abs(baseLogDir + "/bitcoind.log")
	if err != nil {
		return nil, nil, err
	}

	tempBitcoindDir, err := ioutil.TempDir("", "bitcoind")
	if err != nil {
		return nil, nil,
			fmt.Errorf("unable to create temp directory: %v", err)
	}

	zmqBlockAddr := fmt.Sprintf("tcp://127.0.0.1:%d", NextAvailablePort())
	zmqTxAddr := fmt.Sprintf("tcp://127.0.0.1:%d", NextAvailablePort())
	rpcPort := NextAvailablePort()
	p2pPort := NextAvailablePort()

	cmdArgs := []string{
		"-datadir=" + tempBitcoindDir,
		"-whitelist=127.0.0.1", // whitelist localhost to speed up relay
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6f" +
			"d$507c670e800a95284294edb5773b05544b" +
			"220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		fmt.Sprintf("-port=%d", p2pPort),
		"-zmqpubrawblock=" + zmqBlockAddr,
		"-zmqpubrawtx=" + zmqTxAddr,
		"-debuglogfile=" + logFile,
	}
	cmdArgs = append(cmdArgs, extraArgs...)
	bitcoind := exec.Command("bitcoind", cmdArgs...)

	err = bitcoind.Start()
	if err != nil {
		if err := os.RemoveAll(tempBitcoindDir); err != nil {
			fmt.Printf("unable to remote temp dir %v: %v",
				tempBitcoindDir, err)
		}
		return nil, nil, fmt.Errorf("couldn't start bitcoind: %v", err)
	}

	cleanUp := func() error {
		_ = bitcoind.Process.Kill()
		_ = bitcoind.Wait()

		var errStr string
		// After shutting down the chain backend, we'll make a copy of
		// the log file before deleting the temporary log dir.
		logDestination := fmt.Sprintf(
			"%s/output_bitcoind_chainbackend.log", GetLogDir(),
		)
		err := CopyFile(logDestination, logFile)
		if err != nil {
			errStr += fmt.Sprintf("unable to copy file: %v\n", err)
		}
		if err = os.RemoveAll(baseLogDir); err != nil {
			errStr += fmt.Sprintf(
				"cannot remove dir %s: %v\n", baseLogDir, err,
			)
		}
		if err := os.RemoveAll(tempBitcoindDir); err != nil {
			errStr += fmt.Sprintf(
				"cannot remove dir %s: %v\n",
				tempBitcoindDir, err,
			)
		}
		if errStr != "" {
			return errors.New(errStr)
		}
		return nil
	}

	// Allow process to start.
	time.Sleep(1 * time.Second)

	rpcHost := fmt.Sprintf("127.0.0.1:%d", rpcPort)
	rpcUser := "weks"
	rpcPass := "weks"

	rpcCfg := rpcclient.ConnConfig{
		Host:                 rpcHost,
		User:                 rpcUser,
		Pass:                 rpcPass,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
		DisableTLS:           true,
		HTTPPostMode:         true,
	}

	client, err := rpcclient.New(&rpcCfg, nil)
	if err != nil {
		_ = cleanUp()
		return nil, nil, fmt.Errorf("unable to create rpc client: %v",
			err)
	}

	bd := BitcoindBackendConfig{
		rpcHost:      rpcHost,
		rpcUser:      rpcUser,
		rpcPass:      rpcPass,
		zmqBlockPath: zmqBlockAddr,
		zmqTxPath:    zmqTxAddr,
		p2pPort:      p2pPort,
		rpcClient:    client,
		minerAddr:    miner,
	}

	return &bd, cleanUp, nil
}
