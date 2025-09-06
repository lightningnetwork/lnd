package lnwire

import (
	"bytes"
	"io"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// Init is the first message reveals the features supported or required by this
// node. Nodes wait for receipt of the other's features to simplify error
// diagnosis where features are incompatible. Each node MUST wait to receive
// init before sending any other messages.
type Init struct {
	// GlobalFeatures is a legacy feature vector used for backwards
	// compatibility with older nodes. Any features defined here should be
	// merged with those presented in Features.
	GlobalFeatures *RawFeatureVector

	// Features is a feature vector containing the features supported by
	// the remote node.
	//
	// NOTE: Older nodes may place some features in GlobalFeatures, but all
	// new features are to be added in Features. When handling an Init
	// message, any GlobalFeatures should be merged into the unified
	// Features field.
	Features *RawFeatureVector

	// AuxFeatures is an optional field that stores auxiliary feature bits
	// for custom channel negotiation. This is used by aux channel
	// implementations to negotiate custom channel behavior.
	AuxFeatures fn.Option[AuxFeatureBits]

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewInitMessage creates new instance of init message object.
func NewInitMessage(gf *RawFeatureVector, f *RawFeatureVector) *Init {
	return &Init{
		GlobalFeatures: gf,
		Features:       f,
		ExtraData:      make([]byte, 0),
	}
}

// A compile time check to ensure Init implements the lnwire.Message
// interface.
var _ Message = (*Init)(nil)

// A compile time check to ensure Init implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*Init)(nil)

// Decode deserializes a serialized Init message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Decode(r io.Reader, pver uint32) error {
	err := ReadElements(r,
		&msg.GlobalFeatures,
		&msg.Features,
		&msg.ExtraData,
	)
	if err != nil {
		return err
	}

	// Extract any TLV records from the extra data.
	var auxFeatures AuxFeatureBits
	auxFeaturesRecord := tlv.MakePrimitiveRecord(
		AuxFeatureBitsTLV, &auxFeatures,
	)

	typeMap, err := msg.ExtraData.ExtractRecords(&auxFeaturesRecord)
	if err != nil {
		return err
	}

	// If the aux features TLV was present, set it in the message.
	if val, ok := typeMap[AuxFeatureBitsTLV]; ok && val == nil {
		msg.AuxFeatures = fn.Some(auxFeatures)
	}

	return nil
}

// Encode serializes the target Init into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteRawFeatureVector(w, msg.GlobalFeatures); err != nil {
		return err
	}

	if err := WriteRawFeatureVector(w, msg.Features); err != nil {
		return err
	}

	// If we have aux features, encode them into the extra data.
	recordProducers := make([]tlv.RecordProducer, 0, 1)
	msg.AuxFeatures.WhenSome(func(features AuxFeatureBits) {
		record := tlv.MakePrimitiveRecord(AuxFeatureBitsTLV, &features)
		recordProducers = append(recordProducers, &record)
	})

	if len(recordProducers) > 0 {
		err := EncodeMessageExtraData(
			&msg.ExtraData, recordProducers...,
		)
		if err != nil {
			return err
		}
	}

	return WriteBytes(w, msg.ExtraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MsgType() MessageType {
	return MsgInit
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (msg *Init) SerializedSize() (uint32, error) {
	return MessageSerializedSize(msg)
}
