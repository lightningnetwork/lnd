//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"context"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
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
	HtlcDispatcher HtlcDispatcher

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

	// RemoteRouterController provides the ability to manage the remote router
	// state.
	RemoteRouterController RemoteRouterController
}

// HtlcDispatcher is the subset of the switch used to dispatch HTLC attempts
// and wait for their results. It is kept separate from the router's
// PaymentAttemptDispatcher so that RPC callers, which may abandon a wait, can
// use the context-aware result lookup without changing the router.
type HtlcDispatcher interface {
	// SendHTLC forwards a fully encoded HTLC to the given first hop.
	SendHTLC(firstHop lnwire.ShortChannelID, attemptID uint64,
		htlcAdd *lnwire.UpdateAddHTLC) error

	// GetAttemptResultWithContext returns a channel that delivers the
	// result of the given attempt. The wait is abandoned, and the channel
	// closed, when ctx is cancelled or the switch shuts down. A closed
	// channel is not a result: the HTLC may still be in flight.
	GetAttemptResultWithContext(ctx context.Context, attemptID uint64,
		paymentHash lntypes.Hash,
		deobfuscator htlcswitch.ErrorDecrypter) (
		<-chan *htlcswitch.PaymentResult, error)
}

// The switch is assigned to Config.HtlcDispatcher by reflection, so check at
// compile time that it implements the interface.
var _ HtlcDispatcher = (*htlcswitch.Switch)(nil)

// RemoteRouterController defines a restricted interface for managing the remote
// router state.
type RemoteRouterController interface {
	// DisableRemoteRouter disables the remote router, allowing a migration
	// back to the embedded router.
	DisableRemoteRouter() error
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
