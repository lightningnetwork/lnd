package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/wire"
)

// CancelHTLC is sent by Alice to Bob in order to remove a previously added
// HTLC. Upon receipt of an CancelHTLC the HTLC should be removed from the next
// commitment transaction, with the CancelHTLC propgated backwards in the route
// to fully un-clear the HTLC.
type CancelHTLC struct {
	// ChannelPoint is the particular active channel that this CancelHTLC
	// is binded to.
	ChannelPoint *wire.OutPoint

	// HTLCKey references which HTLC on the remote node's commitment
	// transaction has timed out.
	HTLCKey HTLCKey
}

// Decode deserializes a serialized CancelHTLC message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint(8)
	// HTLCKey(8)
	err := readElements(r,
		&c.ChannelPoint,
		&c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

// CancelHTLC creates a new CancelHTLC message.
func NewHTLCTimeoutRequest() *CancelHTLC {
	return &CancelHTLC{}
}

// A compile time check to ensure CancelHTLC implements the lnwire.Message
// interface.
var _ Message = (*CancelHTLC)(nil)

// Encode serializes the target CancelHTLC into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
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
func (c *CancelHTLC) Command() uint32 {
	return CmdCancelHTLC
}

// MaxPayloadLength returns the maximum allowed payload size for a CancelHTLC
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) MaxPayloadLength(uint32) uint32 {
	// 36 + 8
	return 44
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the CancelHTLC are valid.
//
// This is part of the lnwire.Message interface.
func (c *CancelHTLC) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target CancelHTLC.  This is
// part of the lnwire.Message interface.
func (c *CancelHTLC) String() string {
	return fmt.Sprintf("\n--- Begin CancelHTLC ---\n") +
		fmt.Sprintf("ChannelPoint:\t%d\n", c.ChannelPoint) +
		fmt.Sprintf("HTLCKey:\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End CancelHTLC ---\n")
}
