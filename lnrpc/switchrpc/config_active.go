//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/routing"
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

	// HtlcDispatcher provides the means by which payment attempts can
	// be dispatched.
	HtlcDispatcher routing.PaymentAttemptDispatcher

	// ChannelInfoAccessor defines an interface for accessing channel
	// information necessary for dispatching payment attempts, specifically
	// methods for fetching links by public key.
	ChannelInfoAccessor ChannelInfoAccessor
}

// ChannelInfoAccessor defines a restricted, read-only view into the switch's
// active channel links. The information provided can be of use when dispatching
// payments.
type ChannelInfoAccessor interface {
	// GetLinksByPubkey provides a read-only view of all active channel
	// links associated with a given node public key.
	GetLinksByPubkey(pubKey [33]byte) ([]htlcswitch.ChannelInfoProvider,
		error)
}
