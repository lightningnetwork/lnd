//go:build neutrino && !sideload
// +build neutrino,!sideload

package lntest

import "github.com/btcsuite/btcd/chaincfg"

// NewBackend starts and returns a NeutrinoBackendConfig for the node.
func NewBackend(miner string, params *chaincfg.Params) (
	*NeutrinoBackendConfig, func() error, error) {

	return newBackend(miner, false, params)
}
