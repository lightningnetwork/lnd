package main

import (
	litecoinCfg "github.com/ltcsuite/ltcd/chaincfg"
	bitcoinCfg "github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/wire"
)

// activeNetParams is a pointer to the parameters specific to the currently
// active bitcoin network.
var activeNetParams = bitcoinTestNetParams

// bitcoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type bitcoinNetParams struct {
	*bitcoinCfg.Params
	rpcPort string
}

// litecoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type litecoinNetParams struct {
	*litecoinCfg.Params
	rpcPort string
}

// bitcoinMainNetParams contains parameters specific to the Bitcoin mainnet.
var bitcoinMainNetParams = bitcoinNetParams{
	Params:  &bitcoinCfg.MainNetParams,
	rpcPort: "8334",
}

// bitcoinTestNetParams contains parameters specific to the 3rd version of the
// test network.
var bitcoinTestNetParams = bitcoinNetParams{
	Params:  &bitcoinCfg.TestNet3Params,
	rpcPort: "18334",
}

// bitcoinSimNetParams contains parameters specific to the simulation test
// network.
var bitcoinSimNetParams = bitcoinNetParams{
	Params:  &bitcoinCfg.SimNetParams,
	rpcPort: "18556",
}

// regTestNetParams contains parameters specific to a local regtest network.
var regTestNetParams = bitcoinNetParams{
	Params:  &bitcoinCfg.RegressionNetParams,
	rpcPort: "18334",
}

// litecoinMainNetParams contains parameters specific to the Litecoin mainnet.
var litecoinMainNetParams = litecoinNetParams{
	Params:  &litecoinCfg.MainNetParams,
	rpcPort: "9334",
}

// litecoinTestNetParams contains parameters specific to the 4th version of the
// test network.
var litecoinTestNetParams = litecoinNetParams{
	Params:  &litecoinCfg.TestNet4Params,
	rpcPort: "19334",
}

// applyLitecoinParams applies the relevant chain configuration parameters that
// differ for litecoin to the chain parameters typed for btcsuite derivation.
// This function is used in place of using something like interface{} to
// abstract over _which_ chain (or fork) the parameters are for.
func applyLitecoinParams(params *bitcoinNetParams, paramsCopy *litecoinNetParams) {
	params.Name = paramsCopy.Name
	params.Net = wire.BitcoinNet(paramsCopy.Net)
	params.DefaultPort = paramsCopy.DefaultPort
	params.CoinbaseMaturity = paramsCopy.CoinbaseMaturity

	copy(params.GenesisHash[:], paramsCopy.GenesisHash[:])

	// Address encoding magics
	params.PubKeyHashAddrID = paramsCopy.PubKeyHashAddrID
	params.ScriptHashAddrID = paramsCopy.ScriptHashAddrID
	params.PrivateKeyID = paramsCopy.PrivateKeyID
	params.WitnessPubKeyHashAddrID = paramsCopy.WitnessPubKeyHashAddrID
	params.WitnessScriptHashAddrID = paramsCopy.WitnessScriptHashAddrID

	copy(params.HDPrivateKeyID[:], paramsCopy.HDPrivateKeyID[:])
	copy(params.HDPublicKeyID[:], paramsCopy.HDPublicKeyID[:])

	params.HDCoinType = paramsCopy.HDCoinType

	params.rpcPort = paramsCopy.rpcPort
}
