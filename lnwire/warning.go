package lnwire

import (
	"bytes"
	"fmt"
	"io"
)

// WarningData is a set of bytes associated with a particular sent warning. A
// receiving node SHOULD only print out data verbatim if the string is composed
// solely of printable ASCII characters. For reference, the printable character
// set includes byte values 32 through 127 inclusive.
type WarningData []byte

// Warning is used to express non-critical errors in the protocol, providing
// a "soft" way for nodes to communicate failures.
type Warning struct {
	// ChanID references the active channel in which the warning occurred
	// within. If the ChanID is all zeros, then this warning applies to the
	// entire established connection.
	ChanID ChannelID

	// Data is the attached warning data that describes the exact failure
	// which caused the warning message to be sent.
	Data WarningData
}

// A compile time check to ensure Warning implements the lnwire.Message
// interface.
var _ Message = (*Warning)(nil)

// A compile time check to ensure Warning implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*Warning)(nil)

// NewWarning creates a new Warning message.
func NewWarning() *Warning {
	return &Warning{}
}

// Warning returns the string representation to Warning.
func (c *Warning) Warning() string {
	errMsg := "non-ascii data"
	if isASCII(c.Data) {
		errMsg = string(c.Data)
	}

	return fmt.Sprintf("chan_id=%v, err=%v", c.ChanID, errMsg)
}

// Decode deserializes a serialized Warning message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *Warning) Decode(r io.Reader, _ uint32) error {
	return ReadElements(r,
		&c.ChanID,
		&c.Data,
	)
}

// Encode serializes the target Warning into the passed io.Writer observing the
// protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *Warning) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteBytes(w, c.ChanID[:]); err != nil {
		return err
	}

	return WriteWarningData(w, c.Data)
}

// MsgType returns the integer uniquely identifying an Warning message on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *Warning) MsgType() MessageType {
	return MsgWarning
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *Warning) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}
