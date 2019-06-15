package lnd

import (
	"github.com/btcsuite/btcd/chaincfg"
	bitcoinCfg "github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	bitcoinWire "github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	litecoinCfg "github.com/ltcsuite/ltcd/chaincfg"
	litecoinWire "github.com/ltcsuite/ltcd/wire"
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

// bitcoinMainNetParams contains parameters specific to the current Bitcoin
// mainnet.
var bitcoinMainNetParams = bitcoinNetParams{
	Params:   &bitcoinCfg.MainNetParams,
	rpcPort:  "8334",
	CoinType: keychain.CoinTypeBitcoin,
}

// bitcoinSimNetParams contains parameters specific to the simulation test
// network.
var bitcoinSimNetParams = bitcoinNetParams{
	Params:   &bitcoinCfg.SimNetParams,
	rpcPort:  "18556",
	CoinType: keychain.CoinTypeTestnet,
}

// litecoinSimNetParams contains parameters specific to the simulation test
// network.
var litecoinSimNetParams = litecoinNetParams{
	Params:   &litecoinCfg.SimNetParams,
	rpcPort:  "18556",
	CoinType: keychain.CoinTypeTestnet,
}

// litecoinTestNetParams contains parameters specific to the 4th version of the
// test network.
var litecoinTestNetParams = litecoinNetParams{
	Params:   &litecoinCfg.TestNet4Params,
	rpcPort:  "19334",
	CoinType: keychain.CoinTypeTestnet,
}

// litecoinMainNetParams contains the parameters specific to the current
// Litecoin mainnet.
var litecoinMainNetParams = litecoinNetParams{
	Params:   &litecoinCfg.MainNetParams,
	rpcPort:  "9334",
	CoinType: keychain.CoinTypeLitecoin,
}

// litecoinRegTestNetParams contains parameters specific to a local litecoin
// regtest network.
var litecoinRegTestNetParams = litecoinNetParams{
	Params:   &litecoinCfg.RegressionNetParams,
	rpcPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// bitcoinRegTestNetParams contains parameters specific to a local bitcoin
// regtest network.
var bitcoinRegTestNetParams = bitcoinNetParams{
	Params:   &bitcoinCfg.RegressionNetParams,
	rpcPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// applyLitecoinParams applies the relevant chain configuration parameters that
// differ for litecoin to the chain parameters typed for btcsuite derivation.
// This function is used in place of using something like interface{} to
// abstract over _which_ chain (or fork) the parameters are for.
func applyLitecoinParams(params *bitcoinNetParams, litecoinParams *litecoinNetParams) {
	params.Name = litecoinParams.Name
	params.Net = bitcoinWire.BitcoinNet(litecoinParams.Net)
	params.DefaultPort = litecoinParams.DefaultPort
	params.CoinbaseMaturity = litecoinParams.CoinbaseMaturity

	copy(params.GenesisHash[:], litecoinParams.GenesisHash[:])
	copy(params.GenesisBlock.Header.MerkleRoot[:],
		litecoinParams.GenesisBlock.Header.MerkleRoot[:])
	params.GenesisBlock.Header.Version =
		litecoinParams.GenesisBlock.Header.Version
	params.GenesisBlock.Header.Timestamp =
		litecoinParams.GenesisBlock.Header.Timestamp
	params.GenesisBlock.Header.Bits =
		litecoinParams.GenesisBlock.Header.Bits
	params.GenesisBlock.Header.Nonce =
		litecoinParams.GenesisBlock.Header.Nonce
	params.GenesisBlock.Transactions[0].Version =
		litecoinParams.GenesisBlock.Transactions[0].Version
	params.GenesisBlock.Transactions[0].LockTime =
		litecoinParams.GenesisBlock.Transactions[0].LockTime
	params.GenesisBlock.Transactions[0].TxIn[0].Sequence =
		litecoinParams.GenesisBlock.Transactions[0].TxIn[0].Sequence
	params.GenesisBlock.Transactions[0].TxIn[0].PreviousOutPoint.Index =
		litecoinParams.GenesisBlock.Transactions[0].TxIn[0].PreviousOutPoint.Index
	copy(params.GenesisBlock.Transactions[0].TxIn[0].SignatureScript[:],
		litecoinParams.GenesisBlock.Transactions[0].TxIn[0].SignatureScript[:])
	copy(params.GenesisBlock.Transactions[0].TxOut[0].PkScript[:],
		litecoinParams.GenesisBlock.Transactions[0].TxOut[0].PkScript[:])
	params.GenesisBlock.Transactions[0].TxOut[0].Value =
		litecoinParams.GenesisBlock.Transactions[0].TxOut[0].Value
	params.GenesisBlock.Transactions[0].TxIn[0].PreviousOutPoint.Hash =
		chainhash.Hash{}
	params.PowLimitBits = litecoinParams.PowLimitBits
	params.PowLimit = litecoinParams.PowLimit

	dnsSeeds := make([]chaincfg.DNSSeed, len(litecoinParams.DNSSeeds))
	for i := 0; i < len(litecoinParams.DNSSeeds); i++ {
		dnsSeeds[i] = chaincfg.DNSSeed{
			Host:         litecoinParams.DNSSeeds[i].Host,
			HasFiltering: litecoinParams.DNSSeeds[i].HasFiltering,
		}
	}
	params.DNSSeeds = dnsSeeds

	// Address encoding magics
	params.PubKeyHashAddrID = litecoinParams.PubKeyHashAddrID
	params.ScriptHashAddrID = litecoinParams.ScriptHashAddrID
	params.PrivateKeyID = litecoinParams.PrivateKeyID
	params.WitnessPubKeyHashAddrID = litecoinParams.WitnessPubKeyHashAddrID
	params.WitnessScriptHashAddrID = litecoinParams.WitnessScriptHashAddrID
	params.Bech32HRPSegwit = litecoinParams.Bech32HRPSegwit

	copy(params.HDPrivateKeyID[:], litecoinParams.HDPrivateKeyID[:])
	copy(params.HDPublicKeyID[:], litecoinParams.HDPublicKeyID[:])

	params.HDCoinType = litecoinParams.HDCoinType

	checkPoints := make([]chaincfg.Checkpoint, len(litecoinParams.Checkpoints))
	for i := 0; i < len(litecoinParams.Checkpoints); i++ {
		var chainHash chainhash.Hash
		copy(chainHash[:], litecoinParams.Checkpoints[i].Hash[:])

		checkPoints[i] = chaincfg.Checkpoint{
			Height: litecoinParams.Checkpoints[i].Height,
			Hash:   &chainHash,
		}
	}
	params.Checkpoints = checkPoints

	params.rpcPort = litecoinParams.rpcPort
	params.CoinType = litecoinParams.CoinType
}

// isTestnet tests if the given params correspond to a testnet
// parameter configuration.
func isTestnet(params *bitcoinNetParams) bool {
	switch params.Params.Net {
	case bitcoinWire.TestNet3, bitcoinWire.BitcoinNet(litecoinWire.TestNet4):
		return true
	default:
		return false
	}
}
