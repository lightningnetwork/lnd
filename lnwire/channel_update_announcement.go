package lnwire

import (
	"bytes"
	"errors"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// ChannelUpdateAnnouncement message is used after channel has been initially
// announced. Each side independently announces its fees and minimum expiry for
// HTLCs and other parameters. Also this message is used to redeclare initially
// setted channel parameters.
type ChannelUpdateAnnouncement struct {
	// Signature is used to validate the announced data and prove the
	// ownership of node id.
	Signature *btcec.Signature

	// ChannelID is the unique description of the funding transaction.
	ChannelID ChannelID

	// Timestamp allows ordering in the case of multiple announcements.
	// We should ignore the message if timestamp is not greater than
	// the last-received.
	Timestamp uint32

	// Flags least-significant bit must be set to 0 if the creating node
	// corresponds to the first node in the previously sent channel
	// announcement and 1 otherwise.
	Flags uint16

	// TimeLockDelta is the minimum number of blocks this node requires to
	// be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node.
	TimeLockDelta uint16

	// HtlcMinimumMsat is the minimum HTLC value which will be accepted.
	HtlcMinimumMsat uint32

	// FeeBaseMstat...
	FeeBaseMsat uint32

	// FeeProportionalMillionths...
	FeeProportionalMillionths uint32
}

// A compile time check to ensure ChannelUpdateAnnouncement implements the
// lnwire.Message interface.
var _ Message = (*ChannelUpdateAnnouncement)(nil)

// Validate performs any necessary sanity checks to ensure all fields present
// on the ChannelUpdateAnnouncement are valid.
//
// This is part of the lnwire.Message interface.
func (a *ChannelUpdateAnnouncement) Validate() error {
	// NOTE: As far as we don't have the node id (public key) in this
	// message, we can't validate the signature on this stage, it should
	// be validated latter - in discovery service handler.

	if a.TimeLockDelta == 0 {
		return errors.New("expiry should be greater then zero")
	}

	return nil
}

// Decode deserializes a serialized ChannelUpdateAnnouncement stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdateAnnouncement) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.Signature,
		&c.ChannelID,
		&c.Timestamp,
		&c.Flags,
		&c.TimeLockDelta,
		&c.HtlcMinimumMsat,
		&c.FeeBaseMsat,
		&c.FeeProportionalMillionths,
	)
}

// Encode serializes the target ChannelUpdateAnnouncement into the passed
// io.Writer observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdateAnnouncement) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.Signature,
		c.ChannelID,
		c.Timestamp,
		c.Flags,
		c.TimeLockDelta,
		c.HtlcMinimumMsat,
		c.FeeBaseMsat,
		c.FeeProportionalMillionths,
	)
}

// Command returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdateAnnouncement) Command() uint32 {
	return CmdChannelUpdateAnnoucmentMessage
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *ChannelUpdateAnnouncement) MaxPayloadLength(pver uint32) uint32 {
	var length uint32

	// Signature - 64 bytes
	length += 64

	// ChannelID - 8 bytes
	length += 8

	// Timestamp - 4 bytes
	length += 4

	// Flags - 2 bytes
	length += 2

	// Expiry - 2 bytes
	length += 2

	// HtlcMinimumMstat - 4 bytes
	length += 4

	// FeeBaseMstat - 4 bytes
	length += 4

	// FeeProportionalMillionths - 4 bytes
	length += 4

	return length
}

// DataToSign is used to retrieve part of the announcement message which
// should be signed.
func (c *ChannelUpdateAnnouncement) DataToSign() ([]byte, error) {

	// We should not include the signatures itself.
	var w bytes.Buffer
	err := writeElements(&w,
		c.ChannelID,
		c.Timestamp,
		c.Flags,
		c.TimeLockDelta,
		c.HtlcMinimumMsat,
		c.FeeBaseMsat,
		c.FeeProportionalMillionths,
	)
	if err != nil {
		return nil, err
	}

	return w.Bytes(), nil
}
