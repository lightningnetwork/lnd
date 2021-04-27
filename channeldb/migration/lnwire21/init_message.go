package lnwire

import "io"

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

// Decode deserializes a serialized Init message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Decode(r io.Reader, pver uint32) error {
	return ReadElements(r,
		&msg.GlobalFeatures,
		&msg.Features,
	)
}

// Encode serializes the target Init into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Encode(w io.Writer, pver uint32) error {
	return WriteElements(w,
		msg.GlobalFeatures,
		msg.Features,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MsgType() MessageType {
	return MsgInit
}

// MaxPayloadLength returns the maximum allowed payload size for an Init
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MaxPayloadLength(uint32) uint32 {
	return 2 + 2 + maxAllowedSize + 2 + maxAllowedSize
}
