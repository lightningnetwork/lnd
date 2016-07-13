package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// CommitSignature is sent by either side to stage any pending HTLC's in the
// reciever's pending set which has not explcitly been rejected via an
// HTLCAddReject message. Implictly, the new commitment transaction constructed
// which has been signed by CommitSig includes all HTLC's in the remote node's
// pending set. A CommitSignature message may be sent after a series of HTLCAdd
// messages in order to batch add several HTLC's with a single signature
// covering all implicitly accepted HTLC's.
type CommitSignature struct {
	// ChannelPoint uniquely identifies to which currently active channel this
	// CommitSignature applies to.
	ChannelPoint *wire.OutPoint

	// LogIndex is the index into the reciever's HTLC log to which this
	// commitment signature covers. In order to properly verify this
	// signature, the receiver should include all the HTLC's within their
	// log with an index less-than-or-equal to the listed log-index.
	LogIndex uint64

	// Fee represents the total miner's fee that was used when constructing
	// the new commitment transaction.
	// TODO(roasbeef): is the above comment correct?
	Fee btcutil.Amount

	// CommitSig is Alice's signature for Bob's new commitment transaction.
	// Alice is able to send this signature without requesting any additional
	// data due to the piggybacking of Bob's next revocation hash in his
	// prior CommitRevocation message, as well as the cannonical ordering
	// used for all inputs/outputs within commitment transactions.
	CommitSig *btcec.Signature
}

// NewCommitSignature creates a new empty CommitSignature message.
func NewCommitSignature() *CommitSignature {
	return &CommitSignature{}
}

// A compile time check to ensure CommitSignature implements the lnwire.Message
// interface.
var _ Message = (*CommitSignature)(nil)

// Decode deserializes a serialized CommitSignature message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitSignature) Decode(r io.Reader, pver uint32) error {
	// ChannelPoint(8)
	// LogIndex(8)
	// Fee(8)
	// RequesterCommitSig(73max+2)
	err := readElements(r,
		&c.ChannelPoint,
		&c.LogIndex,
		&c.Fee,
		&c.CommitSig,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target CommitSignature into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CommitSignature) Encode(w io.Writer, pver uint32) error {
	err := writeElements(w,
		c.ChannelPoint,
		c.LogIndex,
		c.Fee,
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
func (c *CommitSignature) Command() uint32 {
	return CmdCommitSignature
}

// MaxPayloadLength returns the maximum allowed payload size for a
// CommitSignature complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitSignature) MaxPayloadLength(uint32) uint32 {
	// 36 + 8 + 8 + 73
	return 125
}

// Validate performs any necessary sanity checks to ensure all fields present
// on the CommitSignature are valid.
//
// This is part of the lnwire.Message interface.
func (c *CommitSignature) Validate() error {
	if c.Fee < 0 {
		// While fees can be negative, it's too confusing to allow
		// negative payments. Maybe for some wallets, but not this one!
		return fmt.Errorf("Amount paid cannot be negative.")
	}

	// We're good!
	return nil
}

// String returns the string representation of the target CommitSignature.
//
// This is part of the lnwire.Message interface.
func (c *CommitSignature) String() string {
	var serializedSig []byte
	if c.CommitSig != nil {
		serializedSig = c.CommitSig.Serialize()
	}

	return fmt.Sprintf("\n--- Begin CommitSignature ---\n") +
		fmt.Sprintf("ChannelPoint:\t%v\n", c.ChannelPoint) +
		fmt.Sprintf("LogIndex:\t\t%v\n", c.LogIndex) +
		fmt.Sprintf("Fee:\t\t\t%s\n", c.Fee.String()) +
		fmt.Sprintf("CommitSig:\t\t%x\n", serializedSig) +
		fmt.Sprintf("--- End CommitSignature ---\n")
}
