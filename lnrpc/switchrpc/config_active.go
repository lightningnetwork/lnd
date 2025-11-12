//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
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

	// AttemptStore provides the means by which the RPC server can manage
	// the state of an HTLC attempt, including initializing and failing it.
	AttemptStore htlcswitch.AttemptStore

	// ChannelInfoAccessor defines an interface for accessing channel
	// information necessary for dispatching payment attempts, specifically
	// methods for fetching links by public key.
	ChannelInfoAccessor ChannelInfoAccessor

	// RouteProcessor provides the capability to convert from external (rpc)
	// to internal representation of a payment route.
	RouteProcessor RouteProcessor
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

// RouteProcessor provides the ability to convert a route from the external rpc
// representation to our internal format for use when constructing onions.
type RouteProcessor interface {
	// UnmarshallRoute unmarshalls an rpc route. For hops that don't specify
	// a pubkey, the channel graph is queried.
	UnmarshallRoute(rpcroute *lnrpc.Route) (*route.Route, error)
}
