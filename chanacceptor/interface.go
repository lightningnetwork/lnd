package chanacceptor

import (
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errChannelRejected is returned when the rpc channel acceptor rejects
	// a channel due to acceptor timeout, shutdown, or because no custom
	// error value is available when the channel was rejected.
	errChannelRejected = errors.New("channel rejected")
)

// ChannelAcceptRequest is a struct containing the requesting node's public key
// along with the lnwire.OpenChannel or lnwire.OpenChannel2 message that they
// sent when requesting an inbound channel. This information is provided to each
// acceptor so that they can each leverage their own decision-making with this
// information.
type ChannelAcceptRequest struct {
	// Node is the public key of the node requesting to open a channel.
	Node *btcec.PublicKey

	// OpenChanMsg is the actual OpenChannel protocol message that the peer
	// sent to us. Must be nil if OpenChan2Msg is set.
	OpenChanMsg *lnwire.OpenChannel

	// OpenChan2Msg is the actual OpenChannel2 protocol message that the
	// peer sent to us. Must be nil if OpenChanMsg is set.
	OpenChan2Msg *lnwire.OpenChannel2
}

func (c *ChannelAcceptRequest) PendingChannelID() [32]byte {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.PendingChannelID
	}

	return c.OpenChan2Msg.PendingChannelID
}

func (c *ChannelAcceptRequest) ChannelType() *lnwire.ChannelType {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.ChannelType
	}

	return c.OpenChan2Msg.ChannelType
}

func (c *ChannelAcceptRequest) ChainHash() chainhash.Hash {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.ChainHash
	}

	return c.OpenChan2Msg.ChainHash
}

func (c *ChannelAcceptRequest) FundingAmount() btcutil.Amount {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.FundingAmount
	}

	return c.OpenChan2Msg.FundingAmount
}

func (c *ChannelAcceptRequest) PushAmount() lnwire.MilliSatoshi {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.PushAmount
	}

	return 0
}

func (c *ChannelAcceptRequest) DustLimit() btcutil.Amount {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.DustLimit
	}

	return c.OpenChan2Msg.DustLimit
}

func (c *ChannelAcceptRequest) MaxValueInFlight() lnwire.MilliSatoshi {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.MaxValueInFlight
	}

	return c.OpenChan2Msg.MaxValueInFlight
}

func (c *ChannelAcceptRequest) ChannelReserve() btcutil.Amount {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.ChannelReserve
	}

	// The dual funding protocol fixes the channel reserve at 1% of the
	// total funding amount contributed by both parties. We don't have the
	// local contribution amount yet, so we return the channel reserve
	// required for the remote amount only.
	var chanReserveDivisor btcutil.Amount = 100

	return c.OpenChan2Msg.FundingAmount / chanReserveDivisor
}

func (c *ChannelAcceptRequest) HtlcMinimum() lnwire.MilliSatoshi {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.HtlcMinimum
	}

	return c.OpenChan2Msg.HtlcMinimum
}

func (c *ChannelAcceptRequest) FeePerKiloWeight() uint32 {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.FeePerKiloWeight
	}

	// OpenChannel2 has a separate fee rate for the funding and commitment
	// transactions. The commitment fee rate is analogous to OpenChannel's
	// only fee rate, so we return it here.
	//
	// TODO(morehouse): Add the funding fee rate to the RPC API.
	return c.OpenChan2Msg.CommitFeePerKWeight
}

func (c *ChannelAcceptRequest) CsvDelay() uint16 {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.CsvDelay
	}

	return c.OpenChan2Msg.CsvDelay
}

func (c *ChannelAcceptRequest) MaxAcceptedHTLCs() uint16 {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.MaxAcceptedHTLCs
	}

	return c.OpenChan2Msg.MaxAcceptedHTLCs
}

func (c *ChannelAcceptRequest) ChannelFlags() lnwire.FundingFlag {
	if c.OpenChanMsg != nil {
		return c.OpenChanMsg.ChannelFlags
	}

	return c.OpenChan2Msg.ChannelFlags
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
