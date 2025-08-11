//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is the primary configuration struct for the switch RPC subserver.
// It contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
//
//nolint:ll
type Config struct {
	// SwitchMacPath is the path for the switch macaroon. If unspecified
	// then we assume that the macaroon will be found under the network
	// directory, named DefaultSwitchMacFilename.
	SwitchMacPath string `long:"switchmacaroonpath" description:"Path to the switch macaroon"`

	// NetworkDir is the main network directory wherein the switch rpc
	// server will find the macaroon named DefaultSwitchMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the Switch rpc server.
	MacService *macaroons.Service
}
