package lnwire

import (
	"fmt"
	"io"
)

// AllowHTLCRequest is sent to Bob from Alice. Alice wants to know if Bob is able to
// make htlc chain with some other node with a given amount.
// This message is sent to all nodes in possible HTLC chain to ensure that they can
// accept it.
// It is used to validate/filter possible paths in routing.
type AllowHTLCRequestMessage struct {
	// Id of node to which payment may be requested in future
	// It is string because it is internally converted to graph.ID in routing module.
	PartnerID string
	// Number uniquely identifying this request
	// It is unique for a given sender. Messages from different senders
	// may have the same RequestID
	RequestID uint64
	// Requested amount
	Amount CreditsAmount
}

// Decode deserializes a serialized AllowHTLCRequest message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCRequestMessage) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint (8)
	// HTLCKey   (8)
	err := readElements(r,
		&c.PartnerID,
		&c.RequestID,
		&c.Amount,
	)
	if err != nil {
		return err
	}

	return nil
}

// AllowHTLCRequest returns a new empty AllowHTLCRequest message.
func NewAllowHTLCRequest() *AllowHTLCRequestMessage {
	return &AllowHTLCRequestMessage{}
}

// Encode serializes the target AllowHTLCRequest into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCRequestMessage) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.PartnerID,
		c.RequestID,
		c.Amount,
	)

	if err != nil {
		return err
	}

	return nil
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCRequestMessage) Command() uint32 {
	return CmdAllowHTLCRequest
}

// MaxPayloadLength returns the maximum allowed payload size for a AllowHTLCRequest
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCRequestMessage) MaxPayloadLength(uint32) uint32 {
	// 33 + 8 + 8
	// 33 - because 32 is length of the string SHA256 and one byte for the length
	return 49
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the AllowHTLCRequest are valid.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCRequestMessage) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target AllowHTLCRequest.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCRequestMessage) String() string {
	return fmt.Sprintf("\n--- Begin AllowHTLCRequest ---\n") +
		fmt.Sprintf("PartnerID:\t\t%v\n", c.PartnerID) +
		fmt.Sprintf("RequestID:\t\t%d\n", c.RequestID) +
		fmt.Sprintf("Amount:\t\t%d\n", c.Amount) +
		fmt.Sprintf("--- End AllowHTLCRequest ---\n")
}

// A compile time check to ensure AllowHTLCRequest implements the lnwire.Message
// interface.
var _ Message = (*AllowHTLCRequestMessage)(nil)