package chanacceptor

// ChanAcceptError is an error that it returned when an external channel
// acceptor rejects a channel. Note that this error type is whitelisted and will
// be delivered to the peer initiating a channel.
type ChanAcceptError struct {
	error
}
