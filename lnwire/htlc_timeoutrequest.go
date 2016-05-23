package lnwire

import (
	"fmt"
	"io"
)

// HTLCTimeoutRequest is sent by Alice to Bob in order to timeout a previously
// added HTLC. Upon receipt of an HTLCTimeoutRequest the HTLC should be removed
// from the next commitment transaction, with the HTLCTimeoutRequest propgated
// backwards in the route to fully clear the HTLC.
type HTLCTimeoutRequest struct {
	// ChannelID is the particular active channel that this HTLCTimeoutRequest
	// is binded to.
	ChannelID uint64

	// HTLCKey references which HTLC on the remote node's commitment
	// transaction has timed out.
	HTLCKey HTLCKey
}

// Decode deserializes a serialized HTLCTimeoutRequest message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCTimeoutRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelID(8)
	// HTLCKey(8)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

// NewHTLCTimeoutRequest creates a new HTLCTimeoutRequest message.
func NewHTLCTimeoutRequest() *HTLCTimeoutRequest {
	return &HTLCTimeoutRequest{}
}

// A compile time check to ensure HTLCTimeoutRequest implements the lnwire.Message
// interface.
var _ Message = (*HTLCTimeoutRequest)(nil)

// Encode serializes the target HTLCTimeoutRequest into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *HTLCTimeoutRequest) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelID,
		c.HTLCKey,
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
func (c *HTLCTimeoutRequest) Command() uint32 {
	return CmdHTLCTimeoutRequest
}

// MaxPayloadLength returns the maximum allowed payload size for a HTLCTimeoutRequest
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCTimeoutRequest) MaxPayloadLength(uint32) uint32 {
	// 16
	return 16
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the HTLCTimeoutRequest are valid.
//
// This is part of the lnwire.Message interface.
func (c *HTLCTimeoutRequest) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target HTLCTimeoutRequest.  //
// This is part of the lnwire.Message interface.
func (c *HTLCTimeoutRequest) String() string {
	return fmt.Sprintf("\n--- Begin HTLCTimeoutRequest ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End HTLCTimeoutRequest ---\n")
}
