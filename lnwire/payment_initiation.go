package lnwire

import (
	"fmt"
	"io"
)

// PaymentInitiation is the message sent by Alice to Bob when she wishes to make payment.
type PaymentInitiation struct {
	// Amount is the number of credits in payment.
	Amount CreditsAmount

	// Some number to identify payment
	InvoiceNumber [32]byte

	Memo []byte
}

// NewPaymentInitiation  returns a new empty PaymentInitiation  message.
func NewPaymentInitiation() *PaymentInitiation {
	return &PaymentInitiation{}
}

// A compile time check to ensure HTLCAddRequest implements the lnwire.Message
// interface.
var _ Message = (*PaymentInitiation)(nil)

// Decode deserializes a serialized PaymentInitiation  message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiation) Decode(r io.Reader, pver uint32) error {

	err := readElements(r,
		&c.Amount,
		&c.InvoiceNumber,
		&c.Memo,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target PaymentInitiation into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiation) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.Amount,
		c.InvoiceNumber,
		c.Memo,
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
func (c *PaymentInitiation) Command() uint32 {
	return CmdPaymentInitiation
}

// MaxPayloadLength returns the maximum allowed payload size for a PaymentInitiation
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiation) MaxPayloadLength(uint32) uint32 {
	// base size ~110, but blob can be variable.
	// shouldn't be bigger than 8K though...
	return 8192
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the PaymentInitiation are valid.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiation) Validate() error {
	if c.Amount < 0 {
		// While fees can be negative, it's too confusing to allow
		// negative payments. Maybe for some wallets, but not this one!
		return fmt.Errorf("Amount paid cannot be negative.")
	}
	// We're good!
	return nil
}

// String returns the string representation of the target HTLCAddRequest.
//
// This is part of the lnwire.Message interface.
func (c *PaymentInitiation) String() string {
	return fmt.Sprintf("\n--- Begin PaymentInitiation ---\n") +
		fmt.Sprintf("Amount\t\t%d\n", c.Amount) +
		fmt.Sprintf("InvoiceNumber:\t(%v)\n", c.InvoiceNumber) +
		fmt.Sprintf("Memo:\t%v\n", c.Memo) +
		fmt.Sprintf("--- End PaymentInitiation ---\n")
}