package watchonlyrpc

import (
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// FullMethodSignCoordinatorStreams is the full gRPC method path for the
	// remote signer stream RPC.
	FullMethodSignCoordinatorStreams = "/watchonlyrpc.WatchOnly/" +
		"SignCoordinatorStreams"
)

// SignCoordinatorStreamsPermissions are the macaroon permissions required to
// access the remote signer coordination stream.
var SignCoordinatorStreamsPermissions = []bakery.Op{{
	Entity: "remotesigner",
	Action: "generate",
}}

// InboundServer is a minimal gRPC server implementation that exposes only the
// SignCoordinatorStreams RPC.
type InboundServer struct {
	UnimplementedWatchOnlyServer
}

// SignCoordinatorStreams accepts an inbound remote signer stream.
func (s *InboundServer) SignCoordinatorStreams(
	stream WatchOnly_SignCoordinatorStreamsServer) error {

	return nil
}
