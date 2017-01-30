package htlcswitch

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/wire"
)

// RequestType represents the type of HTLC switch request type, and it is needed
// for compilation error in case if wrong type was specified.
type RequestType uint8

const (
	// ForwardAddRequest encapsulates the HTLC add request which should be
	// propagated to another channel over HTLC switch.
	ForwardAddRequest RequestType = iota

	// UserAddRequest encapsulates the HTLC add request which made by user
	// and the state of which should be propagated back to user after HTLC
	// will be settled.
	UserAddRequest

	// ForwardSettleRequest encapsulates the settle HTLC request which
	// made by last hope and propagated back to the original hope which sent
	// the HTLC add request.
	ForwardSettleRequest

	// CancelRequest encapsulates the cancel HTLC request which propagated
	// back to the original hope which sent the HTLC add request.
	CancelRequest
)

// SwitchRequest encapsulate
type SwitchRequest struct {
	// Type is a type of switch request which is used to determine the
	// necessary behaviour. For example: If HTLC was settled - should we
	// send the notification over error channel or propagate it back
	// over HTLC switch?
	Type RequestType

	// PayHash payment hash of htlc request.
	PayHash [32]byte

	// Dest is the next peer in HTLC path.
	Dest *routing.HopID

	// ChannelPoint channel point from which HTLC message came from.
	ChannelPoint *wire.OutPoint

	// Htlc lnwire HTLC message type of which depends on switch request
	// type.
	Htlc lnwire.Message

	// err is an error channel which is used to notify user about
	// status of payment request is it was canceled or successfully
	// settled.
	err chan error
}

// Error function is used to return request channel error, which make sense
// only on user request.
func (r *SwitchRequest) Error() chan error {
	if r.Type == UserAddRequest {
		return r.err
	} else {
		panic("only user request has error channel")
	}
}

// NewUserAddRequest creates new switch request with UserAddRequest type, for
// more information look at UserAddRequest comments.
func NewUserAddRequest(dest *routing.HopID,
	htlc *lnwire.HTLCAddRequest) *SwitchRequest {
	return &SwitchRequest{
		Type: UserAddRequest,
		Dest: dest,
		Htlc: lnwire.Message(htlc),
		err:  make(chan error),
	}
}

// NewForwardAddRequest creates new switch request with ForwardAddRequest type,
// for more information look at ForwardAddRequest type comments.
// NOTE: the name "source" is considered in terms of htlc switch circuit.
func NewForwardAddRequest(dest *routing.HopID, source *wire.OutPoint,
	htlc *lnwire.HTLCAddRequest) *SwitchRequest {
	return &SwitchRequest{
		Type:         ForwardAddRequest,
		Dest:         dest,
		ChannelPoint: source,
		Htlc:         lnwire.Message(htlc),
	}
}

// NewForwardSettleRequest creates new switch request with ForwardSettleRequest
// type, for more information look at ForwardSettleRequest type comments.
// NOTE: the name "source" is considered from htlc switch POV.
func NewForwardSettleRequest(destination *wire.OutPoint,
	htlc *lnwire.HTLCSettleRequest) *SwitchRequest {
	return &SwitchRequest{
		Type:         ForwardSettleRequest,
		ChannelPoint: destination,
		Htlc:         lnwire.Message(htlc),
	}
}

// NewCancelRequest creates new switch request with CancelRequest type, for more
// information look at CancelRequest type comments.
// NOTE: the name "destination" is considered from htlc switch POV.
func NewCancelRequest(destination *wire.OutPoint, htlc *lnwire.CancelHTLC,
	payHash [32]byte) *SwitchRequest {
	return &SwitchRequest{
		Type:         CancelRequest,
		ChannelPoint: destination,
		PayHash:      payHash,
		Htlc:         htlc,
	}
}
