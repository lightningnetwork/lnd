package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// RevokeAndAck is sent by either side once a CommitSig message has been
// received, and validated. This message serves to revoke the prior commitment
// transaction, which was the most up to date version until a CommitSig message
// referencing the specified ChannelPoint was received.  Additionally, this
// message also piggyback's the next revocation hash that Alice should use when
// constructing the Bob's version of the next commitment transaction (which
// would be done before sending a CommitSig message).  This piggybacking allows
// Alice to send the next CommitSig message modifying Bob's commitment
// transaction without first asking for a revocation hash initially.
type RevokeAndAck struct {
	// ChanID uniquely identifies to which currently active channel this
	// RevokeAndAck applies to.
	ChanID ChannelID

	// Revocation is the preimage to the revocation hash of the now prior
	// commitment transaction.
	Revocation [32]byte

	// NextRevocationKey is the next commitment point which should be used
	// for the next commitment transaction the remote peer creates for us.
	// This, in conjunction with revocation base point will be used to
	// create the proper revocation key used within the commitment
	// transaction.
	NextRevocationKey *btcec.PublicKey

	// LocalNonce is the next _local_ nonce for the sending party. This
	// allows the receiving party to propose a new commitment using their
	// remote nonce and the sender's local nonce.
	LocalNonce OptMusig2NonceTLV

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewRevokeAndAck creates a new RevokeAndAck message.
func NewRevokeAndAck() *RevokeAndAck {
	return &RevokeAndAck{
		ExtraData: make([]byte, 0),
	}
}

// A compile time check to ensure RevokeAndAck implements the lnwire.Message
// interface.
var _ Message = (*RevokeAndAck)(nil)

// A compile time check to ensure RevokeAndAck implements the
// lnwire.SizeableMessage interface.
var _ SizeableMessage = (*RevokeAndAck)(nil)

// Decode deserializes a serialized RevokeAndAck message stored in the
// passed io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r,
		&c.ChanID,
		c.Revocation[:],
		&c.NextRevocationKey,
	)
	if err != nil {
		return err
	}

	var tlvRecords ExtraOpaqueData
	if err := ReadElements(r, &tlvRecords); err != nil {
		return err
	}

	localNonce := c.LocalNonce.Zero()
	typeMap, err := tlvRecords.ExtractRecords(&localNonce)
	if err != nil {
		return err
	}

	// Set the corresponding TLV types if they were included in the stream.
	if val, ok := typeMap[c.LocalNonce.TlvType()]; ok && val == nil {
		c.LocalNonce = tlv.SomeRecordT(localNonce)
	}

	if len(tlvRecords) != 0 {
		c.ExtraData = tlvRecords
	}

	return nil
}

// Encode serializes the target RevokeAndAck into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) Encode(w *bytes.Buffer, pver uint32) error {
	recordProducers := make([]tlv.RecordProducer, 0, 1)
	c.LocalNonce.WhenSome(func(localNonce Musig2NonceTLV) {
		recordProducers = append(recordProducers, &localNonce)
	})
	err := EncodeMessageExtraData(&c.ExtraData, recordProducers...)
	if err != nil {
		return err
	}

	if err := WriteChannelID(w, c.ChanID); err != nil {
		return err
	}

	if err := WriteBytes(w, c.Revocation[:]); err != nil {
		return err
	}

	if err := WritePublicKey(w, c.NextRevocationKey); err != nil {
		return err
	}

	return WriteBytes(w, c.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *RevokeAndAck) MsgType() MessageType {
	return MsgRevokeAndAck
}

// TargetChanID returns the channel id of the link for which this message is
// intended.
//
// NOTE: Part of peer.LinkUpdater interface.
func (c *RevokeAndAck) TargetChanID() ChannelID {
	return c.ChanID
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (c *RevokeAndAck) SerializedSize() (uint32, error) {
	return MessageSerializedSize(c)
}
