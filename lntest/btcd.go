package lntest

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
)

// logDir is the name of the temporary log directory.
const logDir = "./.backendlogs"

// BtcdBackendConfig is an implementation of the BackendConfig interface
// backed by a btcd node.
type BtcdBackendConfig struct {
	// rpcConfig houses the connection config to the backing btcd instance.
	rpcConfig rpcclient.ConnConfig

	// p2pAddress is the p2p address of the btcd instance.
	p2pAddress string
}

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

// P2PAddr returns the address of this node to be used when connection over the
// Bitcoin P2P network.
func (b BtcdBackendConfig) P2PAddr() string {
	return b.p2pAddress
}

// NewBtcdBackend starts a new rpctest.Harness and returns a BtcdBackendConfig
// for that node.
func NewBtcdBackend() (*BtcdBackendConfig, func(), error) {
	args := []string{
		"--rejectnonstd",
		"--txindex",
		"--trickleinterval=100ms",
		"--debuglevel=debug",
		"--logdir=" + logDir,
	}
	netParams := &chaincfg.SimNetParams
	chainBackend, err := rpctest.New(netParams, nil, args)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create btcd node: %v", err)
	}

	if err := chainBackend.SetUp(false, 0); err != nil {
		return nil, nil, fmt.Errorf("unable to set up btcd backend: %v", err)
	}

	bd := &BtcdBackendConfig{
		rpcConfig:  chainBackend.RPCConfig(),
		p2pAddress: chainBackend.P2PAddress(),
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
