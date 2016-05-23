package lnwire

import (
	"fmt"
	"io"
)

// HTLCAddReject is sent by Bob when he wishes to reject a particular HTLC that
// Alice attempted to add via an HTLCAddRequest message. The rejected HTLC is
// referenced by its unique HTLCKey ID. An HTLCAddReject message is bound to a
// single active channel, referenced by a unique ChannelID. Additionally, the
// HTLCKey of the rejected HTLC is present
type HTLCAddReject struct {
	// ChannelID references the particular active channel to which this
	// HTLCAddReject message is binded to.
	ChannelID uint64

	// HTLCKey is used to identify which HTLC previously attempted to be
	// added via an HTLCAddRequest message is being declined.
	HTLCKey HTLCKey
}

// Decode deserializes a serialized HTLCAddReject message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddReject) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// HTLCKey   (8)
	err := readElements(r,
		&c.ChannelID,
		&c.HTLCKey,
	)
	if err != nil {
		return err
	}

	return nil
}

// NewHTLCAddReject returns a new empty HTLCAddReject message.
func NewHTLCAddReject() *HTLCAddReject {
	return &HTLCAddReject{}
}

// A compile time check to ensure HTLCAddReject implements the lnwire.Message
// interface.
var _ Message = (*HTLCAddReject)(nil)

// Encode serializes the target HTLCAddReject into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddReject) Encode(w io.Writer, pver uint32) error {
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
func (c *HTLCAddReject) Command() uint32 {
	return CmdHTLCAddReject
}

// MaxPayloadLength returns the maximum allowed payload size for a HTLCAddReject
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddReject) MaxPayloadLength(uint32) uint32 {
	// 8 + 8
	return 16
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the HTLCAddReject are valid.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddReject) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target HTLCAddReject.
//
// This is part of the lnwire.Message interface.
func (c *HTLCAddReject) String() string {
	return fmt.Sprintf("\n--- Begin HTLCAddReject ---\n") +
		fmt.Sprintf("ChannelID:\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("HTLCKey:\t\t%d\n", c.HTLCKey) +
		fmt.Sprintf("--- End HTLCAddReject ---\n")
}
