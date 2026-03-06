package remotesignerrpc

import (
	"fmt"

	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// FullMethodSignCoordinatorStreams is the full gRPC method path for the
	// remote signer stream RPC.
	FullMethodSignCoordinatorStreams = "/remotesignerrpc.RemoteSigner/" +
		"SignCoordinatorStreams"
)

// SignCoordinatorStreamsPermissions are the macaroon permissions required to
// access the remote signer coordination stream.
var SignCoordinatorStreamsPermissions = []bakery.Op{{
	Entity: "remotesigner",
	Action: "generate",
}}

// InboundConnection is the minimal interface the dedicated remote signer RPC
// server needs to accept and manage an inbound sign coordinator stream.
type InboundConnection interface {
	AddConnection(stream RemoteSigner_SignCoordinatorStreamsServer) error
}

// InboundServer is a minimal gRPC server implementation that exposes only the
// SignCoordinatorStreams RPC.
type InboundServer struct {
	UnimplementedRemoteSignerServer

	Conn InboundConnection
}

// SignCoordinatorStreams accepts an inbound remote signer stream and hands it
// over to the configured coordinator/connection.
func (s *InboundServer) SignCoordinatorStreams(
	stream RemoteSigner_SignCoordinatorStreamsServer) error {

	if s.Conn == nil {
		return fmt.Errorf("inbound connections from remote signers not enabled")
	}

	return s.Conn.AddConnection(stream)
}
