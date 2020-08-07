// +build btcd

package lntest

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
)

// logDir is the name of the temporary log directory.
const logDir = "./.backendlogs"

// perm is used to signal we want to establish a permanent connection using the
// btcd Node API.
//
// NOTE: Cannot be const, since the node API expects a reference.
var perm = "perm"

// BtcdBackendConfig is an implementation of the BackendConfig interface
// backed by a btcd node.
type BtcdBackendConfig struct {
	// rpcConfig houses the connection config to the backing btcd instance.
	rpcConfig rpcclient.ConnConfig

	// harness is the backing btcd instance.
	harness *rpctest.Harness

	// minerAddr is the p2p address of the miner to connect to.
	minerAddr string
}

// A compile time assertion to ensure BtcdBackendConfig meets the BackendConfig
// interface.
var _ BackendConfig = (*BtcdBackendConfig)(nil)

// GenArgs returns the arguments needed to be passed to LND at startup for
// using this node as a chain backend.
func (b BtcdBackendConfig) GenArgs() []string {
	var args []string
	encodedCert := hex.EncodeToString(b.rpcConfig.Certificates)
	args = append(args, "--bitcoin.node=btcd")
	args = append(args, fmt.Sprintf("--btcd.rpchost=%v", b.rpcConfig.Host))
	args = append(args, fmt.Sprintf("--btcd.rpcuser=%v", b.rpcConfig.User))
	args = append(args, fmt.Sprintf("--btcd.rpcpass=%v", b.rpcConfig.Pass))
	args = append(args, fmt.Sprintf("--btcd.rawrpccert=%v", encodedCert))

	return args
}

// ConnectMiner is called to establish a connection to the test miner.
func (b BtcdBackendConfig) ConnectMiner() error {
	return b.harness.Node.Node(btcjson.NConnect, b.minerAddr, &perm)
}

// DisconnectMiner is called to disconnect the miner.
func (b BtcdBackendConfig) DisconnectMiner() error {
	return b.harness.Node.Node(btcjson.NRemove, b.minerAddr, &perm)
}

// Name returns the name of the backend type.
func (b BtcdBackendConfig) Name() string {
	return "btcd"
}

// NewBackend starts a new rpctest.Harness and returns a BtcdBackendConfig for
// that node. miner should be set to the P2P address of the miner to connect
// to.
func NewBackend(miner string, netParams *chaincfg.Params) (
	*BtcdBackendConfig, func(), error) {

	args := []string{
		"--rejectnonstd",
		"--txindex",
		"--trickleinterval=100ms",
		"--debuglevel=debug",
		"--logdir=" + logDir,
		"--connect=" + miner,
		"--nowinservice",
	}
	chainBackend, err := rpctest.New(netParams, nil, args)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create btcd node: %v", err)
	}

	if err := chainBackend.SetUp(false, 0); err != nil {
		return nil, nil, fmt.Errorf("unable to set up btcd backend: %v", err)
	}

	bd := &BtcdBackendConfig{
		rpcConfig: chainBackend.RPCConfig(),
		harness:   chainBackend,
		minerAddr: miner,
	}

	cleanUp := func() {
		chainBackend.TearDown()

		// After shutting down the chain backend, we'll make a copy of
		// the log file before deleting the temporary log dir.
		logFile := logDir + "/" + netParams.Name + "/btcd.log"
		err := CopyFile("./output_btcd_chainbackend.log", logFile)
		if err != nil {
			fmt.Printf("unable to copy file: %v\n", err)
		}
		if err = os.RemoveAll(logDir); err != nil {
			fmt.Printf("Cannot remove dir %s: %v\n", logDir, err)
		}
	}

	return bd, cleanUp, nil
}
