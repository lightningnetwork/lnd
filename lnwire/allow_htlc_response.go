package lnwire

import (
	"fmt"
	"io"
)

type AllowHTLCStatus uint8

const (
	AllowHTLCStatus_Allow AllowHTLCStatus = 1
	AllowHTLCStatus_Decline AllowHTLCStatus = 2
	AllowHTLCStatus_Timeout AllowHTLCStatus = 3
)

func (st AllowHTLCStatus) String() string {
	statusToString := map[AllowHTLCStatus]string{
		AllowHTLCStatus_Allow: "<Allow>",
		AllowHTLCStatus_Decline: "<Decline>",
		AllowHTLCStatus_Timeout: "<Timeout>",
	}
	s, ok := statusToString[st]
	if ok{
		return s
	} else {
		return fmt.Sprintf("<Unknown status: %d>", st)
	}
}

// AllowHTLCResponse is sent from Bob to Alice. It informs Alice if Bob is able to
// make htlc chain with some other node with a given amount.
// It is used to validate/filter possible paths in routing.
type AllowHTLCResponseMessage struct {
	// Number uniquely identifying this response.
	// It should be equal to RequstID of AllowHTLCRequest
	RequestID uint64
	// Status
	Status AllowHTLCStatus
}

// Decode deserializes a serialized AllowHTLCResponse message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCResponseMessage) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint (8)
	// HTLCKey   (8)
	var status uint8
	err := readElements(r,
		&c.RequestID,
		&status,
	)
	c.Status = AllowHTLCStatus(status)
	if err != nil {
		return err
	}

	return nil
}

// AllowHTLCResponse returns a new empty AllowHTLCResponse message.
func NewAllowHTLCResponse() *AllowHTLCResponseMessage {
	return &AllowHTLCResponseMessage{}
}

// Encode serializes the target AllowHTLCResponse into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCResponseMessage) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.RequestID,
		uint8(c.Status),
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
func (c *AllowHTLCResponseMessage) Command() uint32 {
	return CmdAllowHTLCResponse
}

// MaxPayloadLength returns the maximum allowed payload size for a AllowHTLCResponse
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCResponseMessage) MaxPayloadLength(uint32) uint32 {
	// 8 + 1
	return 9
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the AllowHTLCResponse are valid.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCResponseMessage) Validate() error {
	switch c.Status {
	case AllowHTLCStatus_Allow,
		AllowHTLCStatus_Decline,
		AllowHTLCStatus_Timeout:
		break
	default:
		return fmt.Errorf("Incorrect status %v", c.Status)
	}
	return nil
}

// String returns the string representation of the target AllowHTLCRequest.
//
// This is part of the lnwire.Message interface.
func (c *AllowHTLCResponseMessage) String() string {
	return fmt.Sprintf("\n--- Begin AllowHTLCResponse ---\n") +
		fmt.Sprintf("RequestID:\t\t%d\n", c.RequestID) +
		fmt.Sprintf("Status:\t\t%v\n", c.Status) +
		fmt.Sprintf("--- End AllowHTLCResponse ---\n")
}

// A compile time check to ensure AllowHTLCResponse implements the lnwire.Message
// interface.
var _ Message = (*AllowHTLCResponseMessage)(nil)