// +build signerrpc

package signrpc

import (
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is the primary configuration struct for the signer RPC server. It
// contains all the items required for the signer rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// SignerMacPath is the path for the signer macaroon. If unspecified
	// then we assume that the macaroon will be found under the network
	// directory, named DefaultSignerMacFilename.
	SignerMacPath string `long:"signermacaroonpath" description:"Path to the signer macaroon"`

	// NetworkDir is the main network directory wherein the signer rpc
	// server will find the macaroon named DefaultSignerMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the signer rpc server.
	MacService *macaroons.Service

	// Signer is the signer instance that backs the signer RPC server. The
	// job of the signer RPC server is simply to proxy valid requests to
	// the active signer instance.
	Signer lnwallet.Signer
}
