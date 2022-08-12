package chanacceptor

import (
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
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
// open an inbound channel. Note that fields added to this struct must be added
// to the mergeResponse function to allow combining of responses from different
// acceptors.
type ChannelAcceptResponse struct {
	// ChanAcceptError the error returned by the channel acceptor. If the
	// channel was accepted, this value will be nil.
	ChanAcceptError

	// UpfrontShutdown is the address that we will set as our upfront
	// shutdown address.
	UpfrontShutdown lnwire.DeliveryAddress

	// CSVDelay is the csv delay we require for the remote peer.
	CSVDelay uint16

	// Reserve is the amount that require the remote peer hold in reserve
	// on the channel.
	Reserve btcutil.Amount

	// InFlightTotal is the maximum amount that we allow the remote peer to
	// hold in outstanding htlcs.
	InFlightTotal lnwire.MilliSatoshi

	// HtlcLimit is the maximum number of htlcs that we allow the remote
	// peer to offer us.
	HtlcLimit uint16

	// MinHtlcIn is the minimum incoming htlc value allowed on the channel.
	MinHtlcIn lnwire.MilliSatoshi

	// MinAcceptDepth is the minimum depth that the initiator of the
	// channel should wait before considering the channel open.
	MinAcceptDepth uint16

	// ZeroConf indicates that the fundee wishes to send min_depth = 0 and
	// request a zero-conf channel with the counter-party.
	ZeroConf bool
}

// NewChannelAcceptResponse is a constructor for a channel accept response,
// which creates a response with an appropriately wrapped error (in the case of
// a rejection) so that the error will be whitelisted and delivered to the
// initiating peer. Accepted channels simply return a response containing a nil
// error.
func NewChannelAcceptResponse(accept bool, acceptErr error,
	upfrontShutdown lnwire.DeliveryAddress, csvDelay, htlcLimit,
	minDepth uint16, reserve btcutil.Amount, inFlight,
	minHtlcIn lnwire.MilliSatoshi, zeroConf bool) *ChannelAcceptResponse {

	resp := &ChannelAcceptResponse{
		UpfrontShutdown: upfrontShutdown,
		CSVDelay:        csvDelay,
		Reserve:         reserve,
		InFlightTotal:   inFlight,
		HtlcLimit:       htlcLimit,
		MinHtlcIn:       minHtlcIn,
		MinAcceptDepth:  minDepth,
		ZeroConf:        zeroConf,
	}

	// If we want to accept the channel, we return a response with a nil
	// error.
	if accept {
		return resp
	}

	// Use a generic error when no custom error is provided.
	if acceptErr == nil {
		acceptErr = errChannelRejected
	}

	resp.ChanAcceptError = ChanAcceptError{
		error: acceptErr,
	}

	return resp
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

// MultiplexAcceptor is an interface that abstracts the ability of a
// ChannelAcceptor to contain sub-ChannelAcceptors.
type MultiplexAcceptor interface {
	// Embed the ChannelAcceptor.
	ChannelAcceptor

	// AddAcceptor nests a ChannelAcceptor inside the MultiplexAcceptor.
	AddAcceptor(acceptor ChannelAcceptor) uint64

	// Remove a sub-ChannelAcceptor.
	RemoveAcceptor(id uint64)
}
