package lnwire

import (
	"fmt"
	"io"
)

// Ping defines a message which is sent by peers periodically to determine if
// the connection is still valid. Each ping message should carry a unique nonce
// which is to be echoed back within the Pong response.
type Ping struct {
	// Nonce is a unique value associated with this ping message. The pong
	// message that responds to this ping should reference the same value.
	Nonce uint64
}

// NewPing returns a new Ping message binded to the specified nonce.
func NewPing(nonce uint64) *Ping {
	return &Ping{
		Nonce: nonce,
	}
}

// A compile time check to ensure Ping implements the lnwire.Message interface.
var _ Message = (*Ping)(nil)

// Decode deserializes a serialized Ping message stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p *Ping) Decode(r io.Reader, pver uint32) error {
	err := readElements(r,
		&p.Nonce,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target Ping into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the lnwire.Message interface.
func (p *Ping) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		p.Nonce,
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
func (p *Ping) Command() uint32 {
	return CmdPing
}

// MaxPayloadLength returns the maximum allowed payload size for a Ping
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p Ping) MaxPayloadLength(uint32) uint32 {
	return 8
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the Ping are valid.
//
// This is part of the lnwire.Message interface.
func (p *Ping) Validate() error {
	return nil
}

// String returns the string representation of the target Ping.
//
// This is part of the lnwire.Message interface.
func (p *Ping) String() string {
	return fmt.Sprintf("Ping(%v)", p.Nonce)
}
