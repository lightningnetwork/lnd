package chanacceptor

import (
	"errors"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errChannelRejected is returned when the rpc channel acceptor rejects
	// a channel due to acceptor timeout, shutdown, or because no custom
	// error value is available when the channel was rejected.
	errChannelRejected = errors.New("channel rejected")
)

// ChannelAcceptRequest is a struct containing the requesting node's public key
// along with the lnwire.OpenChannel message that they sent when requesting an
// inbound channel. This information is provided to each acceptor so that they
// can each leverage their own decision-making with this information.
type ChannelAcceptRequest struct {
	// Node is the public key of the node requesting to open a channel.
	Node *btcec.PublicKey

	// OpenChanMsg is the actual OpenChannel protocol message that the peer
	// sent to us.
	OpenChanMsg *lnwire.OpenChannel
}

// ChannelAcceptResponse is a struct containing the response to a request to
// open an inbound channel.
type ChannelAcceptResponse struct {
	// ChanAcceptError the error returned by the channel acceptor. If the
	// channel was accepted, this value will be nil.
	ChanAcceptError
}

// NewChannelAcceptResponse is a constructor for a channel accept response,
// which creates a response with an appropriately wrapped error (in the case of
// a rejection) so that the error will be whitelisted and delivered to the
// initiating peer. Accepted channels simply return a response containing a nil
// error.
func NewChannelAcceptResponse(accept bool,
	acceptErr error) *ChannelAcceptResponse {

	// If we want to accept the channel, we return a response with a nil
	// error.
	if accept {
		return &ChannelAcceptResponse{}
	}

	// Use a generic error when no custom error is provided.
	if acceptErr == nil {
		acceptErr = errChannelRejected
	}

	return &ChannelAcceptResponse{
		ChanAcceptError: ChanAcceptError{
			error: acceptErr,
		},
	}
}

// RejectChannel returns a boolean that indicates whether we should reject the
// channel.
func (c *ChannelAcceptResponse) RejectChannel() bool {
	return c.error != nil
}

// ChannelAcceptor is an interface that represents  a predicate on the data
// contained in ChannelAcceptRequest.
type ChannelAcceptor interface {
	Accept(req *ChannelAcceptRequest) *ChannelAcceptResponse
}
