package lnwire

import (
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"

	"io"
)

// CloseRequest is sent by either side in order to initiate the cooperative
// closure of a channel. This message is rather sparse as both side implicitly
// know to craft a transaction sending the settled funds of both parties to the
// final delivery addresses negotiated during the funding workflow.
//
// NOTE: The requester is able to only send a signature to initiate the
// cooperative channel closure as all transactions are assembled observing
// BIP 69 which defines a cannonical ordering for input/outputs. Therefore,
// both sides are able to arrive at an identical closure transaction as they
// know the order of the inputs/outputs.
type CloseRequest struct {
	// ChanID serves to identify which channel is to be closed.
	ChanID ChannelID

	// RequesterCloseSig is the signature of the requester for the fully
	// assembled closing transaction.
	RequesterCloseSig *btcec.Signature

	// Fee is the required fee-per-KB the closing transaction must have.
	// It is recommended that a "sufficient" fee be paid in order to
	// achieve timely channel closure.
	// TODO(roasbeef): if initiator always pays fees, then no longer needed.
	Fee btcutil.Amount
}

// NewCloseRequest creates a new CloseRequest.
func NewCloseRequest(cid ChannelID, sig *btcec.Signature) *CloseRequest {
	// TODO(roasbeef): update once fees aren't hardcoded
	return &CloseRequest{
		ChanID:            cid,
		RequesterCloseSig: sig,
	}
}

// A compile time check to ensure CloseRequest implements the lnwire.Message
// interface.
var _ Message = (*CloseRequest)(nil)

// Decode deserializes a serialized CloseRequest stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CloseRequest) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.ChanID,
		&c.RequesterCloseSig,
		&c.Fee)
}

// Encode serializes the target CloseRequest into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CloseRequest) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.ChanID,
		c.RequesterCloseSig,
		c.Fee)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *CloseRequest) MsgType() MessageType {
	return MsgCloseRequest
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CloseRequest) MaxPayloadLength(pver uint32) uint32 {
	// 36 + 73 + 8
	return 117
}
