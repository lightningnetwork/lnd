package lnwire

// A compile time check to ensure Warning implements the lnwire.Message
// interface.
var _ Message = (*Warning)(nil)

// Warning is used to express non-critical errors in the protocol, providing
// a "soft" way for nodes to communicate failures. Since it has the same
// structure as errors, warnings simply include an Error so that we can leverage
// their encode/decode functionality, and over write the MsgType function to
// differentiate them.
type Warning struct {
	Error
}

// MsgType returns the integer uniquely identifying an Warning message on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *Warning) MsgType() MessageType {
	return MsgWarning
}
