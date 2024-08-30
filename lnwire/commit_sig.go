package lnwire

import (
	"bytes"
	"fmt"
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
	// intended for inclusion in the ExtraData field of the CommitSig
	// message.
	CustomRecords CustomRecords

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewCommitSig creates a new empty CommitSig message.
func NewCommitSig() *CommitSig {
	return &CommitSig{
		ExtraData: make([]byte, 0),
	}
}

// A compile time check to ensure CommitSig implements the lnwire.Message
// interface.
var _ Message = (*CommitSig)(nil)

// Decode deserializes a serialized CommitSig message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r,
		&c.ChanID,
		&c.CommitSig,
		&c.HtlcSigs,
	)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	partialSig := c.PartialSig.Zero()
	typeMap, err := tlvRecords.ExtractRecords(&partialSig)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[c.PartialSig.TlvType()]; ok && val == nil {
		c.PartialSig = tlv.SomeRecordT(partialSig)

		// Remove the entry from the TLV map. Anything left in the map
		// will be included in the custom records field.
		delete(typeMap, c.PartialSig.TlvType())
	}

	// Parse through the remaining extra data map to separate the custom
	// records, from the set of official records.
	tlvTypes := newWireTlvMap(typeMap)

	// Set the custom records field to the custom records specific TLV
	// record map.
	customRecords, err := NewCustomRecordsFromTlvTypeMap(
		tlvTypes.customTypes,
	)
	if err != nil {
		return err
	}
	c.CustomRecords = customRecords

	// Set custom records to nil if we didn't parse anything out of it so
	// that we can use assert.Equal in tests.
	if len(customRecords) == 0 {
		c.CustomRecords = nil
	}

	// Set extra data to nil if we didn't parse anything out of it so that
	// we can use assert.Equal in tests.
	if len(tlvTypes.officialTypes) == 0 {
		c.ExtraData = nil
		return nil
	}

	// Encode the remaining records back into the extra data field. These
	// records are not in the custom records TLV type range and do not have
	// associated fields in the CommitSig struct.
	c.ExtraData, err = NewExtraOpaqueDataFromTlvTypeMap(
		tlvTypes.officialTypes,
	)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target CommitSig into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *CommitSig) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteSig(w, c.CommitSig); err != nil {
		return err
	}

	if err := WriteSigs(w, c.HtlcSigs); err != nil {
		return err
	}

	// Construct a slice of all the records that we should include in the
	// message extra data field. We will start by including any records
	// from the extra data field.
	msgExtraDataRecords, err := c.ExtraData.RecordProducers()
	if err != nil {
		return err
	}

	// Include the partial sig record if it is set.
	c.PartialSig.WhenSome(func(sig PartialSigWithNonceTLV) {
		msgExtraDataRecords = append(msgExtraDataRecords, &sig)
	})

	// Include custom records in the extra data wire field if they are
	// present. Ensure that the custom records are validated before
	// encoding them.
	if err := c.CustomRecords.Validate(); err != nil {
		return fmt.Errorf("custom records validation error: %w", err)
	}

	// Extend the message extra data records slice with TLV records from
	// the custom records field.
	customTlvRecords := c.CustomRecords.RecordProducers()
	msgExtraDataRecords = append(msgExtraDataRecords, customTlvRecords...)

	// We will now construct the message extra data field that will be
	// encoded into the byte writer.
	var msgExtraData ExtraOpaqueData
	if err := msgExtraData.PackRecords(msgExtraDataRecords...); err != nil {
		return err
	}

	return WriteBytes(w, msgExtraData)
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
