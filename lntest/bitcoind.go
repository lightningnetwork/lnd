//go:build bitcoind && !notxindex && !rpcpolling
// +build bitcoind,!notxindex,!rpcpolling

package lntest

import (
	"github.com/btcsuite/btcd/chaincfg"
)

// NewBackend starts a bitcoind node with the txindex enabled and returns a
// BitcoindBackendConfig for that node.
func NewBackend(miner string, netParams *chaincfg.Params) (
	*BitcoindBackendConfig, func() error, error) {

	extraArgs := []string{
		"-regtest",
		"-txindex",
		"-disablewallet",
	}

	return newBackend(miner, netParams, extraArgs, false)
}
