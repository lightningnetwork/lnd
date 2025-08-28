package lnwire

import (
	"bytes"
	"io"
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

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field of the init message.
	CustomRecords CustomRecords

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
	var msgExtraData ExtraOpaqueData

	err := ReadElements(r,
		&msg.GlobalFeatures,
		&msg.Features,
		&msgExtraData,
	)
	if err != nil {
		return err
	}

	customRecords, _, extraDData, err := ParseAndExtractCustomRecords(
		msgExtraData,
	)
	if err != nil {
		return err
	}

	msg.CustomRecords = customRecords
	msg.ExtraData = extraDData

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

	extraData, err := MergeAndEncode(nil, msg.ExtraData, msg.CustomRecords)
	if err != nil {
		return err
	}

	return WriteBytes(w, extraData)
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
