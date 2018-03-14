package main

import (
	"github.com/lightningnetwork/lnd/keychain"
	litecoinCfg "github.com/ltcsuite/ltcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg"
	bitcoinCfg "github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// activeNetParams is a pointer to the parameters specific to the currently
// active bitcoin network.
var activeNetParams = bitcoinTestNetParams

// bitcoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type bitcoinNetParams struct {
	*bitcoinCfg.Params
	rpcPort  string
	CoinType uint32
}

// litecoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type litecoinNetParams struct {
	*litecoinCfg.Params
	rpcPort  string
	CoinType uint32
}

// bitcoinTestNetParams contains parameters specific to the 3rd version of the
// test network.
var bitcoinTestNetParams = bitcoinNetParams{
	Params:   &bitcoinCfg.TestNet3Params,
	rpcPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// bitcoinSimNetParams contains parameters specific to the simulation test
// network.
var bitcoinSimNetParams = bitcoinNetParams{
	Params:   &bitcoinCfg.SimNetParams,
	rpcPort:  "18556",
	CoinType: keychain.CoinTypeTestnet,
}

// liteTestNetParams contains parameters specific to the 4th version of the
// test network.
var liteTestNetParams = litecoinNetParams{
	Params:   &litecoinCfg.TestNet4Params,
	rpcPort:  "19334",
	CoinType: keychain.CoinTypeTestnet,
}

// regTestNetParams contains parameters specific to a local regtest network.
var regTestNetParams = bitcoinNetParams{
	Params:   &bitcoinCfg.RegressionNetParams,
	rpcPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// applyLitecoinParams applies the relevant chain configuration parameters that
// differ for litecoin to the chain parameters typed for btcsuite derivation.
// This function is used in place of using something like interface{} to
// abstract over _which_ chain (or fork) the parameters are for.
func applyLitecoinParams(params *bitcoinNetParams) {
	params.Name = liteTestNetParams.Name
	params.Net = wire.BitcoinNet(liteTestNetParams.Net)
	params.DefaultPort = liteTestNetParams.DefaultPort
	params.CoinbaseMaturity = liteTestNetParams.CoinbaseMaturity

	copy(params.GenesisHash[:], liteTestNetParams.GenesisHash[:])

	// Address encoding magics
	params.PubKeyHashAddrID = liteTestNetParams.PubKeyHashAddrID
	params.ScriptHashAddrID = liteTestNetParams.ScriptHashAddrID
	params.PrivateKeyID = liteTestNetParams.PrivateKeyID
	params.WitnessPubKeyHashAddrID = liteTestNetParams.WitnessPubKeyHashAddrID
	params.WitnessScriptHashAddrID = liteTestNetParams.WitnessScriptHashAddrID
	params.Bech32HRPSegwit = liteTestNetParams.Bech32HRPSegwit

	copy(params.HDPrivateKeyID[:], liteTestNetParams.HDPrivateKeyID[:])
	copy(params.HDPublicKeyID[:], liteTestNetParams.HDPublicKeyID[:])

	params.HDCoinType = liteTestNetParams.HDCoinType

	checkPoints := make([]chaincfg.Checkpoint, len(liteTestNetParams.Checkpoints))
	for i := 0; i < len(liteTestNetParams.Checkpoints); i++ {
		var chainHash chainhash.Hash
		copy(chainHash[:], liteTestNetParams.Checkpoints[i].Hash[:])

		checkPoints[i] = chaincfg.Checkpoint{
			Height: liteTestNetParams.Checkpoints[i].Height,
			Hash:   &chainHash,
		}
	}
	params.Checkpoints = checkPoints

	params.rpcPort = liteTestNetParams.rpcPort
	params.CoinType = liteTestNetParams.CoinType
}
