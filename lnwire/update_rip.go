package lnwire

import "io"

// RIPUpdate is the message that peers will send to its neighbours
// to update the route table when lnd configed the RIP router
type RIPUpdate struct {
	// Destination is the destination node of the HTLC path
	Destination [33]byte

	// NextHop is the next node of a HTLC path
	NextHop		[33]byte

	// LinkChan is the channel used for HTLC
	LinkChan	ChannelID

	// Distance means how many hops the path is
	Distance	int8
}

var _ Message = (*RIPUpdate)(nil)

// Decode deserializes a serialized RIPUpdate message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *RIPUpdate) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		c.Destination[:],
		c.NextHop[:],
		&c.LinkChan,
		&c.Distance,
	)
}

// Encode serializes the target RIPUpdate into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *RIPUpdate) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.Distance[:],
		c.Destination[:],
		c.LinkChan,
		c.Distance,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *RIPUpdate) MsgType() MessageType {
	return MsgRIPUpdate
}

// MaxPayloadLength returns the maximum allowed payload size for an UpdateFulfillHTLC
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *RIPUpdate) MaxPayloadLength(uint32) uint32 {
	// 33 + 33 + 32 + 1
	return 72
}







