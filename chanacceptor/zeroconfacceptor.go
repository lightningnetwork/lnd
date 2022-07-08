package chanacceptor

import "github.com/lightningnetwork/lnd/lnwire"

// ZeroConfAcceptor wraps a regular ChainedAcceptor. If no acceptors are in the
// ChainedAcceptor, then Accept will reject all channel open requests. This
// should only be enabled when the zero-conf feature bit is set and is used to
// protect users from a malicious counter-party double-spending the zero-conf
// funding tx.
type ZeroConfAcceptor struct {
	chainedAcceptor *ChainedAcceptor
}

// NewZeroConfAcceptor initializes a ZeroConfAcceptor.
func NewZeroConfAcceptor() *ZeroConfAcceptor {
	return &ZeroConfAcceptor{
		chainedAcceptor: NewChainedAcceptor(),
	}
}

// AddAcceptor adds a sub-ChannelAcceptor to the internal ChainedAcceptor.
func (z *ZeroConfAcceptor) AddAcceptor(acceptor ChannelAcceptor) uint64 {
	return z.chainedAcceptor.AddAcceptor(acceptor)
}

// RemoveAcceptor removes a sub-ChannelAcceptor from the internal
// ChainedAcceptor.
func (z *ZeroConfAcceptor) RemoveAcceptor(id uint64) {
	z.chainedAcceptor.RemoveAcceptor(id)
}

// Accept will deny the channel open request if the internal ChainedAcceptor is
// empty. If the internal ChainedAcceptor has any acceptors, then Accept will
// instead be called on it.
//
// NOTE: Part of the ChannelAcceptor interface.
func (z *ZeroConfAcceptor) Accept(
	req *ChannelAcceptRequest) *ChannelAcceptResponse {

	// Alias for less verbosity.
	channelType := req.OpenChanMsg.ChannelType

	// Check if the channel type sets the zero-conf bit.
	var zeroConfSet bool

	if channelType != nil {
		channelFeatures := lnwire.RawFeatureVector(*channelType)
		zeroConfSet = channelFeatures.IsSet(lnwire.ZeroConfRequired)
	}

	// If there are no acceptors and the counter-party is requesting a zero
	// conf channel, reject the attempt.
	if z.chainedAcceptor.numAcceptors() == 0 && zeroConfSet {
		// Deny the channel open request.
		rejectChannel := NewChannelAcceptResponse(
			false, nil, nil, 0, 0, 0, 0, 0, 0, false,
		)
		return rejectChannel
	}

	// Otherwise, the ChainedAcceptor has sub-acceptors, so call Accept on
	// it.
	return z.chainedAcceptor.Accept(req)
}

// A compile-time constraint to ensure ZeroConfAcceptor implements the
// MultiplexAcceptor interface.
var _ MultiplexAcceptor = (*ZeroConfAcceptor)(nil)
