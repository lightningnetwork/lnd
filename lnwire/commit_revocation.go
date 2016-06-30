package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

// CommitRevocation is sent by either side once a CommitSignature message has
// been received, and validated. This message serves to revoke the prior
// commitment transaction, which was the most up to date version until a
// CommitSignature message referencing the specified ChannelPoint was received.
// Additionally, this message also piggyback's the next revocation hash that
// Alice should use when constructing the Bob's version of the next commitment
// transaction (which would be done before sending a CommitSignature message).
// This piggybacking allows Alice to send the next CommitSignature message
// modifying Bob's commitment transaction without first asking for a revocation
// hash initially.
// TODO(roasbeef): update docs everywhere
type CommitRevocation struct {
	// ChannelPoint uniquely identifies to which currently active channel this
	// CommitRevocation applies to.
	ChannelPoint *wire.OutPoint

	// Revocation is the pre-image to the revocation hash of the now prior
	// commitment transaction.
	//
	// If the received revocation is the all zeroes hash ('0' * 32), then
	// this CommitRevocation is being sent in order to build up the
	// sender's initial revocation window (IRW). In this case, the
	// CommitRevocation should be added to the receiver's queue of unused
	// revocations to be used to construct future commitment transactions.
	Revocation [32]byte

	// NextRevocationKey is the next revocation key which should be added
	// to the queue of unused revocation keys for the remote peer. This key
	// will be used within the revocation clause for any new commitment
	// transactions created for the remote peer.
	NextRevocationKey *btcec.PublicKey

	// NextRevocationHash is the next revocation hash which should be added
	// to the queue on unused revocation hashes for the remote peer. This
	// revocation hash will be used within any HTLC's included within this
	// next commitment transaction.
	NextRevocationHash [32]byte
}

// NewCommitRevocation creates a new CommitRevocation message.
func NewCommitRevocation() *CommitRevocation {
	return &CommitRevocation{}
}

// A compile time check to ensure CommitRevocation implements the lnwire.Message
// interface.
var _ Message = (*CommitRevocation)(nil)

// Decode deserializes a serialized CommitRevocation message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint (8)
	// Revocation (32)
	// NextRevocationKey (33)
	// NextRevocationHash (32)
	err := readElements(r,
		&c.ChannelPoint,
		&c.Revocation,
		&c.NextRevocationKey,
		&c.NextRevocationHash,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target CommitRevocation into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CommitRevocation) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
		c.Revocation,
		c.NextRevocationKey,
		c.NextRevocationHash,
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
	// 36 + 32 + 33 + 32
	return 133
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
// TODO(roasbeef): remote all String() methods...spew should be used instead.
func (c *CommitRevocation) String() string {
	return fmt.Sprintf("\n--- Begin CommitRevocation ---\n") +
		fmt.Sprintf("ChannelPoint:\t%d\n", c.ChannelPoint) +
		fmt.Sprintf("NextRevocationHash:\t%x\n", c.NextRevocationHash) +
		fmt.Sprintf("Revocation:\t%x\n", c.Revocation) +
		fmt.Sprintf("--- End CommitRevocation ---\n")
}
