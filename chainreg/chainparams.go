package chainreg

import (
	bitcoinCfg "github.com/btcsuite/btcd/chaincfg"
	bitcoinWire "github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
)

// BitcoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type BitcoinNetParams struct {
	*bitcoinCfg.Params
	RPCPort  string
	CoinType uint32
}

// BitcoinTestNetParams contains parameters specific to the 3rd version of the
// test network.
var BitcoinTestNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.TestNet3Params,
	RPCPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// BitcoinTestNet4Params contains parameters specific to the 4th version of the
// test network.
var BitcoinTestNet4Params = BitcoinNetParams{
	Params:   &bitcoinCfg.TestNet4Params,
	RPCPort:  "48334",
	CoinType: keychain.CoinTypeTestnet,
}

// BitcoinMainNetParams contains parameters specific to the current Bitcoin
// mainnet.
var BitcoinMainNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.MainNetParams,
	RPCPort:  "8334",
	CoinType: keychain.CoinTypeBitcoin,
}

// BitcoinSimNetParams contains parameters specific to the simulation test
// network.
var BitcoinSimNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.SimNetParams,
	RPCPort:  "18556",
	CoinType: keychain.CoinTypeTestnet,
}

// BitcoinSigNetParams contains parameters specific to the signet test network.
var BitcoinSigNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.SigNetParams,
	RPCPort:  "38332",
	CoinType: keychain.CoinTypeTestnet,
}

// BitcoinRegTestNetParams contains parameters specific to a local bitcoin
// regtest network.
var BitcoinRegTestNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.RegressionNetParams,
	RPCPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// IsTestnet tests if the given params correspond to a testnet parameter
// configuration.
func IsTestnet(params *BitcoinNetParams) bool {
	return params.Params.Net == bitcoinWire.TestNet3 ||
		params.Params.Net == bitcoinWire.TestNet4
}
