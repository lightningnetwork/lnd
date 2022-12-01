//go:build chainrpc
// +build chainrpc

package chainrpc

import (
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is the primary configuration struct for the chain notifier RPC server.
// It contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	// ChainNotifierMacPath is the path for the chain notifier macaroon. If
	// unspecified then we assume that the macaroon will be found under the
	// network directory, named DefaultChainNotifierMacFilename.
	ChainNotifierMacPath string `long:"notifiermacaroonpath" description:"Path to the chain notifier macaroon"`

	// NetworkDir is the main network directory wherein the chain notifier
	// RPC server will find the macaroon named
	// DefaultChainNotifierMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the chain notifier RPC server.
	MacService *macaroons.Service

	// ChainNotifier is the chain notifier instance that backs the chain
	// notifier RPC server. The job of the chain notifier RPC server is
	// simply to proxy valid requests to the active chain notifier instance.
	ChainNotifier chainntnfs.ChainNotifier

	// Chain provides access to the most up-to-date blockchain data.
	Chain lnwallet.BlockChainIO
}
