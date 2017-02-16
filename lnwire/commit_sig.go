package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

// CommitSig is sent by either side to stage any pending HTLC's in the
// receiver's pending set into a new commitment state.  Implicitly, the new
// commitment transaction constructed which has been signed by CommitSig
// includes all HTLC's in the remote node's pending set. A CommitSig message
// may be sent after a series of UpdateAddHTLC/UpdateFufillHTLC messages in
// order to batch add several HTLC's with a single signature covering all
// implicitly accepted HTLC's.
type CommitSig struct {
	// ChannelPoint uniquely identifies to which currently active channel
	// this CommitSig applies to.
	ChannelPoint wire.OutPoint

	// CommitSig is Alice's signature for Bob's new commitment transaction.
	// Alice is able to send this signature without requesting any
	// additional data due to the piggybacking of Bob's next revocation
	// hash in his prior RevokeAndAck message, as well as the canonical
	// ordering used for all inputs/outputs within commitment transactions.
	CommitSig *btcec.Signature

	// TODO(roasbeef): add HTLC sigs after state machine is updated to
	// support that
}

// NewCommitSig creates a new empty CommitSig message.
func NewCommitSig() *CommitSig {
	return &CommitSig{}
}

// A compile time check to ensure CommitSig implements the lnwire.Message
// interface.
var _ Message = (*CommitSig)(nil)

// Decode deserializes a serialized CommitSig message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint(8)
	// RequesterCommitSig(73max+2)
	err := readElements(r,
		&c.ChannelPoint,
		&c.CommitSig,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target CommitSig into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
		c.CommitSig,
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
func (c *CommitSig) Command() uint32 {
	return CmdCommitSig
}

// MaxPayloadLength returns the maximum allowed payload size for a
// CommitSig complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) MaxPayloadLength(uint32) uint32 {
	// 36 + 73
	return 109
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the CommitSig are valid.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Validate() error {
	// We're good!
	return nil
}

// String returns the string representation of the target CommitSig.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) String() string {
	var serializedSig []byte
	if c.CommitSig != nil {
		serializedSig = c.CommitSig.Serialize()
	}

	return fmt.Sprintf("\n--- Begin CommitSig ---\n") +
		fmt.Sprintf("ChannelPoint:\t%v\n", c.ChannelPoint) +
		fmt.Sprintf("CommitSig:\t\t%x\n", serializedSig) +
		fmt.Sprintf("--- End CommitSig ---\n")
}
