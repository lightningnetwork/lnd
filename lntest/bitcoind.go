// +build bitcoind

package lntest

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
)

// logDir is the name of the temporary log directory.
const logDir = "./.backendlogs"

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

// NewBackend starts a bitcoind node and returns a BitoindBackendConfig for
// that node.
func NewBackend(miner string, netParams *chaincfg.Params) (
	*BitcoindBackendConfig, func(), error) {

	if netParams != &chaincfg.RegressionNetParams {
		return nil, nil, fmt.Errorf("only regtest supported")
	}

	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, nil, err
	}

	logFile, err := filepath.Abs(logDir + "/bitcoind.log")
	if err != nil {
		return nil, nil, err
	}

	tempBitcoindDir, err := ioutil.TempDir("", "bitcoind")
	if err != nil {
		return nil, nil,
			fmt.Errorf("unable to create temp directory: %v", err)
	}

	zmqBlockPath := "ipc:///" + tempBitcoindDir + "/blocks.socket"
	zmqTxPath := "ipc:///" + tempBitcoindDir + "/txs.socket"
	rpcPort := rand.Int()%(65536-1024) + 1024
	p2pPort := rand.Int()%(65536-1024) + 1024

	bitcoind := exec.Command(
		"bitcoind",
		"-datadir="+tempBitcoindDir,
		"-debug",
		"-regtest",
		"-txindex",
		"-whitelist=127.0.0.1", // whitelist localhost to speed up relay
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6f"+
			"d$507c670e800a95284294edb5773b05544b"+
			"220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		fmt.Sprintf("-port=%d", p2pPort),
		"-disablewallet",
		"-zmqpubrawblock="+zmqBlockPath,
		"-zmqpubrawtx="+zmqTxPath,
		"-debuglogfile="+logFile,
	)

	err = bitcoind.Start()
	if err != nil {
		os.RemoveAll(tempBitcoindDir)
		return nil, nil, fmt.Errorf("couldn't start bitcoind: %v", err)
	}

	cleanUp := func() {
		bitcoind.Process.Kill()
		bitcoind.Wait()

		// After shutting down the chain backend, we'll make a copy of
		// the log file before deleting the temporary log dir.
		err := CopyFile("./output_bitcoind_chainbackend.log", logFile)
		if err != nil {
			fmt.Printf("unable to copy file: %v\n", err)
		}
		if err = os.RemoveAll(logDir); err != nil {
			fmt.Printf("Cannot remove dir %s: %v\n", logDir, err)
		}

		os.RemoveAll(tempBitcoindDir)
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
		cleanUp()
		return nil, nil, fmt.Errorf("unable to create rpc client: %v",
			err)
	}

	// We start by adding the miner to the bitcoind addnode list. We do
	// this instead of connecting using command line flags, because it will
	// allow us to disconnect the miner using the AddNode command later.
	if err := client.AddNode(miner, rpcclient.ANAdd); err != nil {
		cleanUp()
		return nil, nil, fmt.Errorf("unable to add node: %v", err)
	}

	bd := BitcoindBackendConfig{
		rpcHost:      rpcHost,
		rpcUser:      rpcUser,
		rpcPass:      rpcPass,
		zmqBlockPath: zmqBlockPath,
		zmqTxPath:    zmqTxPath,
		p2pPort:      p2pPort,
		rpcClient:    client,
		minerAddr:    miner,
	}

	return &bd, cleanUp, nil
}
