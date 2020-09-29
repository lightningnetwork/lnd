package chainreg

import (
	"time"

	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lncfg"
)

// Config houses necessary fields that a chainControl instance needs to
// function.
type Config struct {
	// Bitcoin defines settings for the Bitcoin chain.
	Bitcoin *lncfg.Chain

	// Litecoin defines settings for the Litecoin chain.
	Litecoin *lncfg.Chain

	// PrimaryChain is a function that returns our primary chain via its
	// ChainCode.
	PrimaryChain func() ChainCode

	// HeightHintCacheQueryDisable is a boolean that disables height hint
	// queries if true.
	HeightHintCacheQueryDisable bool

	// NeutrinoMode defines settings for connecting to a neutrino light-client.
	NeutrinoMode *lncfg.Neutrino

	// BitcoindMode defines settings for connecting to a bitcoind node.
	BitcoindMode *lncfg.Bitcoind

	// LitecoindMode defines settings for connecting to a litecoind node.
	LitecoindMode *lncfg.Bitcoind

	// BtcdMode defines settings for connecting to a btcd node.
	BtcdMode *lncfg.Btcd

	// LtcdMode defines settings for connecting to an ltcd node.
	LtcdMode *lncfg.Btcd

	// LocalChanDB is a pointer to the local backing channel database.
	LocalChanDB *channeldb.DB

	// RemoteChanDB is a pointer to the remote backing channel database.
	RemoteChanDB *channeldb.DB

	// PrivateWalletPw is the private wallet password to the underlying
	// btcwallet instance.
	PrivateWalletPw []byte

	// PublicWalletPw is the public wallet password to the underlying btcwallet
	// instance.
	PublicWalletPw []byte

	// Birthday specifies the time the wallet was initially created.
	Birthday time.Time

	// RecoveryWindow specifies the address look-ahead for which to scan when
	// restoring a wallet.
	RecoveryWindow uint32

	// Wallet is a pointer to the backing wallet instance.
	Wallet *wallet.Wallet

	// NeutrinoCS is a pointer to a neutrino ChainService. Must be non-nil if
	// using neutrino.
	NeutrinoCS *neutrino.ChainService

	// ActiveNetParams details the current chain we are on.
	ActiveNetParams BitcoinNetParams

	// FeeURL defines the URL for fee estimation we will use. This field is
	// optional.
	FeeURL string
}
