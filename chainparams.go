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

// TODO(roasbeef): blacklist ipv6 local host as well

// litecoinTestNetParams contains parameters specific to the 4th version of the
// test network.
var litecoinTestNetParams = litecoinNetParams{
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

	params.Name = litecoinTestNetParams.Name
	params.Net = wire.BitcoinNet(litecoinTestNetParams.Net)
	params.DefaultPort = litecoinTestNetParams.DefaultPort
	params.CoinbaseMaturity = litecoinTestNetParams.CoinbaseMaturity

	copy(params.GenesisHash[:], litecoinTestNetParams.GenesisHash[:])

	// Address encoding magics
	params.PubKeyHashAddrID = litecoinTestNetParams.PubKeyHashAddrID
	params.ScriptHashAddrID = litecoinTestNetParams.ScriptHashAddrID
	params.PrivateKeyID = litecoinTestNetParams.PrivateKeyID
	params.WitnessPubKeyHashAddrID = litecoinTestNetParams.WitnessPubKeyHashAddrID
	params.WitnessScriptHashAddrID = litecoinTestNetParams.WitnessScriptHashAddrID
	params.Bech32HRPSegwit = litecoinTestNetParams.Bech32HRPSegwit

	copy(params.HDPrivateKeyID[:], litecoinTestNetParams.HDPrivateKeyID[:])
	copy(params.HDPublicKeyID[:], litecoinTestNetParams.HDPublicKeyID[:])

	params.HDCoinType = litecoinTestNetParams.HDCoinType

	checkPoints := make([]chaincfg.Checkpoint, len(litecoinTestNetParams.Checkpoints))
	for i := 0; i < len(litecoinTestNetParams.Checkpoints); i++ {
		var chainHash chainhash.Hash
		copy(chainHash[:], litecoinTestNetParams.Checkpoints[i].Hash[:])

		checkPoints[i] = chaincfg.Checkpoint{
			Height: litecoinTestNetParams.Checkpoints[i].Height,
			Hash:   &chainHash,
		}
	}
	params.Checkpoints = checkPoints

	params.rpcPort = litecoinTestNetParams.rpcPort
	params.CoinType = litecoinTestNetParams.CoinType
}
