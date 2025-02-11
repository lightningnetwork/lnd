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
type Config struct {
	// Switch is the main htlc switch instance that backs this RPC server.
	Switch *htlcswitch.Switch

	// RouteProcessor provides the capability to convert from external (rpc)
	// to internal representation of a payment route.
	RouteProcessor RouteProcessor

	// SwitchMacPath is the path for the router macaroon. If unspecified
	// then we assume that the macaroon will be found under the network
	// directory, named DefaultRouterMacFilename.
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

type RouteProcessor interface {
	// UnmarshallRoute unmarshalls an rpc route. For hops that don't specify
	// a pubkey, the channel graph is queried.
	UnmarshallRoute(rpcroute *lnrpc.Route) (*route.Route, error)
}
