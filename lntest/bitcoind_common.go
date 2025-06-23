//go:build bitcoind
// +build bitcoind

package lntest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/port"
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
	rpcPolling   bool

	// minerAddr is the p2p address of the miner to connect to.
	minerAddr string
}

// A compile time assertion to ensure BitcoindBackendConfig meets the
// BackendConfig interface.
var _ node.BackendConfig = (*BitcoindBackendConfig)(nil)

// GenArgs returns the arguments needed to be passed to LND at startup for
// using this node as a chain backend.
func (b BitcoindBackendConfig) GenArgs() []string {
	var args []string
	args = append(args, "--bitcoin.node=bitcoind")
	args = append(args, fmt.Sprintf("--bitcoind.rpchost=%v", b.rpcHost))
	args = append(args, fmt.Sprintf("--bitcoind.rpcuser=%v", b.rpcUser))
	args = append(args, fmt.Sprintf("--bitcoind.rpcpass=%v", b.rpcPass))

	if b.rpcPolling {
		args = append(args, fmt.Sprintf("--bitcoind.rpcpolling"))
		args = append(args,
			fmt.Sprintf("--bitcoind.blockpollinginterval=10ms"))
		args = append(args,
			fmt.Sprintf("--bitcoind.txpollinginterval=10ms"))
	} else {
		args = append(args, fmt.Sprintf("--bitcoind.zmqpubrawblock=%v",
			b.zmqBlockPath))
		args = append(args, fmt.Sprintf("--bitcoind.zmqpubrawtx=%v",
			b.zmqTxPath))
	}

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

// Credentials returns the rpc username, password and host for the backend.
func (b BitcoindBackendConfig) Credentials() (string, string, string, error) {
	return b.rpcUser, b.rpcPass, b.rpcHost, nil
}

// Name returns the name of the backend type.
func (b BitcoindBackendConfig) Name() string {
	return "bitcoind"
}

// P2PAddr return bitcoin p2p ip:port.
func (b BitcoindBackendConfig) P2PAddr() (string, error) {
	return fmt.Sprintf("127.0.0.1:%d", b.p2pPort), nil
}

// newBackend starts a bitcoind node with the given extra parameters and returns
// a BitcoindBackendConfig for that node.
func newBackend(miner string, netParams *chaincfg.Params, extraArgs []string,
	rpcPolling bool) (*BitcoindBackendConfig, func() error, error) {

	baseLogDir := fmt.Sprintf(logDirPattern, node.GetLogDir())
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

	tempBitcoindDir, err := os.MkdirTemp("", "bitcoind")
	if err != nil {
		return nil, nil,
			fmt.Errorf("unable to create temp directory: %w", err)
	}

	zmqBlockAddr := fmt.Sprintf("tcp://127.0.0.1:%d",
		port.NextAvailablePort())
	zmqTxAddr := fmt.Sprintf("tcp://127.0.0.1:%d", port.NextAvailablePort())
	rpcPort := port.NextAvailablePort()
	p2pPort := port.NextAvailablePort()
	torBindPort := port.NextAvailablePort()

	cmdArgs := []string{
		"-datadir=" + tempBitcoindDir,
		"-whitelist=127.0.0.1", // whitelist localhost to speed up relay
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6f" +
			"d$507c670e800a95284294edb5773b05544b" +
			"220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		fmt.Sprintf("-bind=127.0.0.1:%d", p2pPort),
		fmt.Sprintf("-bind=127.0.0.1:%d=onion", torBindPort),
		"-zmqpubrawblock=" + zmqBlockAddr,
		"-zmqpubrawtx=" + zmqTxAddr,
		"-debug",
		"-debugexclude=libevent",
		"-debuglogfile=" + logFile,
		"-blockfilterindex",
		"-peerblockfilters",
	}
	cmdArgs = append(cmdArgs, extraArgs...)
	bitcoind := exec.Command("bitcoind", cmdArgs...)

	err = bitcoind.Start()
	if err != nil {
		if err := os.RemoveAll(tempBitcoindDir); err != nil {
			fmt.Printf("unable to remote temp dir %v: %v",
				tempBitcoindDir, err)
		}
		return nil, nil, fmt.Errorf("couldn't start bitcoind: %w", err)
	}

	cleanUp := func() error {
		_ = bitcoind.Process.Kill()
		_ = bitcoind.Wait()

		var errStr string
		// After shutting down the chain backend, we'll make a copy of
		// the log file before deleting the temporary log dir.
		logDestination := fmt.Sprintf(
			"%s/output_bitcoind_chainbackend.log", node.GetLogDir(),
		)
		err := node.CopyFile(logDestination, logFile)
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
		return nil, nil, fmt.Errorf("unable to create rpc client: %w",
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
		rpcPolling:   rpcPolling,
	}

	return &bd, cleanUp, nil
}
