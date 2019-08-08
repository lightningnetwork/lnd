package chanacceptor

// RPCAcceptor represents the RPC-controlled variant of the ChannelAcceptor.
// One RPCAcceptor allows one RPC client.
type RPCAcceptor struct {
	acceptClosure func(req *ChannelAcceptRequest) bool
}

// Accept is a predicate on the ChannelAcceptRequest which is sent to the RPC
// client who will respond with the ultimate decision. This assumes an accept
// closure has been specified during creation.
//
// NOTE: Part of the ChannelAcceptor interface.
func (r *RPCAcceptor) Accept(req *ChannelAcceptRequest) bool {
	return r.acceptClosure(req)
}

// NewRPCAcceptor creates and returns an instance of the RPCAcceptor.
func NewRPCAcceptor(closure func(*ChannelAcceptRequest) bool) *RPCAcceptor {
	return &RPCAcceptor{
		acceptClosure: closure,
	}
}

// A compile-time constraint to ensure RPCAcceptor implements the ChannelAcceptor
// interface.
var _ ChannelAcceptor = (*RPCAcceptor)(nil)
