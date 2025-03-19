package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// CommitSig is sent by either side to stage any pending HTLC's in the
// receiver's pending set into a new commitment state. Implicitly, the new
// commitment transaction constructed which has been signed by CommitSig
// includes all HTLC's in the remote node's pending set. A CommitSig message
// may be sent after a series of UpdateAddHTLC/UpdateFulfillHTLC messages in
// order to batch add several HTLC's with a single signature covering all
// implicitly accepted HTLC's.
type CommitSig struct {
	// ChanID uniquely identifies to which currently active channel this
	// CommitSig applies to.
	ChanID ChannelID

	// CommitSig is Alice's signature for Bob's new commitment transaction.
	// Alice is able to send this signature without requesting any
	// additional data due to the piggybacking of Bob's next revocation
	// hash in his prior RevokeAndAck message, as well as the canonical
	// ordering used for all inputs/outputs within commitment transactions.
	// If initiating a new commitment state, this signature should ONLY
	// cover all of the sending party's pending log updates, and the log
	// updates of the remote party that have been ACK'd.
	CommitSig Sig

	// HtlcSigs is a signature for each relevant HTLC output within the
	// created commitment. The order of the signatures is expected to be
	// identical to the placement of the HTLC's within the BIP 69 sorted
	// commitment transaction. For each outgoing HTLC (from the PoV of the
	// sender of this message), a signature for an HTLC timeout transaction
	// should be signed, for each incoming HTLC the HTLC timeout
	// transaction should be signed.
	HtlcSigs []Sig

	// PartialSig is used to transmit a musig2 extended partial signature
	// that also carries along the public nonce of the signer.
	//
	// NOTE: This field is only populated if a musig2 taproot channel is
	// being signed for. In this case, the above Sig type MUST be blank.
	PartialSig OptPartialSigWithNonceTLV

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field.
	CustomRecords CustomRecords

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewCommitSig creates a new empty CommitSig message.
func NewCommitSig() *CommitSig {
	return &CommitSig{}
}

// A compile time check to ensure CommitSig implements the lnwire.Message
// interface.
var _ Message = (*CommitSig)(nil)

// A compile time check to ensure CommitSig implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*CommitSig)(nil)

// Decode deserializes a serialized CommitSig message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Decode(r io.Reader, pver uint32) error {
	// msgExtraData is a temporary variable used to read the message extra
	// data field from the reader.
	var msgExtraData ExtraOpaqueData

	err := ReadElements(r,
		&c.ChanID,
		&c.CommitSig,
		&c.HtlcSigs,
		&msgExtraData,
	)
	if err != nil {
		return err
	}

	// Extract TLV records from the extra data field.
	partialSig := c.PartialSig.Zero()

	customRecords, parsed, extraData, err := ParseAndExtractCustomRecords(
		msgExtraData, &partialSig,
	)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if _, ok := parsed[partialSig.TlvType()]; ok {
		c.PartialSig = tlv.SomeRecordT(partialSig)
	}

	c.CustomRecords = customRecords
	c.ExtraData = extraData

	return nil
}

// Encode serializes the target CommitSig into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := make([]tlv.RecordProducer, 0, 1)
	c.PartialSig.WhenSome(func(sig PartialSigWithNonceTLV) {
		recordProducers = append(recordProducers, &sig)
	})

	extraData, err := MergeAndEncode(
		recordProducers, c.ExtraData, c.CustomRecords,
	)
	if err != nil {
		return err
	}

	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, c.CommitSig); err != nil {
		return err
	}

	if err := WriteSigs(w, c.HtlcSigs); err != nil {
		return err
	}

	return WriteBytes(w, extraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) MsgType() MessageType {
	return MsgCommitSig
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *CommitSig) TargetChanID() ChannelID {
	return c.ChanID
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *CommitSig) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}
