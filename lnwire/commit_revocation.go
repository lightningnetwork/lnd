package lnwire

import (
	"fmt"
	"io"
)

// CommitRevocation is sent by either side once a CommitSignature message has
// been received, and validated. This message serves to revoke the prior
// commitment transaction, which was the most up to date version until a
// CommitSignature message referencing the specified ChannelID was received.
// Additionally, this message also piggyback's the next revocation hash that
// Alice should use when constructing the Bob's version of the next commitment
// transaction (which would be done before sending a CommitSignature message).
// This piggybacking allows Alice to send the next CommitSignature message
// modifying Bob's commitment transaction without first asking for a revocation
// hash initially.
type CommitRevocation struct {
	// ChannelID uniquely identifies to which currently active channel this
	// CommitRevocation applies to.
	ChannelID uint64

	// Revocation is the pre-image to the revocation hash of the now prior
	// commitment transaction.
	Revocation [20]byte

	// NextRevocationHash is the next revocation hash to use to create the
	// next commitment transaction which is to be created upon receipt of a
	// CommitSignature message.
	NextRevocationHash [20]byte
}

// Decode deserializes a serialized CommitRevocation message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) Decode(r io.Reader, pver uint32) error {
	// NextRevocationHash (20)
	// Revocation (20)
	err := readElements(r,
		&c.NextRevocationHash,
		&c.Revocation,
	)
	if err != nil {
		return err
	}

	return nil
}

// NewCommitRevocation creates a new CommitRevocation message.
func NewCommitRevocation() *CommitRevocation {
	return &CommitRevocation{}
}

// A compile time check to ensure CommitRevocation implements the lnwire.Message
// interface.
var _ Message = (*CommitRevocation)(nil)

// Encode serializes the target CommitRevocation into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.NextRevocationHash,
		c.Revocation,
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
func (c *CommitRevocation) Command() uint32 {
	return CmdCommitRevocation
}

// MaxPayloadLength returns the maximum allowed payload size for a
// CommitRevocation complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) MaxPayloadLength(uint32) uint32 {
	// 8 + 20 + 20
	return 48
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the CommitRevocation are valid.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target CommitRevocation.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) String() string {
	return fmt.Sprintf("\n--- Begin CommitRevocation ---\n") +
		fmt.Sprintf("ChannelID:\t%d\n", c.ChannelID) +
		fmt.Sprintf("NextRevocationHash:\t%x\n", c.NextRevocationHash) +
		fmt.Sprintf("Revocation:\t%x\n", c.Revocation) +
		fmt.Sprintf("--- End CommitRevocation ---\n")
}
