package lnwire

import (
	"fmt"
	"io"
)

// PaymentInitiation is the message sent by Alice to Bob when she wishes to make payment.
type PaymentInitiationConfirmation struct {
	// Some number to identify payment
	InvoiceNumber [32]byte

	// RedemptionHash is the hash to be used within the HTLC script.
	// An HTLC is only fufilled once Bob is provided with the required pre-image
	// for the listed hash.
	RedemptionHash [32]byte
}

// NewPaymentInitiation  returns a new empty PaymentInitiationConfirmation  message.
func NewPaymentInitiationConfirmation() *PaymentInitiationConfirmation {
	return &PaymentInitiationConfirmation{}
}

// A compile time check to ensure PaymentInitiationConfirmation implements the lnwire.Message
// interface.
var _ Message = (*PaymentInitiationConfirmation)(nil)

// Decode deserializes a serialized PaymentInitiationConfirmation  message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiationConfirmation) Decode(r io.Reader, pver uint32) error {

	err := readElements(r,
		&c.InvoiceNumber,
		&c.RedemptionHash,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target PaymentInitiationConfirmation into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiationConfirmation) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.InvoiceNumber,
		c.RedemptionHash,
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
func (c *PaymentInitiationConfirmation) Command() uint32 {
	return CmdPaymentInitiationConfirmation
}

// MaxPayloadLength returns the maximum allowed payload size for a PaymentInitiation
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiationConfirmation) MaxPayloadLength(uint32) uint32 {
	// base size ~110, but blob can be variable.
	// shouldn't be bigger than 8K though...
	return 8192
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the PaymentInitiation are valid.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiationConfirmation) Validate() error {
	return nil
}

// String returns the string representation of the target HTLCAddRequest.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiationConfirmation) String() string {
	return fmt.Sprintf("\n--- Begin PaymentInitiationConfirmation ---\n") +
		fmt.Sprintf("InvoiceNumber:\t(%v)\n", c.InvoiceNumber) +
		fmt.Sprintf("RedemptionHash:\t%v\n", c.RedemptionHash) +
		fmt.Sprintf("--- End PaymentInitiationConfirmation ---\n")
}