// +build walletrpc

package walletrpc

import (
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is the primary configuration struct for the WalletKit RPC server. It
// contains all the items required for the signer rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// WalletKitMacPath is the path for the signer macaroon. If unspecified
	// then we assume that the macaroon will be found under the network
	// directory, named DefaultWalletKitMacFilename.
	WalletKitMacPath string `long:"walletkitmacaroonpath" description:"Path to the wallet kit macaroon"`

	// NetworkDir is the main network directory wherein the signer rpc
	// server will find the macaroon named DefaultWalletKitMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the signer rpc server.
	MacService *macaroons.Service

	// FeeEstimator is an instance of the primary fee estimator instance
	// the WalletKit will use to respond to fee estimation requests.
	FeeEstimator lnwallet.FeeEstimator

	// Wallet is the primary wallet that the WalletKit will use to proxy
	// any relevant requests to.
	Wallet lnwallet.WalletController

	// KeyRing is an interface that the WalletKit will use to derive any
	// keys due to incoming client requests.
	KeyRing keychain.KeyRing
}
