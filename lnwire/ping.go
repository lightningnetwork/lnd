package lnwire

import "io"

// Pong defines a message which is the direct response to a received Ping
// message. A Pong reply indicates that a connection is still active. The Pong
// reply to a Ping message should contain the nonce carried in the original
// Pong message.
type Pong struct {
	// Nonce is the unique nonce that was associated with the Ping message
	// that this Pong is replying to.
	Nonce uint64
}

// NewPong returns a new Pong message binded to the specified nonce.
func NewPong(nonce uint64) *Pong {
	return &Pong{
		Nonce: nonce,
	}
}

// A compile time check to ensure Pong implements the lnwire.Message interface.
var _ Message = (*Pong)(nil)

// Decode deserializes a serialized Pong message stored in the passed io.Reader
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p *Pong) Decode(r io.Reader, pver uint32) error {
	err := readElements(r,
		&p.Nonce,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target Pong into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the lnwire.Message interface.
func (p *Pong) Encode(w io.Writer, pver uint32) error {
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
func (p *Pong) Command() uint32 {
	return CmdPong
}

// MaxPayloadLength returns the maximum allowed payload size for a Pong
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (p *Pong) MaxPayloadLength(uint32) uint32 {
	return 8
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the Pong are valid.
//
// This is part of the lnwire.Message interface.
func (p *Pong) Validate() error {
	return nil
}
