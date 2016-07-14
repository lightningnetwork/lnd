package main

import "github.com/roasbeef/btcd/chaincfg"

// activeNetParams is a pointer to the parameters specific to the currently
// active bitcoin network.
var activeNetParams = segNetParams

// netParams couples the p2p parameters of a network with the corresponding RPC
// port of a daemon running on the particular network.
type netParams struct {
	*chaincfg.Params
	rpcPort string
}

// testNetParams contains parameters specific to the 3rd version of the test network.
var testNetParams = netParams{
	Params:  &chaincfg.TestNet3Params,
	rpcPort: "18334",
}

// segNetParams contains parameters specific to the segregated witness test
// network.
var segNetParams = netParams{
	Params:  &chaincfg.SegNet4Params,
	rpcPort: "28902",
}

// simNetParams contains parameters specific to the simulation test network.
var simNetParams = netParams{
	Params:  &chaincfg.SimNetParams,
	rpcPort: "18556",
}
